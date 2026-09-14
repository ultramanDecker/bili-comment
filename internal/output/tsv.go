package output

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// TSV 是给 LLM 的主力格式。
//
// 同一行数据，JSON 与 TSV 的 token 数大约是 55 : 28——差的不是数据，
// 是每条记录都要重写一遍的键名。一万条评论下这个差距决定了整份数据
// 能不能塞进上下文。
//
// 布局分三段：
//
//	# 开头块    视频标识、抓取参数。写任何行之前就知道，先写。
//	表头 + 行   只有一个列结构，**只有评论**。
//	# 结尾块    完整性与墓碑汇总，抓完才知道，只能写在最后。
//
// 为什么墓碑不做成行：TSV 的价值在于列结构整齐，塞进结构不同的行就等于
// 把表格变成自由格式，下游每读一行都要先判断它是什么。墓碑是「覆盖范围」
// 的说明而非数据，放结尾块里既保住了表格的整齐，也没有隐瞒它——
// 结尾块明写「有 N 栋楼、共 M 条回复没有抓」。
//
// 用 `grep -v '^#'` 就能剥掉注释块，剩下的部分对 cut / awk 完全友好。
type TSV struct {
	bw *bufio.Writer
}

func (f *TSV) Name() string    { return "tsv" }
func (f *TSV) Ext() string     { return "tsv" }
func (f *TSV) Schema() []Field { return CommentSchema() }
func (f *TSV) Flush() error    { return f.bw.Flush() }

func (f *TSV) Begin(w io.Writer, m *Meta) error {
	f.bw = bufio.NewWriter(w)
	if m.Resuming {
		// 表头与开头块在上次保留的前缀里，再写一遍就是重复行。
		return nil
	}

	fmt.Fprintf(f.bw, "# bili-comment 数据集，格式版本 1\n")
	if m.Video != nil {
		v := m.Video
		fmt.Fprintf(f.bw, "# video\t%s\t%s\n", v.BVID, tsvField(v.Title))
		fmt.Fprintf(f.bw, "# up\t%d\t%s\n", v.UpMid, tsvField(v.UpName))
		fmt.Fprintf(f.bw, "# published\t%s\n", v.PubTime.Time().Format("2006-01-02 15:04:05"))
		fmt.Fprintf(f.bw, "# stat_reply\t%d\n", v.StatReply)
		fmt.Fprintf(f.bw, "# fetched_at\t%s\n", v.FetchedAt.Time().Format("2006-01-02 15:04:05"))
	}
	fmt.Fprintf(f.bw, "# mode\t%s\n", m.Mode)
	fmt.Fprintf(f.bw, "# columns\t%s\n", strings.Join(f.columnNames(), "\t"))

	// 表头。表头只写一次是这个格式省 token 的全部秘密。
	for i, fd := range commentFields {
		if i > 0 {
			f.bw.WriteByte('\t')
		}
		f.bw.WriteString(fd.Name)
	}
	f.bw.WriteByte('\n')
	return f.bw.Flush()
}

func (f *TSV) Write(r Row) error {
	// 只有评论进表体，见类型注释。
	if r.Kind != KindComment || r.Comment == nil {
		return nil
	}
	vals := CommentValues(r.Comment)
	for i, v := range vals {
		if i > 0 {
			f.bw.WriteByte('\t')
		}
		f.bw.WriteString(escapeTSV(v))
	}
	return f.bw.WriteByte('\n')
}

func (f *TSV) End(s *Summary) error {
	if s == nil {
		return f.bw.Flush()
	}
	f.bw.WriteString("#\n")
	fmt.Fprintf(f.bw, "# completeness\troot\t%d\t%d\t%s\n",
		s.Fetched, s.Expected, truncatedWord(s.Truncated))
	fmt.Fprintf(f.bw, "# completeness\troot_reason\t%s\n", ReasonText(s.Reason))
	if r := s.Replies; r != nil {
		fmt.Fprintf(f.bw, "# completeness\tsub_replies\t%d\t%d\t%s\n",
			r.Fetched, r.Expected, truncatedWord(r.Truncated))
		fmt.Fprintf(f.bw, "# completeness\tsub_reply_threads\texpanded=%d\tskipped=%d\toffset_limited=%d\n",
			r.Expanded, r.Skipped, r.OffsetLimited)
		if r.SkippedReplies > 0 {
			fmt.Fprintf(f.bw, "# not_fetched\t%d 栋楼共 %d 条回复未抓取\n", r.Skipped, r.SkippedReplies)
			for _, rc := range r.ReasonsSorted() {
				fmt.Fprintf(f.bw, "# not_fetched_reason\t%s\t%d\n", rc.Reason, rc.Count)
			}
		}
	}
	if s.Error != "" {
		fmt.Fprintf(f.bw, "# error\t%s\n", tsvField(s.Error))
	}
	f.bw.WriteString("#\n")
	f.bw.WriteString("# 完整性的含义见同目录的 schema.md。字段顺序见上面的 # columns 行。\n")
	return f.bw.Flush()
}

func (f *TSV) columnNames() []string {
	out := make([]string, len(commentFields))
	for i, fd := range commentFields {
		out[i] = fd.Name
	}
	return out
}

func truncatedWord(t bool) string {
	if t {
		return "truncated"
	}
	return "complete"
}

// escapeTSV 把会破坏列结构、以及会把一条记录拆成两行的字符转义掉。
//
// 评论正文里换行极其常见（有人写小作文），直接写出去会让一行变三行，
// 下游按行读就会把一段话切成三条记录。用反斜杠转义而不是删除：
// 删掉换行会把两句话粘成一句，改的是数据；转义只是换个写法，数据没变。
//
// 反斜杠自身必须第一个转义，否则后面转出来的 \t 会被再转一遍。
func escapeTSV(s string) string {
	if !strings.ContainsAny(s, "\\\t\n\r") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// tsvField 转义注释块里的自由文本。
//
// 注释块用制表符分列，所以标题、昵称、错误信息里的制表符和换行同样要转义——
// 昵称里带换行的人在 B 站是真实存在的。
func tsvField(s string) string { return escapeTSV(s) }
