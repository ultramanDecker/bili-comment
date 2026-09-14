package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/ultramanDecker/bili-comment/internal/bilibili"
	"github.com/ultramanDecker/bili-comment/internal/model"
	"github.com/ultramanDecker/bili-comment/internal/store"
)

func specPaths(specs []outputSpec) map[string]string {
	out := map[string]string{}
	for _, sp := range specs {
		out[sp.name] = sp.path
	}
	return out
}

func primaryOf(specs []outputSpec) string {
	for _, sp := range specs {
		if sp.primary {
			return sp.name
		}
	}
	return ""
}

// stdout 只有一个通道，多种格式挤在同一条流里谁也读不了。
func TestPlanOutputsToStdout(t *testing.T) {
	dir, specs, err := planOutputs("", nil)
	if err != nil {
		t.Fatalf("默认输出到 stdout 应当可用：%v", err)
	}
	if dir != "" {
		t.Errorf("stdout 模式不该有目录，得到 %q", dir)
	}
	if len(specs) != 1 || specs[0].name != "jsonl" || specs[0].path != "" {
		t.Fatalf("默认应当是单个 jsonl 到 stdout，得到 %+v", specs)
	}
	if !specs[0].primary {
		t.Error("唯一的一种格式必须是主格式")
	}

	if _, _, err := planOutputs("", []string{"jsonl", "tsv"}); err == nil {
		t.Error("多种格式写到 stdout 应当报错，而不是把两种格式混进同一条流")
	}

	// json 要先在内存里攒齐，写到 stdout 会先卡住再一次性吐出，
	// 看上去就像程序挂了。
	if _, _, err := planOutputs("", []string{"json"}); err == nil {
		t.Error("json 写到 stdout 应当被挡下")
	}

	if _, _, err := planOutputs("", []string{"xml"}); err == nil {
		t.Error("不认识的格式应当报错")
	}
}

// 目录模式的产出是一个数据集，所以一定有主数据 jsonl 和随附的
// manifest/stats/schema——它们是「让 LLM 少读一点」的那部分，不是可选项。
func TestPlanOutputsToDirectory(t *testing.T) {
	dirIn := t.TempDir()
	dir, specs, err := planOutputs(dirIn, nil)
	if err != nil {
		t.Fatalf("目录模式失败：%v", err)
	}
	if dir != dirIn {
		t.Errorf("目录 = %q，期望 %q", dir, dirIn)
	}
	got := specPaths(specs)
	if got["jsonl"] != filepath.Join(dirIn, "comments.jsonl") ||
		got["md"] != filepath.Join(dirIn, "comments.md") {
		t.Errorf("默认格式组合不对：%+v", got)
	}
	if primaryOf(specs) != "jsonl" {
		t.Errorf("主格式 = %q，期望 jsonl——它是续传锚点与 stats 的来源", primaryOf(specs))
	}

	// 用户只写 --format md 时补上 jsonl，而不是报错：
	// 他想要的显然是「给我一份数据集」，jsonl 是数据集的一部分。
	_, specs, err = planOutputs(dirIn, []string{"md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 || specPaths(specs)["jsonl"] == "" {
		t.Errorf("目录模式必须带上 jsonl：%+v", specs)
	}
	if primaryOf(specs) != "jsonl" {
		t.Errorf("补出来的 jsonl 也该是主格式，得到 %q", primaryOf(specs))
	}

	// 重复与别名要去重，否则同一个文件会被打开两次、写两份交错的记录。
	_, specs, err = planOutputs(dirIn, []string{"md", "markdown", "ndjson", "tsv"})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 3 {
		t.Fatalf("去重后有 %d 种格式，期望 3（jsonl md tsv）：%+v", len(specs), specs)
	}
	seen := map[string]bool{}
	for _, sp := range specs {
		if seen[sp.path] {
			t.Errorf("同一个路径被规划了两次：%s", sp.path)
		}
		seen[sp.path] = true
	}
}

// 已有的目录、以及带结尾分隔符的写法都算目录。不靠「有没有扩展名」猜——
// `-o out` 这种名字太常见，猜错的话用户会得到一个名叫 out 的文件。
func TestPlanOutputsDirDetection(t *testing.T) {
	d := t.TempDir()
	_, specs, err := planOutputs(d, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(specs[0].path, d) {
		t.Errorf("已存在的目录应当按目录模式处理：%+v", specs)
	}

	_, specs, err = planOutputs(filepath.Join(t.TempDir(), "new")+string(os.PathSeparator), nil)
	if err != nil {
		t.Fatal(err)
	}
	if specs[0].name != "jsonl" || !strings.HasSuffix(specs[0].path, "comments.jsonl") {
		t.Errorf("带结尾分隔符的路径应当按目录模式处理：%+v", specs)
	}

	// 不存在的名字按文件处理。
	dir, specs, err := planOutputs(filepath.Join(t.TempDir(), "out"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if dir != "" {
		t.Errorf("不存在的路径不该被当成目录：%q", dir)
	}
	if len(specs) != 1 || specs[0].name != "jsonl" {
		t.Errorf("文件模式默认只写主格式：%+v", specs)
	}
}

// 文件模式下扩展名决定主格式，--format 只做加法。
func TestPlanOutputsFileMode(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "data.tsv")

	dir, specs, err := planOutputs(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if dir != "" {
		t.Errorf("单文件模式不该有目录：%q", dir)
	}
	// 扩展名给了 tsv，就以 tsv 为主格式，而不是偷偷改成 jsonl。
	if len(specs) != 1 || specs[0].name != "tsv" || specs[0].path != path {
		t.Fatalf("扩展名应当决定主格式：%+v", specs)
	}
	if !specs[0].primary {
		t.Error("主格式标记丢了")
	}

	// --format 是加法：主格式不变，兄弟文件换个扩展名。
	_, specs, err = planOutputs(path, []string{"md", "jsonl"})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 3 {
		t.Fatalf("格式数 = %d，期望 3：%+v", len(specs), specs)
	}
	if primaryOf(specs) != "tsv" {
		t.Errorf("主格式被 --format 顶掉了：%q", primaryOf(specs))
	}
	got := specPaths(specs)
	if got["tsv"] != path {
		t.Errorf("主格式的路径被改了：%q", got["tsv"])
	}
	if got["md"] != filepath.Join(base, "data.md") || got["jsonl"] != filepath.Join(base, "data.jsonl") {
		t.Errorf("兄弟文件没有按主文件名派生：%+v", got)
	}

	// --format 里重复写上主格式不该产生第二个文件。
	_, specs, err = planOutputs(path, []string{"tsv"})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 {
		t.Errorf("--format 重复了主格式，规划出 %d 个输出：%+v", len(specs), specs)
	}

	// 没有扩展名时按 jsonl 处理。
	specsPath := filepath.Join(base, "noext")
	_, specs, err = planOutputs(specsPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if specs[0].name != "jsonl" || specs[0].path != specsPath {
		t.Errorf("没有扩展名时应当按 jsonl 处理：%+v", specs)
	}

	// json 作为主输出会要求把所有评论留在内存里，必须被挡下并给出替代方案。
	if _, _, err := planOutputs(filepath.Join(base, "data.json"), nil); err == nil {
		t.Error("json 作为主输出应当被挡下")
	} else if !strings.Contains(err.Error(), "--format json") {
		t.Errorf("报错要给出替代方案，实际：%v", err)
	}
}

// 续传时格式集合变了，进度里的写入位置就没有对应的文件了：
// 少一个会让那个文件接在半截数据后面写，多一个则没有位置可截断。
func TestCheckResumableRejectsFormatChange(t *testing.T) {
	f := commentsFlags{}
	j := &store.Journal{Phase: store.PhaseReplies, Formats: []string{"jsonl", "md"}}

	ok := []outputSpec{{name: "jsonl", primary: true}, {name: "md"}}
	if err := checkResumable(j, ok, f); err != nil {
		t.Errorf("格式一致时不该拒绝续传：%v", err)
	}

	// 本次多了 tsv：它没有可截断的位置。
	more := append(append([]outputSpec{}, ok...), outputSpec{name: "tsv"})
	err := checkResumable(j, more, f)
	if err == nil {
		t.Fatal("多了格式却允许续传")
	}
	if !strings.Contains(err.Error(), "tsv") || !strings.Contains(err.Error(), "多了") {
		t.Errorf("报错没说清是哪个格式多了：%v", err)
	}

	// 本次少了 md：那个文件会接在半截数据后面继续写。
	err = checkResumable(j, []outputSpec{{name: "jsonl", primary: true}}, f)
	if err == nil {
		t.Fatal("少了格式却允许续传")
	}
	if !strings.Contains(err.Error(), "md") || !strings.Contains(err.Error(), "少了") {
		t.Errorf("报错没说清是哪个格式少了：%v", err)
	}

	// 一增一减要同时说出来，否则用户改了一处还以为改对了。
	err = checkResumable(j, []outputSpec{{name: "jsonl", primary: true}, {name: "tsv"}}, f)
	if err == nil {
		t.Fatal("换了一整套格式却允许续传")
	}
	for _, want := range []string{"tsv", "md", "多了", "少了"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里缺少 %q：%v", want, err)
		}
	}

	// 早期版本留下的进度文件没有格式记录，必须按现状接受，
	// 否则所有老进度文件都会突然无法续传。
	old := &store.Journal{Phase: store.PhaseReplies}
	if err := checkResumable(old, more, f); err != nil {
		t.Errorf("没有格式记录的旧进度不该被拒绝：%v", err)
	}
}

// 楼中楼策略变了就换了计划，进度下标会指向另一栋楼——
// 那会把 A 楼的回复写到 B 楼的进度上。
func TestCheckResumableRejectsPolicyChange(t *testing.T) {
	specs := []outputSpec{{name: "jsonl", primary: true}}
	hash := func(f commentsFlags) string { return f.policy.Fingerprint() }

	f1 := commentsFlags{policy: bilibili.ReplyPolicy{Top: 50}}
	j := &store.Journal{
		Phase:      store.PhaseReplies,
		PolicyHash: hash(f1),
		Formats:    []string{"jsonl"},
	}
	if err := checkResumable(j, specs, f1); err != nil {
		t.Errorf("参数一致时不该拒绝续传：%v", err)
	}

	f2 := commentsFlags{policy: bilibili.ReplyPolicy{Top: 10}}
	if err := checkResumable(j, specs, f2); err == nil {
		t.Fatal("楼中楼参数变了却允许续传")
	}

	// 一级评论阶段还没有计划，策略变了也无所谓——那时 PlanIndex 还不存在。
	j.Phase = store.PhaseRoots
	if err := checkResumable(j, specs, f2); err != nil {
		t.Errorf("一级评论阶段不该受楼中楼参数影响：%v", err)
	}
}

// j2offset 读不到位置时必须给 0，而不是给一个随机值：
// 0 表示「这个文件还没有任何确认完整的内容」，截断到 0 是安全的。
func TestJournalOffsetLookup(t *testing.T) {
	if got := j2offset(nil, "jsonl"); got != 0 {
		t.Errorf("没有进度时 = %d，期望 0", got)
	}
	j := &store.Journal{Offsets: map[string]int64{"jsonl": 4096}}
	if got := j2offset(j, "jsonl"); got != 4096 {
		t.Errorf("jsonl = %d，期望 4096", got)
	}
	// 格式在进度里没有记录，说明上次没输出它——截断到 0 会清空文件，
	// 但那种情况 checkResumable 已经先拦下了。
	if got := j2offset(j, "tsv"); got != 0 {
		t.Errorf("没有记录的格式 = %d，期望 0", got)
	}
	if got := j2offset(&store.Journal{}, "jsonl"); got != 0 {
		t.Errorf("Offsets 为 nil 时 = %d，期望 0", got)
	}
}

// 进度文件必须挂在**主输出的完整路径**上。挂错地方的后果不是报错，而是
// --resume 永远找不到进度，安静地从头重抓并把上次那半截文件截掉——
// 恰好是续传要防的事。
func TestJournalPathPointsAtPrimaryOutput(t *testing.T) {
	dir := t.TempDir()
	video := &model.Video{BVID: "BV1xx411c7mD", AID: 1, PubTime: model.Time(time.Now())}

	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	s, err := openSink(cmd, &globals{}, commentsFlags{out: dir}, video)
	if err != nil {
		t.Fatalf("openSink 失败：%v", err)
	}
	defer s.Close()

	want := filepath.Join(dir, "comments.jsonl")
	if s.journalPath != want {
		t.Fatalf("进度文件挂在 %q，期望 %q", s.journalPath, want)
	}
	// 写入并落盘，然后按 --resume 的路径读回来：这是那个 bug 的完整复现路径。
	if err := s.writeComment(&model.Comment{Rpid: 1, Message: "顶"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.saveJournal(); err != nil {
		t.Fatalf("保存进度失败：%v", err)
	}
	j, err := store.LoadJournal(s.journalPath)
	if err != nil {
		t.Fatalf("按主输出路径读不回进度：%v", err)
	}
	if j.Offsets["jsonl"] <= 0 {
		t.Errorf("进度里没有记录 jsonl 的写入位置：%+v", j.Offsets)
	}
	if len(j.Formats) < 2 {
		t.Errorf("进度里应当记下全部输出格式，得到 %v", j.Formats)
	}
	// 进度文件是旁路文件，不该被藏在目录里——那个路径没人会去读。
	if _, err := os.Stat(filepath.Join(dir, ".resume.json")); err == nil {
		t.Error("进度文件不该被藏在目录里的 .resume.json")
	}
}

// 单文件模式下进度文件就在输出文件旁边，行为与目录模式一致。
func TestJournalPathInFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.jsonl")
	video := &model.Video{BVID: "BV1", PubTime: model.Time(time.Now())}

	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	s, err := openSink(cmd, &globals{}, commentsFlags{out: path}, video)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.journalPath != path {
		t.Errorf("进度文件挂在 %q，期望 %q", s.journalPath, path)
	}
	if !s.persistent() {
		t.Error("指定了输出文件就必须有可续传的目标")
	}
}

// 写 stdout 没有可续传的目标，所有与进度相关的动作都要跳过，
// 而不是写一个名叫 "-.resume.json" 的文件出来。
func TestStdoutHasNoJournal(t *testing.T) {
	video := &model.Video{BVID: "BV1", PubTime: model.Time(time.Now())}
	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	s, err := openSink(cmd, &globals{}, commentsFlags{}, video)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.journalPath != "" || s.persistent() {
		t.Errorf("stdout 模式不该有进度文件，得到 %q", s.journalPath)
	}
	if err := s.saveJournal(); err != nil {
		t.Errorf("没有进度文件时保存应当是空操作：%v", err)
	}
}
