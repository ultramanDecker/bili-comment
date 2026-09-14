package cli

import (
	"testing"
	"time"

	"github.com/ultramanDecker/bili-comment/internal/bilibili"
	"github.com/ultramanDecker/bili-comment/internal/model"
)

// mkPlan 按给定的每楼回复数造一个全展开的计划。
func mkPlan(replyCounts ...int) *bilibili.ReplyPlan {
	roots := make([]*model.Comment, 0, len(replyCounts))
	for i, n := range replyCounts {
		roots = append(roots, &model.Comment{Rpid: uint64(i + 1), ReplyCount: n})
	}
	return bilibili.BuildPlan(roots, bilibili.ReplyPolicy{})
}

func etaWith(t *testing.T, d time.Duration, replyCounts ...int) string {
	t.Helper()
	return eta(mkPlan(replyCounts...), 0, bilibili.NewClient(bilibili.Options{Delay: d}))
}

// 估算就是「页数 × 请求间隔」。一楼 2128 条回复要翻 107 页，
// 在 1 秒间隔下是 107 秒——不是「1 楼 × 1 秒」。
func TestEtaCountsPagesNotRoots(t *testing.T) {
	// 2128 条 → ceil(2128/20) = 107 页 → 107 秒。
	if got := etaWith(t, time.Second, 2128); got != "2 分钟" {
		t.Errorf("耗时 = %q，期望 %q", got, "2 分钟")
	}
}

// 页数要向上取整，否则 21 条会被算成一页。
func TestEtaRoundsPagesUp(t *testing.T) {
	// 21 条 → 2 页 → 2 秒。
	if got := etaWith(t, time.Second, 21); got != "3 秒" {
		t.Errorf("耗时 = %q，期望 %q", got, "3 秒")
	}
}

// 超过 offset 上限的部分抓不到，不应该计入耗时——否则会给用户一个
// 永远等不到的预期。单楼最多 251 页。
func TestEtaClampsAtOffsetLimit(t *testing.T) {
	c := bilibili.NewClient(bilibili.Options{Delay: time.Second})
	// 5020 条正好是 251 页，即上限本身。
	atCap := eta(mkPlan(bilibili.ReplyPageSize*bilibili.MaxReplyPages), 0, c)
	huge := eta(mkPlan(1_000_000), 0, c)

	if huge != atCap {
		t.Errorf("超限的楼耗时 = %q，应与刚好触顶的 %q 相同", huge, atCap)
	}
	// 251 页 × 1 秒 ≈ 4 分钟，绝不是「1 万分钟」。
	if huge != "5 分钟" {
		t.Errorf("耗时 = %q，期望被上限截在 %q", huge, "5 分钟")
	}
}

// 只算要展开的楼。被 --replies-top 裁掉的楼一个请求都不会发。
func TestEtaSkipsUnexpanded(t *testing.T) {
	roots := []*model.Comment{
		{Rpid: 1, ReplyCount: 1000},
		{Rpid: 2, ReplyCount: 1000},
		{Rpid: 3, ReplyCount: 1000},
	}
	plan := bilibili.BuildPlan(roots, bilibili.ReplyPolicy{Top: 1})

	// 只有 1 楼要展开：50 页 → 50 秒。
	if got := eta(plan, 0, bilibili.NewClient(bilibili.Options{Delay: time.Second})); got != "51 秒" {
		t.Errorf("耗时 = %q，期望 %q", got, "51 秒")
	}
}

// 续传时只该报「还剩多久」，已经抓完的楼不算进去。
func TestEtaRespectsStartIndex(t *testing.T) {
	c := bilibili.NewClient(bilibili.Options{Delay: time.Second})
	plan := mkPlan(1000, 1000) // 每楼 50 页

	full := eta(plan, 0, c)
	resumed := eta(plan, 1, c)
	if full == resumed {
		t.Fatalf("续传的耗时应当更短，两次都是 %q", full)
	}
	if resumed != "51 秒" {
		t.Errorf("续传耗时 = %q，期望 %q（只剩第二楼）", resumed, "51 秒")
	}
}

// 间隔为 0 时无从估算，要如实说不知道，而不是报一个 0 秒。
//
// NewClient 会把 <=0 的间隔换成默认值，所以这条路只能由 SetDelay 走出来
// ——它是公开方法，将来用于遇风控自动降速，可能被传进 0。
func TestEtaUnknownWithoutDelay(t *testing.T) {
	c := bilibili.NewClient(bilibili.Options{Delay: time.Second})
	c.SetDelay(0)

	if got := eta(mkPlan(100), 0, c); got != "未知" {
		t.Errorf("耗时 = %q，期望 %q", got, "未知")
	}
}
