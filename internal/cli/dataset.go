package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/ultramanDecker/bili-comment/internal/model"
	"github.com/ultramanDecker/bili-comment/internal/output"
)

// 目录模式下跟着主数据一起产出的几份文件。
//
// 它们的共同目的是「让 LLM 少读一点」：一万条评论约五十万 token，塞不进
// 任何上下文，而很多时候看完统计就能回答问题，根本不必读全量。
// 这不是优化，是可用性问题——读不完的数据集等于没有数据集。

// writeDatasetExtras 生成 manifest.json / stats.json / schema.md。
func writeDatasetExtras(s *sink, video *model.Video, mode string) error {
	jsonlPath := ""
	for _, w := range s.writers {
		if w.name == "jsonl" {
			jsonlPath = w.path
		}
	}
	if jsonlPath == "" {
		return nil
	}

	scan, err := scanDataset(jsonlPath)
	if err != nil {
		return err
	}

	meta := &output.Meta{
		Video: videoRow(video, mode),
		Mode:  mode,
		Full:  s.full,
	}

	files := []struct {
		name string
		body func() ([]byte, error)
	}{
		{"stats.json", func() ([]byte, error) { return jsonBytes(scan.stats(meta)) }},
		{"manifest.json", func() ([]byte, error) { return jsonBytes(scan.manifest(meta, s)) }},
		{"schema.md", func() ([]byte, error) {
			return []byte(output.SchemaMarkdown(meta, scan.summary)), nil
		}},
	}
	for _, f := range files {
		b, err := f.body()
		if err != nil {
			return fmt.Errorf("生成 %s 失败：%w", f.name, err)
		}
		// 0600 里没有敏感信息，用常规权限即可。
		if err := os.WriteFile(filepath.Join(s.dir, f.name), b, 0o644); err != nil {
			return fmt.Errorf("写入 %s 失败：%w", f.name, err)
		}
	}
	return nil
}

func jsonBytes(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// datasetScan 是从主输出里读回来的东西。
//
// 统计刻意从**文件**而不是内存里的计数器算：内存里的计数器只说本次运行，
// 而文件可能来自上次 + 本次两段。文件是唯一的事实来源。
type datasetScan struct {
	video   *output.Video
	summary *output.Summary

	comments int
	replies  int
	stubs    int
	stubSum  int

	users     map[int64]struct{}
	locations map[string]int
	months    map[string]int
	likes     likeHist
	flags     map[string]int
	ngrams    map[string]int
	tokens    map[string]int
	repeats   map[string]int
	firstC    time.Time
	lastC     time.Time
}

// 统计表的上限。
//
// 抓一个热门视频可能有几十万条评论，每条产生十几个二元组，不设上限的话
// 光统计就能吃掉几百兆内存。到顶之后停止收录新词条——已有词条的计数
// 仍然准确，只是低频长尾少了一些。对一个「先看看规模」用的文件来说，
// 长尾里的单次出现本来就进不了 Top100。
const (
	maxNGrams = 400_000
	maxTokens = 100_000
	maxRepeat = 5_000
)

func scanDataset(path string) (*datasetScan, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败：%w", path, err)
	}
	defer fh.Close()

	sc := &datasetScan{
		users:     map[int64]struct{}{},
		locations: map[string]int{},
		months:    map[string]int{},
		flags:     map[string]int{},
		ngrams:    map[string]int{},
		tokens:    map[string]int{},
		repeats:   map[string]int{},
	}

	r := bufio.NewScanner(fh)
	r.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for r.Scan() {
		line := r.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			continue
		}
		switch probe.Type {
		case "video":
			var v output.Video
			if json.Unmarshal(line, &v) == nil {
				sc.video = &v
			}
		case "summary":
			var s output.Summary
			if json.Unmarshal(line, &s) == nil {
				sc.summary = &s
			}
		case "reply_stub":
			var st output.Stub
			if json.Unmarshal(line, &st) == nil {
				sc.stubs++
				sc.stubSum += st.ReplyCount
			}
		default:
			var cm model.Comment
			if err := json.Unmarshal(line, &cm); err != nil {
				continue
			}
			sc.add(&cm)
		}
	}
	if err := r.Err(); err != nil {
		return nil, fmt.Errorf("读取 %s 失败：%w", path, err)
	}
	return sc, nil
}

func (s *datasetScan) add(cm *model.Comment) {
	if cm.Root == 0 {
		s.comments++
	} else {
		s.replies++
	}
	if cm.User.Mid != 0 {
		s.users[cm.User.Mid] = struct{}{}
	}
	if cm.Location != "" {
		s.locations[cm.Location]++
	}
	for _, f := range cm.Flags {
		s.flags[f]++
	}
	s.likes.add(cm.Like)

	if !cm.Ctime.IsZero() {
		t := cm.Ctime.Time()
		s.months[t.Format("2006-01")]++
		if s.firstC.IsZero() || t.Before(s.firstC) {
			s.firstC = t
		}
		if t.After(s.lastC) {
			s.lastC = t
		}
	}

	// 只有被判为复读的评论才需要留原文用于「重复榜」。留全部评论的原文
	// 就是把整个数据集在内存里复制一份，而重复榜只需要那几十条。
	if _, isRepeat := s.flagsOf(cm, "repeat"); isRepeat && len(s.repeats) < maxRepeat {
		s.repeats[cm.Message]++
	}
	s.countText(cm.Message)
}

func (s *datasetScan) flagsOf(cm *model.Comment, want string) (struct{}, bool) {
	for _, f := range cm.Flags {
		if f == want {
			return struct{}{}, true
		}
	}
	return struct{}{}, false
}

// countText 统计文本特征。
//
// 中文没有空格，不引入分词词典就取不到「词」。所以这里分两类统计，
// 而且**分别命名**：`tokens` 是真正的词（@某人、#话题#、英文单词），
// `ngrams` 是字符二元组。把二元组也叫「高频词」是不诚实的——
// 下游会以为拿到的是词，然后奇怪为什么榜上有「个转」这种东西。
func (s *datasetScan) countText(msg string) {
	if msg == "" {
		return
	}
	runes := []rune(msg)

	// 汉字与假名的连续段落 → 字符二元组。
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		seg := runes[start:end]
		start = -1
		if len(seg) < 2 {
			return
		}
		if len(s.ngrams) >= maxNGrams {
			return
		}
		// 超长段落封顶：评论区小作文可能上千字，全量出二元组会让
		// 统计被少数几条长评论主导。
		if len(seg) > 60 {
			seg = seg[:60]
		}
		for i := 0; i+2 <= len(seg); i++ {
			s.ngrams[string(seg[i:i+2])]++
		}
	}

	for i, r := range runes {
		switch {
		case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r):
			if start < 0 {
				start = i
			}
		case r == '@' || r == '#':
			// 提及与话题：从标记符读到下一个分隔符。
			flush(i)
			j := i + 1
			for j < len(runes) && !isSep(runes[j]) {
				j++
			}
			if j > i+1 && len(s.tokens) < maxTokens {
				tok := string(runes[i:j])
				// #话题# 结尾的那个 # 是闭合符，不属于名字。
				tok = strings.TrimSuffix(tok, "#")
				if len([]rune(tok)) > 1 {
					s.tokens[tok]++
				}
			}
		case isASCIIWord(r):
			flush(i)
			j := i
			for j < len(runes) && isASCIIWord(runes[j]) {
				j++
			}
			if j-i >= 3 && len(s.tokens) < maxTokens {
				s.tokens[strings.ToLower(string(runes[i:j]))]++
			}
		default:
			flush(i)
		}
	}
	flush(len(runes))
}

func isSep(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.Is(unicode.Han, r)
}

func isASCIIWord(r rune) bool {
	return r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r))
}

// likeHist 是点赞数的分段计数。
//
// 用分段而不是完整直方图：点赞数是长尾分布，逐值统计会得到一张
// 绝大部分格子是 0 的表，既占地方又看不出东西。
type likeHist struct {
	Zero   int `json:"0"`
	One9   int `json:"1-9"`
	Ten99  int `json:"10-99"`
	Hun999 int `json:"100-999"`
	Kplus  int `json:"1000+"`
}

func (h *likeHist) add(n int) {
	switch {
	case n <= 0:
		h.Zero++
	case n < 10:
		h.One9++
	case n < 100:
		h.Ten99++
	case n < 1000:
		h.Hun999++
	default:
		h.Kplus++
	}
}

// ---- stats.json ----

// counted 是「某个东西出现了几次」。
//
// 用统一的 {value, count} 而不是给每个榜起一套字段名：字段名在
// 外层的键里已经说了（top_tokens、locations……），再重复一遍只会
// 让下游为每个榜写一份不同的解析代码。
type counted struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

type statsFile struct {
	GeneratedAt  string          `json:"generated_at"`
	Video        *output.Video   `json:"video"`
	Completeness *output.Summary `json:"completeness"`
	Totals       statsTotals     `json:"totals"`
	Flags        []counted       `json:"flags,omitempty"`

	// TopTokens 是真正的词：@提及、#话题#、英文单词。
	TopTokens []counted `json:"top_tokens,omitempty"`

	// TopNgrams 是汉字连续段的字符二元组。
	//
	// 刻意不叫「高频词」：没有分词词典就取不到词，二元组里会混进
	// 「个转」这种跨词边界的碎片。叫什么名字决定了下游怎么用它，
	// 把二元组说成词是不诚实的。
	TopNgrams []counted `json:"top_ngrams,omitempty"`

	Locations     []counted      `json:"locations,omitempty"`
	TimeBuckets   []counted      `json:"time_buckets,omitempty"`
	LikeHistogram map[string]int `json:"like_histogram"`
	TopRepeated   []repeatItem   `json:"top_repeated,omitempty"`
}

type statsTotals struct {
	RootComments int    `json:"root_comments"`
	SubReplies   int    `json:"sub_replies"`
	Users        int    `json:"distinct_users"`
	Stubs        int    `json:"reply_stubs"`
	StubReplies  int    `json:"stub_reply_count_sum"`
	FirstComment string `json:"first_comment_at,omitempty"`
	LastComment  string `json:"last_comment_at,omitempty"`
}

type repeatItem struct {
	Count   int    `json:"count"`
	Message string `json:"message"`
}

func (s *datasetScan) stats(meta *output.Meta) *statsFile {
	f := &statsFile{
		GeneratedAt:  time.Now().Format(time.RFC3339),
		Video:        meta.Video,
		Completeness: s.summary,
		Totals: statsTotals{
			RootComments: s.comments,
			SubReplies:   s.replies,
			Users:        len(s.users),
			Stubs:        s.stubs,
			StubReplies:  s.stubSum,
		},
		Flags: topN(s.flags, 0, 100),
		LikeHistogram: map[string]int{
			"0": s.likes.Zero, "1-9": s.likes.One9, "10-99": s.likes.Ten99,
			"100-999": s.likes.Hun999, "1000+": s.likes.Kplus,
		},
	}
	if !s.firstC.IsZero() {
		f.Totals.FirstComment = s.firstC.Format(time.RFC3339)
		f.Totals.LastComment = s.lastC.Format(time.RFC3339)
	}

	// 只出现一次的条目进不了榜：一次出现说明不了「高频」，
	// 而它占了长尾里的绝大多数条目。
	f.TopTokens = topN(s.tokens, 2, 100)
	f.TopNgrams = topN(s.ngrams, 2, 100)
	f.Locations = topN(s.locations, 1, 100)
	f.TimeBuckets = sortedCounts(s.months, 1)

	reps := make([]repeatItem, 0, len(s.repeats))
	for msg, n := range s.repeats {
		reps = append(reps, repeatItem{Count: n, Message: msg})
	}
	sort.Slice(reps, func(i, j int) bool {
		if reps[i].Count != reps[j].Count {
			return reps[i].Count > reps[j].Count
		}
		return reps[i].Message < reps[j].Message
	})
	if len(reps) > 20 {
		reps = reps[:20]
	}
	f.TopRepeated = reps
	return f
}

// topN 取出计数最高的 n 个条目。
//
// 次级排序键是名字，不是为了好看：没有它的话，计数相同的条目顺序取决于
// map 的遍历顺序，同一份数据两次跑出来的 stats.json 字节不一样，
// 于是「这份文件变了吗」这种再正常不过的检查就没法做了。
func topN(m map[string]int, minCount, n int) []counted {
	out := make([]counted, 0, len(m))
	for k, v := range m {
		if v >= minCount {
			out = append(out, counted{Value: k, Count: v})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Value < out[j].Value
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// sortedCounts 按名字排序，用于时间桶这类本身就是有序的维度——
// 时间轴上「哪个月最热闹」要靠相邻对比才看得出来，打乱就没有意义了。
func sortedCounts(m map[string]int, minCount int) []counted {
	out := make([]counted, 0, len(m))
	for k, v := range m {
		if v >= minCount {
			out = append(out, counted{Value: k, Count: v})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })
	return out
}

// ---- manifest.json ----

type manifestFile struct {
	Path        string `json:"path"`
	Format      string `json:"format"`
	Description string `json:"description"`
	Bytes       int64  `json:"bytes,omitempty"`

	// Records 统一表示「这个文件里的评论条数」，与 totals 里的
	// root_comments + sub_replies 对得上。jsonl 还会带上视频行、墓碑行
	// 和 summary 行，所以它的行数比这个数字多几条——那几行是元数据，
	// 不是评论。各格式用同一个含义，下游才能横向比对；
	// 按行数算的话 jsonl 会凭空多出几条，看上去像两种格式抓到的不是同一批数据。
	Records int `json:"records,omitempty"`
}

type manifestDoc struct {
	Tool         string          `json:"tool"`
	Version      string          `json:"version"`
	GeneratedAt  string          `json:"generated_at"`
	Video        *output.Video   `json:"video"`
	Completeness *output.Summary `json:"completeness"`

	// Totals 与 stats.json 里的同名键是同一份数据，键名也必须一样：
	// 同一个东西在两个文件里有两个名字，下游就得写两套解析。
	Totals    statsTotals    `json:"totals"`
	Files     []manifestFile `json:"files"`
	ReadFirst []string       `json:"read_first"`
}

// 各格式文件在这个数据集里的角色，写进 manifest 供下游（尤其是 LLM）挑。
var formatRole = map[string]string{
	"jsonl": "主数据，每行一条 JSON，程序消费用",
	"tsv":   "表格，token 最省，适合整份读进上下文",
	"md":    "带楼中楼层级的可读版本，适合读讨论",
	"json":  "一个完整嵌套的 JSON 对象，兼容只吃标准 JSON 的工具",
}

func (s *datasetScan) manifest(meta *output.Meta, sk *sink) *manifestDoc {
	doc := &manifestDoc{
		Tool:         "bili-comment",
		Version:      Version,
		GeneratedAt:  time.Now().Format(time.RFC3339),
		Video:        meta.Video,
		Completeness: s.summary,
		Totals: statsTotals{
			RootComments: s.comments,
			SubReplies:   s.replies,
			Users:        len(s.users),
			Stubs:        s.stubs,
			StubReplies:  s.stubSum,
		},
		ReadFirst: []string{"stats.json", "schema.md"},
	}
	if !s.firstC.IsZero() {
		doc.Totals.FirstComment = s.firstC.Format(time.RFC3339)
		doc.Totals.LastComment = s.lastC.Format(time.RFC3339)
	}

	// 顺序固定，便于比对两次生成的差异。
	names := make([]string, 0, len(sk.writers))
	byName := map[string]*formatWriter{}
	for _, w := range sk.writers {
		names = append(names, w.name)
		byName[w.name] = w
	}
	sort.Strings(names)
	for _, n := range names {
		w := byName[n]
		f := manifestFile{
			Path:        filepath.Base(w.path),
			Format:      n,
			Description: formatRole[n],
			Bytes:       w.n,
		}
		// 所有格式记同一个数字：评论条数。jsonl 还多带视频行、墓碑行和
		// summary 行，Markdown 把墓碑行折进结尾的完整度一栏——
		// 那些都不是评论，谁多带了什么都不该反映在这个数字上。
		f.Records = s.comments + s.replies
		doc.Files = append(doc.Files, f)
	}
	// 附带文件也列进去，让「这个目录里有什么」在一个地方说完。
	for _, extra := range []struct{ name, desc string }{
		{"stats.json", "本地预聚合的统计，多数问题看完它就能回答，不必读全量"},
		{"schema.md", "字段与标记的自描述，以及完整性的含义"},
		{"manifest.json", "本文件"},
	} {
		doc.Files = append(doc.Files, manifestFile{Path: extra.name, Description: extra.desc})
	}
	return doc
}
