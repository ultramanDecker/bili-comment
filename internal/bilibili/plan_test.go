package bilibili

import (
	"testing"

	"github.com/ultramanDecker/bili-comment/internal/model"
)

func mkRoot(rpid uint64, replies, likes int) *model.Comment {
	return &model.Comment{Rpid: rpid, ReplyCount: replies, Like: likes}
}

// expandSet 把计划里要展开的楼收成集合，方便断言。
func expandSet(p *ReplyPlan) map[uint64]bool {
	out := map[uint64]bool{}
	for _, it := range p.Items {
		if it.Expand {
			out[it.Root] = true
		}
	}
	return out
}

func skipOf(p *ReplyPlan, root uint64) SkipReason {
	for _, it := range p.Items {
		if it.Root == root {
			return it.Skip
		}
	}
	return "?"
}

// 默认策略必须全展开：工具不替用户做数据质量取舍。
func TestBuildPlanDefaultExpandsAll(t *testing.T) {
	roots := []*model.Comment{
		mkRoot(1, 5, 10),
		mkRoot(2, 0, 999), // 没有回复
		mkRoot(3, 100, 0), // 回复多但点赞为 0
	}
	p := BuildPlan(roots, ReplyPolicy{})

	if p.ExpandCount() != 2 {
		t.Errorf("展开 %d 楼，期望 2 楼", p.ExpandCount())
	}
	got := expandSet(p)
	if !got[1] || !got[3] {
		t.Errorf("展开集合 = %v，期望含 1 与 3", got)
	}
	if got[2] {
		t.Error("没有回复的楼不该展开")
	}
	if skipOf(p, 2) != SkipNoReplies {
		t.Errorf("2 号楼的跳过原因 = %q，期望 %q", skipOf(p, 2), SkipNoReplies)
	}
}

// 没有回复的楼不产生墓碑行——那里本来就没有东西可丢，多一行只是噪声。
func TestBuildPlanNoStubForEmptyThread(t *testing.T) {
	roots := []*model.Comment{mkRoot(1, 0, 10), mkRoot(2, 5, 10)}
	p := BuildPlan(roots, ReplyPolicy{})

	if p.StubCount() != 0 {
		t.Errorf("墓碑数 = %d，期望 0", p.StubCount())
	}
}

func TestBuildPlanThresholds(t *testing.T) {
	roots := []*model.Comment{
		mkRoot(1, 5, 100), // 点赞够，回复不够
		mkRoot(2, 50, 1),  // 回复够，点赞不够
		mkRoot(3, 50, 100),
	}

	p := BuildPlan(roots, ReplyPolicy{MinLikes: 10, MinCount: 10})
	got := expandSet(p)
	if len(got) != 1 || !got[3] {
		t.Errorf("展开集合 = %v，期望只有 3", got)
	}
	if skipOf(p, 1) != SkipThreshold || skipOf(p, 2) != SkipThreshold {
		t.Error("未达阈值的楼应记为 below_threshold")
	}
	if p.StubCount() != 2 {
		t.Errorf("墓碑数 = %d，期望 2", p.StubCount())
	}
}

// 强制保留是设计里明文要求的逃生口：UP 主参与的对话往往点赞很低，
// 却恰恰是「UP 怎么回应质疑」这类问题的唯一答案，不能被阈值误杀。
func TestBuildPlanForcedIncludesBeatThresholds(t *testing.T) {
	up := mkRoot(1, 5, 0)
	up.IsUp = true
	top := mkRoot(2, 5, 0)
	top.IsTop = true
	plain := mkRoot(3, 5, 0)

	p := BuildPlan([]*model.Comment{up, top, plain},
		ReplyPolicy{MinLikes: 1000, IncludeUp: true, IncludeTop: true})

	got := expandSet(p)
	if !got[1] || !got[2] {
		t.Errorf("UP 主与置顶应被强制展开，实际 %v", got)
	}
	if got[3] {
		t.Error("普通楼未达阈值，不该展开")
	}
}

// 强制保留同样要能穿过 --replies-top 的排名裁剪，
// 否则「最热的 N 楼」会把 UP 主那楼挤掉，逃生口形同虚设。
func TestBuildPlanForcedIncludesBeatTopN(t *testing.T) {
	up := mkRoot(1, 5, 0) // 回复数最少，排名垫底
	up.IsUp = true
	roots := []*model.Comment{
		up,
		mkRoot(2, 500, 0),
		mkRoot(3, 400, 0),
		mkRoot(4, 300, 0),
	}

	p := BuildPlan(roots, ReplyPolicy{Top: 2, IncludeUp: true})
	got := expandSet(p)

	if !got[1] {
		t.Error("UP 主的楼被 --replies-top 裁掉了，强制保留失效")
	}
	if !got[2] || !got[3] {
		t.Errorf("最热的 2 楼应展开，实际 %v", got)
	}
	if got[4] {
		t.Error("第 4 楼回复数排第 3，不该展开")
	}
}

func TestBuildPlanTopNPicksByReplyCount(t *testing.T) {
	roots := []*model.Comment{
		mkRoot(1, 10, 0),
		mkRoot(2, 900, 0),
		mkRoot(3, 50, 0),
	}
	p := BuildPlan(roots, ReplyPolicy{Top: 1})

	got := expandSet(p)
	if len(got) != 1 || !got[2] {
		t.Errorf("展开集合 = %v，期望只有回复数最多的 2 号楼", got)
	}
	if skipOf(p, 1) != SkipTopN {
		t.Errorf("1 号楼的跳过原因 = %q，期望 %q", skipOf(p, 1), SkipTopN)
	}
}

// 回复数相同时必须有稳定的次级排序键。否则同一份数据两次运行可能排出不同的
// 顺序，续传时进度下标就会指向另一栋楼，静默地漏抓或重抓。
func TestBuildPlanTopNIsDeterministicOnTies(t *testing.T) {
	mk := func() []*model.Comment {
		return []*model.Comment{
			mkRoot(30, 100, 0),
			mkRoot(10, 100, 0),
			mkRoot(20, 100, 0),
			mkRoot(40, 100, 0),
		}
	}
	first := expandSet(BuildPlan(mk(), ReplyPolicy{Top: 2}))
	for i := 0; i < 20; i++ {
		if got := expandSet(BuildPlan(mk(), ReplyPolicy{Top: 2})); len(got) != len(first) {
			t.Fatalf("第 %d 次运行结果不同：%v vs %v", i, got, first)
		} else {
			for k := range first {
				if !got[k] {
					t.Fatalf("第 %d 次运行选出了不同的楼：%v vs %v", i, got, first)
				}
			}
		}
	}
	// 并列时按 rpid 升序，取最小的两个。
	if !first[10] || !first[20] {
		t.Errorf("并列时应按 rpid 升序取前 2 个，实际 %v", first)
	}
}

func TestBuildPlanDisabled(t *testing.T) {
	roots := []*model.Comment{mkRoot(1, 100, 100), mkRoot(2, 0, 0)}
	p := BuildPlan(roots, ReplyPolicy{Disabled: true})

	if p.ExpandCount() != 0 {
		t.Errorf("--no-replies 时不该展开任何楼，实际 %d", p.ExpandCount())
	}
	if skipOf(p, 1) != SkipDisabled {
		t.Errorf("跳过原因 = %q，期望 %q", skipOf(p, 1), SkipDisabled)
	}
	// 两栋楼各写一行墓碑，说明「这里被整体关掉了」。
	if p.StubCount() != 1 {
		t.Errorf("墓碑数 = %d，期望 1（没有回复的那栋不算）", p.StubCount())
	}
}

func TestReplyPolicyIsDefault(t *testing.T) {
	if !(ReplyPolicy{}).IsDefault() {
		t.Error("零值策略应视为默认全展开")
	}
	for _, p := range []ReplyPolicy{
		{Disabled: true},
		{Top: 1},
		{MinLikes: 1},
		{MinCount: 1},
	} {
		if p.IsDefault() {
			t.Errorf("%+v 不该被当成默认策略，否则不会写墓碑行", p)
		}
	}
	// 强制保留不影响「是否默认」的判断：它只增不减，不会丢数据。
	if !(ReplyPolicy{IncludeUp: true, IncludeTop: true}).IsDefault() {
		t.Error("只开强制保留仍是全展开，应算默认")
	}
}

// 指纹用于校验续传时参数没变。任何一个会影响计划的字段变了，
// 指纹就必须变——否则续传会按旧计划的下标去写新计划的数据。
func TestPolicyFingerprintCoversAllFields(t *testing.T) {
	base := ReplyPolicy{}
	seen := map[string]bool{base.Fingerprint(): true}
	for _, p := range []ReplyPolicy{
		{Disabled: true},
		{Top: 1},
		{MinLikes: 1},
		{MinCount: 1},
		{IncludeUp: true},
		{IncludeTop: true},
	} {
		fp := p.Fingerprint()
		if seen[fp] {
			t.Errorf("%+v 的指纹与已有策略重复，续传会误判参数没变", p)
		}
		seen[fp] = true
	}
}

func TestPlanItemsKeepRootOrder(t *testing.T) {
	roots := []*model.Comment{mkRoot(7, 1, 0), mkRoot(3, 1, 0), mkRoot(9, 1, 0)}
	p := BuildPlan(roots, ReplyPolicy{})

	for i, want := range []uint64{7, 3, 9} {
		if p.Items[i].Root != want {
			t.Errorf("第 %d 项 = %d，期望 %d（计划必须保持一级评论的原始顺序）", i, p.Items[i].Root, want)
		}
	}
}
