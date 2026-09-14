package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func tmpOutput(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "comments.jsonl")
}

func TestLoadMissingJournal(t *testing.T) {
	_, err := LoadJournal(tmpOutput(t))
	if !errors.Is(err, ErrNoJournal) {
		t.Fatalf("期望 ErrNoJournal，实际 %v", err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	out := tmpOutput(t)
	j := &Journal{AID: 123, BVID: "BV1", Phase: PhaseReplies, Offset: 4096, PlanIndex: 17}
	if err := j.Save(out); err != nil {
		t.Fatalf("保存失败：%v", err)
	}

	got, err := LoadJournal(out)
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	if got.AID != 123 || got.Phase != PhaseReplies || got.Offset != 4096 || got.PlanIndex != 17 {
		t.Errorf("读回的内容 = %+v", got)
	}
	if got.Version != journalVersion {
		t.Errorf("版本 = %d，期望 %d", got.Version, journalVersion)
	}
	if got.UpdatedAt == "" {
		t.Error("应当记录更新时间")
	}
}

// 进度文件被挪用是最危险的情况之一：把另一个输出文件的进度套上来，
// 会按错误的偏移截断，直接毁掉已有数据。
// 累计量必须和 Offset 一起存下来。续传时若只拿回偏移、拿不回这些数字，
// 写出的 summary 会少报已经抓到的部分——而它看上去像个正常的完整结论。
func TestReplyCountersRoundTrip(t *testing.T) {
	out := tmpOutput(t)
	j := &Journal{
		AID: 1, Phase: PhaseReplies, Offset: 8192, PlanIndex: 2,
		ReplyFetched: 2167, ReplyExpected: 6644, ReplyOffsetLimited: 1, ReplyTruncated: true,
	}
	if err := j.Save(out); err != nil {
		t.Fatal(err)
	}

	got, err := LoadJournal(out)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReplyFetched != 2167 || got.ReplyExpected != 6644 {
		t.Errorf("回复计数 = %d/%d，期望 2167/6644", got.ReplyFetched, got.ReplyExpected)
	}
	if !got.ReplyTruncated {
		t.Error("截断标记丢了")
	}
	if got.ReplyOffsetLimited != 1 {
		t.Errorf("超限楼数 = %d，期望 1", got.ReplyOffsetLimited)
	}
	if got.Offset != 8192 || got.PlanIndex != 2 {
		t.Errorf("偏移或下标丢了：offset=%d plan_index=%d", got.Offset, got.PlanIndex)
	}
}

// 墓碑行是否已落盘必须记下来，不能靠 PlanIndex 推断。
// 「第一楼抓到一半时中断」是最常见的断点：那时墓碑行已经写进文件了，
// 而 PlanIndex 还是 0。按 PlanIndex 判断就会把它们再写一遍。
func TestStubsWrittenIsIndependentOfPlanIndex(t *testing.T) {
	out := tmpOutput(t)
	j := &Journal{AID: 1, Phase: PhaseReplies, Offset: 6941, PlanIndex: 0, StubsWritten: true}
	if err := j.Save(out); err != nil {
		t.Fatal(err)
	}

	got, err := LoadJournal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !got.StubsWritten {
		t.Error("墓碑行的落盘标记丢了，续传会把它们再写一遍")
	}
	if got.PlanIndex != 0 {
		t.Errorf("PlanIndex = %d，期望 0（还没有任何一栋楼抓完）", got.PlanIndex)
	}

	// 反向：全新的进度里这个标记必须是 false，否则第一次抓就不写墓碑行了。
	fresh := tmpOutput(t)
	if err := (&Journal{AID: 1, Phase: PhaseReplies}).Save(fresh); err != nil {
		t.Fatal(err)
	}
	fj, err := LoadJournal(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if fj.StubsWritten {
		t.Error("全新抓取不该被认为已经写过墓碑行")
	}
}

// 一级评论阶段的进度文件里不该出现楼中楼的计数——
// 那些字段只对 PhaseReplies 有意义，混进来会让「这份文件描述的是哪个阶段」
// 变得含糊。omitempty 保证了零值不落盘。
func TestRootsPhaseJournalOmitsReplyCounters(t *testing.T) {
	out := tmpOutput(t)
	j := &Journal{AID: 1, Phase: PhaseRoots, Offset: 100, RootCursor: "abc"}
	if err := j.Save(out); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(JournalPath(out))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"reply_fetched", "reply_expected", "reply_truncated", "reply_offset_limited"} {
		if bytes.Contains(data, []byte(key)) {
			t.Errorf("一级评论阶段的进度文件里不该有 %q：%s", key, data)
		}
	}
	if !bytes.Contains(data, []byte("root_cursor")) {
		t.Errorf("游标应当被记录：%s", data)
	}
}

func TestLoadRejectsForeignJournal(t *testing.T) {
	a := filepath.Join(t.TempDir(), "a.jsonl")
	b := filepath.Join(t.TempDir(), "b.jsonl")

	j := &Journal{AID: 1, Offset: 100}
	if err := j.Save(a); err != nil {
		t.Fatal(err)
	}
	// 把 a 的进度文件挪到 b 旁边。
	data, err := os.ReadFile(JournalPath(a))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(JournalPath(b), data, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadJournal(b); err == nil {
		t.Fatal("来自别的输出文件的进度应当被拒绝")
	}
}

// 读不懂的版本必须拒绝，而不是按当前语义去猜——
// 猜错的后果是按错误的位置截断输出。
func TestLoadRejectsUnknownVersion(t *testing.T) {
	out := tmpOutput(t)
	body := `{"version": 999, "output": ` + quote(out) + `, "offset": 0}`
	if err := os.WriteFile(JournalPath(out), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadJournal(out); err == nil {
		t.Fatal("未知版本应当被拒绝")
	}
}

func TestLoadRejectsCorruptJournal(t *testing.T) {
	out := tmpOutput(t)
	if err := os.WriteFile(JournalPath(out), []byte("{这不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadJournal(out); err == nil {
		t.Fatal("损坏的进度文件应当报错，而不是当成没有进度")
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	out := tmpOutput(t)
	if err := Remove(out); err != nil {
		t.Fatalf("删除不存在的进度文件不该报错：%v", err)
	}
	j := &Journal{AID: 1}
	if err := j.Save(out); err != nil {
		t.Fatal(err)
	}
	if err := Remove(out); err != nil {
		t.Fatalf("删除失败：%v", err)
	}
	if _, err := os.Stat(JournalPath(out)); !errors.Is(err, os.ErrNotExist) {
		t.Error("进度文件应当已被删除")
	}
}

// 续传的第一步：把上次中断时写了一半的那栋楼截掉。
func TestTruncateOutputDropsPartialTail(t *testing.T) {
	out := tmpOutput(t)
	if err := os.WriteFile(out, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := TruncateOutput(out, 4)
	if err != nil {
		t.Fatalf("截断失败：%v", err)
	}
	// 句柄必须已经定位在截断处，后续写入才是接着写的。
	if _, err := f.WriteString("XY"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "0123XY" {
		t.Errorf("内容 = %q，期望 %q", got, "0123XY")
	}
}

func TestTruncateOutputAllowsNoOp(t *testing.T) {
	out := tmpOutput(t)
	if err := os.WriteFile(out, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := TruncateOutput(out, 3)
	if err != nil {
		t.Fatalf("截断到文件末尾应当成功：%v", err)
	}
	f.Close()
}

// 文件比进度记录的短，说明它被外部改动过。此时「前缀已完整」这个前提不成立，
// 截断只会把已有数据毁掉——必须拒绝，而不是硬着头皮继续。
func TestTruncateOutputRefusesShrunkFile(t *testing.T) {
	out := tmpOutput(t)
	if err := os.WriteFile(out, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := TruncateOutput(out, 999); err == nil {
		t.Fatal("输出文件短于进度记录时应当拒绝续传")
	}
	// 拒绝之后原有内容必须原封不动。
	got, _ := os.ReadFile(out)
	if string(got) != "abc" {
		t.Errorf("内容被改动了：%q", got)
	}
}

func TestEnsureDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a", "b", "c.jsonl")
	if err := EnsureDir(target); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	if fi, err := os.Stat(filepath.Dir(target)); err != nil || !fi.IsDir() {
		t.Errorf("目录没有建出来：%v", err)
	}
	// 没有目录部分时不该报错。
	if err := EnsureDir("bare.jsonl"); err != nil {
		t.Errorf("裸文件名不该报错：%v", err)
	}
}

func quote(s string) string {
	b := make([]byte, 0, len(s)+2)
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' || s[i] == '"' {
			b = append(b, '\\')
		}
		b = append(b, s[i])
	}
	return string(append(b, '"'))
}
