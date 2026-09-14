package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ultramanDecker/bili-comment/internal/model"
)

var (
	base  = time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	t0    = base.Add(2 * time.Hour)
	t1    = base.Add(26 * time.Hour)
	fetch = base.Add(48 * time.Hour)
)

func testMeta() *Meta {
	return &Meta{
		Video: &Video{
			Type: "video", BVID: "BV1xx411c7mD", AID: 12345,
			Title: "标题里\t有制表符\n和换行", UpMid: 99, UpName: "UP\n主",
			PubTime: model.Time(base), StatReply: 42,
			Mode: "hot", FetchedAt: model.Time(fetch),
		},
		Mode:      "hot",
		FetchedAt: model.Time(fetch),
		Full:      true,
	}
}

func root(rpid uint64, msg string, like int, replies int) *model.Comment {
	return &model.Comment{
		Rpid: rpid, User: model.User{Mid: int64(rpid), Name: "用户A"},
		Message: msg, Like: like, Ctime: model.Time(t0),
		ReplyCount: replies, Location: "上海", Rel: "2小时",
	}
}

func reply(rpid, rootID, parent uint64, msg string) *model.Comment {
	return &model.Comment{
		Rpid: rpid, Root: rootID, Parent: parent,
		User:    model.User{Mid: int64(rpid), Name: "用户B"},
		Message: msg, Like: 1, Ctime: model.Time(t1), Rel: "1天",
	}
}

// render 按真实调用顺序把一批行喂给某个格式，返回写出的文本。
func render(t *testing.T, f Formatter, m *Meta, rows []Row, s *Summary) string {
	t.Helper()
	var buf bytes.Buffer
	if err := f.Begin(&buf, m); err != nil {
		t.Fatalf("Begin 失败：%v", err)
	}
	for _, r := range rows {
		if err := f.Write(r); err != nil {
			t.Fatalf("Write 失败：%v", err)
		}
	}
	if err := f.End(s); err != nil {
		t.Fatalf("End 失败：%v", err)
	}
	if err := f.Flush(); err != nil {
		t.Fatalf("Flush 失败：%v", err)
	}
	return buf.String()
}

func mustFormat(t *testing.T, name string) Formatter {
	t.Helper()
	f, err := New(name)
	if err != nil {
		t.Fatalf("建 %s 失败：%v", name, err)
	}
	return f
}

// 表头与数据行必须来自同一张字段表。两处一旦不同步，TSV 不会报错，
// 只会让下游把点赞数当成 uid 用——这种错误没有任何提示。
func TestSchemaAndValuesStayInSync(t *testing.T) {
	fields := CommentSchema()
	vals := CommentValues(root(1, "顶", 3, 0))
	if len(fields) != len(vals) {
		t.Fatalf("字段表有 %d 列，取值有 %d 个——表头与数据会错位", len(fields), len(vals))
	}
	seen := map[string]bool{}
	for i, f := range fields {
		if f.Name == "" || f.Type == "" || f.Desc == "" {
			t.Errorf("第 %d 列的元信息不完整：%+v", i, f)
		}
		if seen[f.Name] {
			t.Errorf("字段名 %q 重复", f.Name)
		}
		seen[f.Name] = true
	}

	// 每一种有列概念的格式看到的都必须是同一张表，否则 schema.md 在骗人。
	for _, name := range Names() {
		f := mustFormat(t, name)
		if got := f.Schema(); len(got) != len(fields) {
			t.Errorf("%s 的字段数是 %d，期望 %d", name, len(got), len(fields))
		}
	}
}

func TestCanonicalNamesAndExtension(t *testing.T) {
	cases := map[string]string{
		"ndjson": "jsonl", "JSONL": "jsonl", " markdown ": "md", "MD": "md", "tsv": "tsv",
	}
	for in, want := range cases {
		f, err := New(in)
		if err != nil {
			t.Fatalf("New(%q) 失败：%v", in, err)
		}
		if f.Name() != want {
			t.Errorf("New(%q).Name() = %q，期望 %q", in, f.Name(), want)
		}
		if canonical(in) != want {
			t.Errorf("canonical(%q) = %q，期望 %q", in, canonical(in), want)
		}
	}

	if _, err := New("xml"); err == nil {
		t.Error("不认识的格式应当报错")
	}
	// 别名不该在 Names 里出现两次，否则帮助信息里会重复。
	n := 0
	for _, name := range Names() {
		if name == "ndjson" || name == "markdown" {
			t.Errorf("Names 里出现了别名 %q：%v", name, Names())
		}
		n++
	}
	if n != 4 {
		t.Errorf("格式数 = %d，期望 4：%v", n, Names())
	}

	for path, want := range map[string]string{
		"a.jsonl": "jsonl", "a.ndjson": "jsonl", "a.json": "json",
		"a.tsv": "tsv", "a.tab": "tsv", "a.md": "md", "a.txt": "",
	} {
		if got := ByExt(path); got != want {
			t.Errorf("ByExt(%q) = %q，期望 %q", path, got, want)
		}
	}

	if !Streaming("jsonl") || !Streaming("tsv") || Streaming("json") {
		t.Error("只有 json 不能流式写——它没有「写到第几字节」这回事，做不了续传锚点")
	}
}

// TSV 的全部价值在于列结构整齐。所以两条硬性要求：
// 表体里只有评论，且每一行的列数与表头严格相等。
func TestTSVIsATable(t *testing.T) {
	rows := []Row{
		{Kind: KindVideo, Video: testMeta().Video},
		{Kind: KindComment, Comment: root(1, "顶", 3, 2)},
		{Kind: KindComment, Comment: reply(2, 1, 0, "同感")},
		{Kind: KindStub, Stub: &Stub{Type: "reply_stub", Rpid: 3, ReplyCount: 37, Reason: "not_in_top_n"}},
		{Kind: KindComment, Comment: root(4, "第二栋楼", 1, 0)},
	}
	s := &Summary{
		Type: "summary", Expected: 10, Fetched: 2, Pages: 1, Reason: "limit",
		Replies: &ReplySummary{
			Expected: 5, Fetched: 1, Expanded: 1, Skipped: 3, SkippedReplies: 61,
			RootsWithReplies: 1, Reasons: map[string]int{"not_in_top_n": 3},
		},
	}
	out := render(t, mustFormat(t, "tsv"), testMeta(), rows, s)

	cols := len(CommentSchema())
	var data, comments int
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		data++
		n := len(strings.Split(line, "\t"))
		if data == 1 {
			if n != cols {
				t.Errorf("表头有 %d 列，期望 %d：%q", n, cols, line)
			}
			continue
		}
		comments++
		if n != cols {
			t.Errorf("数据行有 %d 列，期望 %d：%q", n, cols, line)
		}
	}
	if data == 0 || data-1 != comments {
		t.Errorf("表体行数 = %d（表头 1 + 评论 %d），期望 1 + 3", data, comments)
	}
	if comments != 3 {
		t.Errorf("表体里有 %d 行，期望 3——墓碑不是评论，不该混进表体", comments)
	}

	// 表头必须与 # columns 行一致：下游拿到一份文件时先读的是后者。
	if !strings.Contains(out, "# columns\t"+strings.Join(colNames(), "\t")) {
		t.Errorf("# columns 行与表头不一致：\n%s", out)
	}

	// grep -v '^#' 之后剩下的必须是干净的表：表头 + 3 行数据。
	var clean []string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if !strings.HasPrefix(line, "#") {
			clean = append(clean, line)
		}
	}
	if len(clean) != 4 {
		t.Errorf("剥掉 # 之后剩 %d 行，期望 4（表头 + 3 条评论）：%q", len(clean), clean)
	}

	// 墓碑不能被丢掉：它要说清「有 3 栋楼共 61 条回复没抓」。
	for _, want := range []string{
		"# completeness\troot\t2\t10\tcomplete",
		"# completeness\tsub_reply_threads\texpanded=1\tskipped=3",
		"# not_fetched\t3 栋楼共 61 条回复未抓取",
		"# not_fetched_reason\tnot_in_top_n\t3",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("结尾块缺少 %q：\n%s", want, out)
		}
	}
}

func colNames() []string {
	out := make([]string, 0, len(CommentSchema()))
	for _, f := range CommentSchema() {
		out = append(out, f.Name)
	}
	return out
}

// 评论正文里的换行极其常见。直接写出去会让一行变三行，下游按行读就会
// 把一段话切成三条记录——而且切出来的每一条看起来都是合法数据。
func TestTSVEscapesStructureBreakers(t *testing.T) {
	msg := "第一行\n第二行\ttab 后面\r回车\\反斜杠"
	out := render(t, mustFormat(t, "tsv"), testMeta(),
		[]Row{{Kind: KindComment, Comment: root(1, msg, 0, 0)}}, nil)

	body := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "1\t") {
			body = line
		}
	}
	if body == "" {
		t.Fatalf("没找到数据行：\n%s", out)
	}
	if strings.Count(body, "\t") != len(CommentSchema())-1 {
		t.Errorf("正文里的制表符没被转义，列数变成了 %d：%q",
			strings.Count(body, "\t")+1, body)
	}
	for _, want := range []string{`第一行\n第二行`, `\ttab 后面`, `\r回车`, `\\反斜杠`} {
		if !strings.Contains(body, want) {
			t.Errorf("正文里缺少转义后的 %q：%q", want, body)
		}
	}

	// 反斜杠必须第一个转义，否则转出来的 \t 会被再转一遍变成 \\t。
	if escapeTSV(`\t`) != `\\t` {
		t.Errorf("escapeTSV(`\\t`) = %q，期望 %q", escapeTSV(`\t`), `\\t`)
	}
	if escapeTSV("原样") != "原样" {
		t.Error("没有特殊字符时不该改动内容")
	}
}

// 续传时文件里已经有上次写下的开头块，再写一遍就是重复行。
func TestBeginSkipsHeaderWhenResuming(t *testing.T) {
	for _, name := range []string{"tsv", "md", "jsonl"} {
		m := testMeta()
		m.Resuming = true
		out := render(t, mustFormat(t, name), m,
			[]Row{{Kind: KindComment, Comment: root(1, "顶", 0, 0)}}, nil)
		if strings.Contains(out, "BV1xx411c7mD") {
			t.Errorf("%s 在续传时重写了开头块：\n%s", name, out)
		}
		if !strings.Contains(out, "顶") {
			t.Errorf("%s 在续传时连数据行一起丢了：\n%s", name, out)
		}
	}

	// 对照：不续传时必须写出视频信息，否则文件不自解释。
	out := render(t, mustFormat(t, "tsv"), testMeta(),
		[]Row{{Kind: KindComment, Comment: root(1, "顶", 0, 0)}}, nil)
	if !strings.Contains(out, "BV1xx411c7mD") {
		t.Errorf("全新抓取时开头块应当带上 BV 号：\n%s", out)
	}
}

// 楼中楼按楼成组到达，但组的边界要靠格式自己认。同一个章节标题写两遍、
// 或者把两栋楼混成一组，读起来就完全不是那个意思了。
func TestMarkdownGroupsRepliesPerRoot(t *testing.T) {
	rows := []Row{
		{Kind: KindComment, Comment: root(1, "第一栋", 10, 3)},
		{Kind: KindComment, Comment: root(2, "第二栋", 5, 2)},
		{Kind: KindComment, Comment: reply(10, 1, 0, "回复一")},
		{Kind: KindComment, Comment: reply(11, 1, 0, "回复二")},
		{Kind: KindComment, Comment: reply(20, 2, 0, "回复三")},
	}
	out := render(t, mustFormat(t, "md"), testMeta(), rows, &Summary{
		Type: "summary", Expected: 2, Fetched: 2, Reason: "end",
		Replies: &ReplySummary{Expected: 3, Fetched: 3, Expanded: 2, RootsWithReplies: 2},
	})

	if n := strings.Count(out, "## 楼中楼"); n != 1 {
		t.Errorf("「## 楼中楼」出现了 %d 次，期望 1", n)
	}
	if n := strings.Count(out, "## 一级评论"); n != 1 {
		t.Errorf("「## 一级评论」出现了 %d 次，期望 1", n)
	}
	if n := strings.Count(out, "### ↳ "); n != 2 {
		t.Errorf("楼中楼小节有 %d 个，期望 2（每栋楼一个）", n)
	}
	// 每组的标题要说清是哪一栋楼、有几条回复。
	if !strings.Contains(out, "### ↳ 用户A 的楼 · 2 条回复（服务端报告 3）") {
		t.Errorf("第一个小节的标题不对：\n%s", out)
	}
	if !strings.Contains(out, "### ↳ 用户A 的楼 · 1 条回复（服务端报告 2）") {
		t.Errorf("第二个小节的标题不对：\n%s", out)
	}

	// 结构必须严格是：A 楼标题 → A 的两条回复 → B 楼标题 → B 的回复。
	// 每栋楼的回复落在那栋楼的标题之下，是这份文件可读的全部依据。
	seq := []string{
		"### ↳ 用户A 的楼 · 2 条回复",
		"回复一",
		"回复二",
		"### ↳ 用户A 的楼 · 1 条回复",
		"回复三",
	}
	at := -1
	for _, s := range seq {
		i := strings.Index(out, s)
		if i < 0 {
			t.Fatalf("输出里找不到 %q：\n%s", s, out)
		}
		if i < at {
			t.Errorf("%q 出现在它该在的位置之前：\n%s", s, out)
		}
		at = i
	}

	// 楼中楼一节必须在一级评论之后，且一级评论一条都不能落到楼中楼里。
	if ri, rr := strings.Index(out, "## 一级评论"), strings.Index(out, "## 楼中楼"); ri > rr {
		t.Errorf("章节顺序反了：一级评论 %d，楼中楼 %d", ri, rr)
	}
	if i, j := strings.Index(out, "第一栋"), strings.Index(out, "## 楼中楼"); i > j {
		t.Error("一级评论的正文落到了楼中楼一节里")
	}
}

// 层级靠 parent 链条还原，而链条断掉是常态：被回复的那条可能已被删除，
// 或者根本没抓到。断掉时停在已知的深度上，不猜，也不能死循环。
func TestMarkdownDepthSurvivesBrokenChains(t *testing.T) {
	byRpid := map[uint64]*model.Comment{
		11: reply(11, 1, 0, "直接回复楼主"),
		12: reply(12, 1, 11, "回复 11"),
		13: reply(13, 1, 12, "回复 12"),
	}
	if got := depthOf(byRpid[11], byRpid, 1); got != 0 {
		t.Errorf("直接回复楼主 = %d 层，期望 0", got)
	}
	if got := depthOf(byRpid[12], byRpid, 1); got != 1 {
		t.Errorf("回复一条回复 = %d 层，期望 1", got)
	}
	if got := depthOf(byRpid[13], byRpid, 1); got != 2 {
		t.Errorf("三层 = %d 层，期望 2", got)
	}
	// 父评论不在数据里（已被删除或没抓到）：停在能确认的那一层。
	orphan := reply(14, 1, 999, "回复了一条不存在的评论")
	if got := depthOf(orphan, byRpid, 1); got != 0 {
		t.Errorf("断链时 = %d 层，期望 0", got)
	}
	// 数据里出现环（A 回复 B、B 回复 A）不能把程序转死。
	loopA := reply(30, 1, 31, "甲")
	loopB := reply(31, 1, 30, "乙")
	loops := map[uint64]*model.Comment{30: loopA, 31: loopB}
	if got := depthOf(loopA, loops, 1); got > maxDepth {
		t.Errorf("环里算出 %d 层，超过了上限 %d", got, maxDepth)
	}
}

// 正文里的换行要原样保留并重新缩进：有人会在评论里写分行的小作文，
// 压掉换行会改变它的意思。但空行不该留下缩进残渣。
func TestMarkdownKeepsLineBreaksInBody(t *testing.T) {
	cm := root(1, "第一句\n\n  第二句  \n第三句\n", 0, 0)
	out := render(t, mustFormat(t, "md"), testMeta(),
		[]Row{{Kind: KindComment, Comment: cm}}, nil)
	// 每行都按两级缩进对齐；行内原有的前导空格是作者自己敲的，保留。
	for _, want := range []string{"  第一句\n", "    第二句", "  第三句\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("正文里缺少 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "\n\n\n") {
		t.Errorf("空行留下了多余的缩进残渣：\n%s", out)
	}
}

// JSONL 是唯一能作为续传锚点的格式：每行独立，截断到任意一条记录的边界
// 都是一份合法文件。所以它必须逐行可解析，且类型可分辨。
func TestJSONLIsLineDelimited(t *testing.T) {
	// 视频行由 Begin 从 Meta 里写出，调用方不会再推一个 KindVideo——
	// 推了就会写出两行，而下游按「第一行是视频」读，多出来那行会被当成评论。
	rows := []Row{
		{Kind: KindComment, Comment: root(1, "顶", 3, 2)},
		{Kind: KindStub, Stub: &Stub{Type: "reply_stub", Rpid: 1, ReplyCount: 37, Reason: "not_in_top_n"}},
		{Kind: KindComment, Comment: reply(2, 1, 0, "同感")},
	}
	s := &Summary{Type: "summary", Expected: 2, Fetched: 1, Reason: "error", Error: "网络断了"}
	out := render(t, mustFormat(t, "jsonl"), testMeta(), rows, s)

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("写出了 %d 行，期望 5（视频 + 3 条 + 收尾）", len(lines))
	}
	var types []string
	for i, line := range lines {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			t.Fatalf("第 %d 行不是合法 JSON：%v\n%s", i+1, err, line)
		}
		if probe.Type == "" {
			// 评论行没有 type 字段，靠有没有 rpid 认出来。
			var cm model.Comment
			if err := json.Unmarshal([]byte(line), &cm); err != nil || cm.Rpid == 0 {
				t.Fatalf("第 %d 行既不是已知类型也不是评论：%s", i+1, line)
			}
			probe.Type = "comment"
		}
		types = append(types, probe.Type)
	}
	want := []string{"video", "comment", "reply_stub", "comment", "summary"}
	for i := range want {
		if types[i] != want[i] {
			t.Errorf("第 %d 行的类型是 %q，期望 %q", i+1, types[i], want[i])
		}
	}

	// 收尾行必须带上出错信息：下游拿到残缺数据却不知道它残缺，
	// 比拿不到数据更危险。
	if !strings.Contains(out, "网络断了") {
		t.Errorf("收尾行丢了错误信息：\n%s", out)
	}

	// 时间戳是秒级整数，不是 RFC3339——这一条直接影响整份文件能不能
	// 塞进 LLM 上下文。
	if !strings.Contains(out, `"ctime":`+strconv.FormatInt(t0.Unix(), 10)) {
		t.Errorf("时间戳没有被写成秒级整数：\n%s", out)
	}
}

// JSON 的定位是兼容性：它必须在内存里攒齐才对，但攒齐之后的结构要
// 能被只吃标准 JSON 的工具直接解析。
func TestJSONIsOneCompleteDocument(t *testing.T) {
	rows := []Row{
		{Kind: KindComment, Comment: root(1, "顶", 3, 2)},
		{Kind: KindComment, Comment: reply(2, 1, 0, "同感")},
	}
	s := &Summary{
		Type: "summary", Expected: 9, Fetched: 1, Reason: "limit", Truncated: true,
		Replies: &ReplySummary{Expected: 4, Fetched: 1, Truncated: true},
	}
	out := render(t, mustFormat(t, "json"), testMeta(), rows, s)

	var doc struct {
		Complete     bool `json:"complete"`
		Completeness struct {
			RootComments struct{ Expected, Fetched int } `json:"root_comments"`
			SubReplies   struct{ Expected, Fetched int } `json:"sub_replies"`
			Truncated    bool                            `json:"truncated"`
		} `json:"completeness"`
		Comments []model.Comment `json:"comments"`
		Video    struct {
			BVID string `json:"bvid"`
		} `json:"video"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("整体不是合法 JSON：%v\n%s", err, out)
	}
	if len(doc.Comments) != 2 {
		t.Errorf("评论数 = %d，期望 2", len(doc.Comments))
	}
	if doc.Video.BVID != "BV1xx411c7mD" {
		t.Errorf("视频信息丢了：%+v", doc.Video)
	}
	if doc.Complete {
		t.Error("被截断的数据不该被标成 complete")
	}
	if doc.Completeness.RootComments.Fetched != 1 || doc.Completeness.SubReplies.Expected != 4 {
		t.Errorf("完整性数据不对：%+v", doc.Completeness)
	}

	// 没有评论时也必须是空列表而不是 null：null 会让下游的 for 循环炸掉。
	empty := render(t, mustFormat(t, "json"), testMeta(), nil, nil)
	if !strings.Contains(empty, `"comments": []`) {
		t.Errorf("没有评论时应当输出空列表：\n%s", empty)
	}
}

// ReasonText / SkipReasonText 是把「为什么数据不全」翻译成人话的地方。
// 翻译不出来时原样透传是对的——编一个说法比不解释更糟。
func TestReasonTexts(t *testing.T) {
	for _, r := range []string{"limit", "since", "end", "offset_limit", "cursor_stuck", "error"} {
		if ReasonText(r) == r || ReasonText(r) == "" {
			t.Errorf("ReasonText(%q) 没有被翻译：%q", r, ReasonText(r))
		}
	}
	if ReasonText("") != "未知" {
		t.Errorf("空原因 = %q，期望「未知」", ReasonText(""))
	}
	if ReasonText("未来才有的原因") != "未来才有的原因" {
		t.Error("不认识的原因应当原样透传，而不是编一个说法")
	}

	for _, r := range []string{"not_in_top_n", "below_threshold", "no_replies", "replies_disabled"} {
		if SkipReasonText(r) == r || SkipReasonText(r) == "" {
			t.Errorf("SkipReasonText(%q) 没有被翻译：%q", r, SkipReasonText(r))
		}
	}
	if SkipReasonText("别的") != "别的" {
		t.Error("不认识的跳过原因应当原样透传")
	}
}

// 墓碑在 Markdown 里不逐条列出，只汇总到文末——三千栋楼逐条列出来
// 会把要读的内容淹掉。但汇总必须说清漏了多少，否则读者会以为评论区
// 只有他读到的那些。
func TestMarkdownFoldsStubsIntoCompleteness(t *testing.T) {
	rows := []Row{
		{Kind: KindComment, Comment: root(1, "只有一栋楼展开", 10, 900)},
		{Kind: KindStub, Stub: &Stub{Type: "reply_stub", Rpid: 2, ReplyCount: 802, Reason: "not_in_top_n"}},
		{Kind: KindStub, Stub: &Stub{Type: "reply_stub", Rpid: 3, ReplyCount: 37, Reason: "not_in_top_n"}},
	}
	s := &Summary{
		Type: "summary", Expected: 3, Fetched: 3, Reason: "end",
		Replies: &ReplySummary{
			Expected: 10, Fetched: 10, Expanded: 1, Skipped: 2, SkippedReplies: 839,
			RootsWithReplies: 3, Reasons: map[string]int{"not_in_top_n": 2},
		},
	}
	out := render(t, mustFormat(t, "md"), testMeta(), rows, s)

	if strings.Contains(out, "802") {
		t.Errorf("墓碑不该被逐条列进 Markdown：\n%s", out)
	}
	for _, want := range []string{
		"- 楼中楼：**10 / 10**，展开 1 栋，未展开 2 栋",
		"未展开的 2 栋楼自称共有 **839** 条回复",
		"不在 --replies-top 选中的范围内：2 栋",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("完整性一节缺少 %q：\n%s", want, out)
		}
	}
	// 结论性提醒必须在文件里：这份数据是给 LLM 读的，
	// 而基于部分评论得出「主流观点是 X」正是它最容易犯的错。
	if !strings.Contains(out, "请先确认上面的完整性数据") {
		t.Errorf("文末缺少完整性提醒：\n%s", out)
	}
}

// 一条一级评论都没有时要写清楚「这个视频没有评论」，
// 否则整份文件看起来像是内容丢了。
func TestMarkdownEmptyDatasetSaysSo(t *testing.T) {
	out := render(t, mustFormat(t, "md"), testMeta(), nil,
		&Summary{Type: "summary", Expected: 0, Fetched: 0, Reason: "end"})
	if !strings.Contains(out, "没有抓到一级评论") {
		t.Errorf("空数据集没有说明：\n%s", out)
	}
	// 出错中断时没写出收尾统计，这本身也要说出来。
	out = render(t, mustFormat(t, "md"), testMeta(),
		[]Row{{Kind: KindComment, Comment: root(1, "顶", 0, 0)}}, nil)
	if !strings.Contains(out, "没有写出收尾统计") {
		t.Errorf("缺少收尾统计时没有说明：\n%s", out)
	}
}

// schema.md 是这份数据下个月被另一个人的 LLM 读到时唯一的解释来源，
// 所以每个字段、每个标记、以及完整性的含义都要在里面。
func TestSchemaMarkdownDocumentsEverything(t *testing.T) {
	s := &Summary{
		Type: "summary", Expected: 100, Fetched: 40, Reason: "limit", Truncated: true,
		Replies: &ReplySummary{Expected: 500, Fetched: 100, Expanded: 3, Skipped: 7, SkippedReplies: 400},
	}
	out := SchemaMarkdown(testMeta(), s)

	if !strings.Contains(out, "BV1xx411c7mD") {
		t.Error("schema.md 里应当有视频身份信息")
	}
	for _, f := range CommentSchema() {
		if !strings.Contains(out, "`"+f.Name+"`") {
			t.Errorf("schema.md 里缺少字段 %q", f.Name)
		}
	}
	for _, f := range FlagDoc() {
		if !strings.Contains(out, "`"+f.Name+"`") {
			t.Errorf("schema.md 里缺少标记 %q", f.Name)
		}
	}
	// 工具对数据做的判断，判断依据必须写在数据旁边。
	if !strings.Contains(out, "SimHash") {
		t.Error("repeat 标记没有说明判据")
	}
	// 两种回复数不可比，这一点最容易被下游误读，必须写进去。
	if !strings.Contains(out, "上界") {
		t.Error("schema.md 没有说明墓碑回复数是上界")
	}
	if !strings.Contains(out, "被截断") {
		t.Error("schema.md 没有反映截断状态")
	}
}

// Go 默认把尖括号和 & 转义成 \< 这样的六字节写法，防的是把 JSON 内联进
// <script> 标签的注入——这个工具没有那个场景。评论区里这些字符不少见，
// 而每一行都是给 LLM 读的，多出来的字节是实打实的成本。
func TestJSONLDoesNotEscapeHTML(t *testing.T) {
	cm := root(1, "笑死 >_<  A&B  x < y", 0, 0)
	out := render(t, mustFormat(t, "jsonl"), testMeta(),
		[]Row{{Kind: KindComment, Comment: cm}}, nil)

	esc := func(r rune) string { return fmt.Sprintf(`\u%04x`, r) }
	for _, r := range []rune{'<', '>', '&'} {
		if strings.Contains(out, esc(r)) {
			t.Errorf("%q 被转义成了 %s：%s", r, esc(r), out)
		}
	}
	if !strings.Contains(out, "笑死 >_<  A&B  x < y") {
		t.Errorf("正文没有原样写出：%s", out)
	}

	// 转义关掉之后必须仍然是合法 JSON，且解析回来与原文完全一致。
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var got model.Comment
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &got); err != nil {
		t.Fatalf("关掉 HTML 转义后不是合法 JSON：%v\n%s", err, out)
	}
	if got.Message != cm.Message {
		t.Errorf("解析回来的正文是 %q，期望 %q", got.Message, cm.Message)
	}
}

// complete 是「这份数据能不能用」，是下游最先读、也最容易被单独读走的字段。
// 它必须与嵌套的 completeness.truncated 一致。
//
// 这里踩过的坑：complete 只看了一级评论的截断标记，于是「一级评论抓全了、
// 楼中楼被 --replies-top 裁掉一部分」时输出 complete=true——而同一份文档里
// 的 completeness.truncated 是 true。两个字段自相矛盾，而错的是更显眼的那个。
func TestJSONCompleteAccountsForReplies(t *testing.T) {
	cases := []struct {
		name string
		s    *Summary
		want bool
	}{
		{
			name: "全都抓全了",
			s: &Summary{Expected: 100, Fetched: 100,
				Replies: &ReplySummary{Expected: 50, Fetched: 50}},
			want: true,
		},
		{
			name: "一级评论被截断",
			s:    &Summary{Expected: 89243, Fetched: 5, Truncated: true},
			want: false,
		},
		{
			name: "一级评论抓全，楼中楼被裁",
			s: &Summary{Expected: 100, Fetched: 100,
				Replies: &ReplySummary{Expected: 5000, Fetched: 800, Truncated: true}},
			want: false,
		},
		{
			name: "抓取中途出错",
			s:    &Summary{Expected: 100, Fetched: 40, Error: "网络断了"},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := render(t, mustFormat(t, "json"), testMeta(),
				[]Row{{Kind: KindComment, Comment: root(1, "顶", 0, 0)}}, tc.s)

			var doc struct {
				Complete     bool `json:"complete"`
				Completeness struct {
					Truncated bool `json:"truncated"`
				} `json:"completeness"`
			}
			if err := json.Unmarshal([]byte(out), &doc); err != nil {
				t.Fatalf("输出不是合法 JSON：%v", err)
			}
			if doc.Complete != tc.want {
				t.Errorf("complete = %v，期望 %v", doc.Complete, tc.want)
			}
			// 唯一不许出现的组合：一边说能用、一边说被截断了。
			// 反方向是允许的——抓取中途报错时 complete=false 而
			// truncated=false，那不是截断，是两个不同的原因。
			if doc.Complete && doc.Completeness.Truncated {
				t.Error("complete=true 却 completeness.truncated=true：下游读前者会以为数据可用")
			}
		})
	}
}

// schema.md 是 manifest 里点名让下游「先读」的文件之一。它必须说清
// truncated 只管一级评论——这正是 complete 那个 bug 的根源：字段名没说范围，
// 读的人就当成在说全部。
func TestSchemaMarkdownExplainsTruncatedScope(t *testing.T) {
	md := SchemaMarkdown(testMeta(), nil)
	if !strings.Contains(md, "replies.truncated") {
		t.Error("schema.md 没说明楼中楼有自己的 truncated")
	}
	if !strings.Contains(md, "只管一级评论") {
		t.Error("schema.md 没说明 truncated 只在说一级评论")
	}
}
