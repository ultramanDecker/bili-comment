package bilibili

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/ultramanDecker/bili-comment/internal/model"
)

// 决策点：一级评论抓完之后、楼中楼开抓之前，决定哪些楼要展开。
//
// 这是整个抓取流程里唯一有实质取舍的地方。楼中楼的请求数等于「要展开的楼数」
// 乘以每楼的页数，而回复量极度集中在少数热门评论下——不展开长尾能砍掉
// 60–70% 的请求。这一点无法靠「先全抓再本地过滤」实现，因为全抓正是要避免的成本。
//
// 默认全展开。选择性展开是用户显式要求的性能逃生口，工具不替用户做数据质量取舍。

// SkipReason 说明某楼为什么没被展开。它会写进输出的墓碑行里，
// 让下游知道「这里有东西没看到」以及为什么。
type SkipReason string

const (
	SkipNoReplies SkipReason = "no_replies"       // 这楼本来就没有回复
	SkipDisabled  SkipReason = "replies_disabled" // 用户整体关掉了楼中楼
	SkipThreshold SkipReason = "below_threshold"  // 未达到点赞数或回复数阈值
	SkipTopN      SkipReason = "not_in_top_n"     // 回复数排名在 --replies-top 之外
)

// ReplyPolicy 是用户对楼中楼展开范围的取舍。
type ReplyPolicy struct {
	Disabled   bool // 完全不展开（--no-replies）
	Top        int  // 只展开回复数最多的 N 楼，0 表示不限
	MinLikes   int  // 只展开点赞数 >= N 的楼
	MinCount   int  // 只展开回复数 >= N 的楼
	IncludeUp  bool // 强制展开 UP 主自己发的一级评论
	IncludeTop bool // 强制展开置顶评论
}

// IsDefault 判断策略是否是「全展开」。
// 全展开时不需要墓碑行——没有东西被丢掉，多写一行只是噪声。
func (p ReplyPolicy) IsDefault() bool {
	return !p.Disabled && p.Top == 0 && p.MinLikes == 0 && p.MinCount == 0
}

// Fingerprint 生成策略的指纹，用于校验续传时参数没有变过。
// 计划依赖策略，策略一变计划就变，已完成的进度下标会指向另一栋楼。
func (p ReplyPolicy) Fingerprint() string {
	var b strings.Builder
	fmt.Fprintf(&b, "d=%t|top=%d|likes=%d|count=%d|up=%t|topt=%t",
		p.Disabled, p.Top, p.MinLikes, p.MinCount, p.IncludeUp, p.IncludeTop)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

// PlanItem 是计划中的一栋楼。
type PlanItem struct {
	Root       uint64
	ReplyCount int
	Likes      int
	Expand     bool
	Skip       SkipReason // Expand 为 false 时说明原因
}

// ReplyPlan 是决策点的输出：按一级评论的原始顺序列出每栋楼怎么处理。
//
// 顺序必须稳定——输出按这个顺序写，续传也按这个顺序记进度下标。
type ReplyPlan struct {
	Items       []PlanItem
	Fingerprint string
}

// ExpandCount 返回计划要展开的楼数。
func (p *ReplyPlan) ExpandCount() int {
	n := 0
	for _, it := range p.Items {
		if it.Expand {
			n++
		}
	}
	return n
}

// BuildPlan 按策略决定每栋楼展不展开。
//
// 强制保留（UP 主 / 置顶）优先于一切阈值和排名：设计上任何阈值过滤都必须留逃生口，
// 否则会精确地丢掉最有信号的部分——UP 主参与的对话往往点赞不高，
// 却恰恰是「UP 怎么回应质疑」这类问题的唯一答案。
func BuildPlan(roots []*model.Comment, p ReplyPolicy) *ReplyPlan {
	plan := &ReplyPlan{Fingerprint: p.Fingerprint()}

	for _, r := range roots {
		it := PlanItem{Root: r.Rpid, ReplyCount: r.ReplyCount, Likes: r.Like}

		switch {
		case r.ReplyCount <= 0:
			// 本来就没回复，不算丢弃，墓碑行也省了。
			it.Skip = SkipNoReplies
		case p.Disabled:
			it.Skip = SkipDisabled
		case (p.IncludeUp && r.IsUp) || (p.IncludeTop && r.IsTop):
			it.Expand = true
		case p.MinLikes > 0 && r.Like < p.MinLikes:
			it.Skip = SkipThreshold
		case p.MinCount > 0 && r.ReplyCount < p.MinCount:
			it.Skip = SkipThreshold
		default:
			it.Expand = true
		}

		plan.Items = append(plan.Items, it)
	}

	applyTopN(plan, roots, p)
	return plan
}

// applyTopN 执行 --replies-top 的排名裁剪。
//
// 排名按回复数降序，回复数相同时按 rpid 升序——必须有这个次级排序键，
// 否则相等元素的相对顺序取决于排序算法，续传时同一个下标可能指向不同的楼。
func applyTopN(plan *ReplyPlan, roots []*model.Comment, p ReplyPolicy) {
	if p.Top <= 0 {
		return
	}

	type cand struct {
		pos   int
		root  uint64
		count int
	}
	var cands []cand
	for i, it := range plan.Items {
		if !it.Expand {
			continue
		}
		// 强制保留的楼不参与排名裁剪。
		r := roots[i]
		if (p.IncludeUp && r.IsUp) || (p.IncludeTop && r.IsTop) {
			continue
		}
		cands = append(cands, cand{pos: i, root: it.Root, count: it.ReplyCount})
	}

	sort.Slice(cands, func(a, b int) bool {
		if cands[a].count != cands[b].count {
			return cands[a].count > cands[b].count
		}
		return cands[a].root < cands[b].root
	})

	for i, c := range cands {
		if i < p.Top {
			continue
		}
		plan.Items[c.pos].Expand = false
		plan.Items[c.pos].Skip = SkipTopN
	}
}

// StubCount 返回会产生墓碑行的楼数。
// 没有回复的楼不算——那里本来就没有东西可丢。
func (p *ReplyPlan) StubCount() int {
	n := 0
	for _, it := range p.Items {
		if !it.Expand && it.Skip != SkipNoReplies {
			n++
		}
	}
	return n
}
