// Package output 把抓取结果写成多种格式。
//
// 包的位置在 model 与 cli 之间：它只认识归一化后的领域模型，不认识 B 站的
// 接口字段，也不认识命令行参数。加一种格式不需要动抓取逻辑，加一个抓取策略
// 也不需要动这里的任何一行。
package output

import (
	"sort"
	"strings"

	"github.com/ultramanDecker/bili-comment/internal/model"
)

// Kind 是一行的类型。
//
// 输出流里混着几种性质不同的行：视频元数据、评论、墓碑、收尾统计。
// 下游必须能一眼分辨它们，所以每种格式都要保留这个区分——
// JSONL 靠 type 字段，TSV 靠列结构（一级评论没有 root，楼中楼有），
// Markdown 靠章节。
type Kind uint8

const (
	KindVideo Kind = iota
	KindComment
	KindStub
)

// Row 是输出流里的一行。同一时刻只有一个字段非 nil。
//
// 用联合体而不是泛型或者 any：格式实现需要按类型分派，
// 而 any 会让每个实现都写一遍类型断言和「认不出来的行怎么办」。
type Row struct {
	Kind    Kind
	Video   *Video
	Comment *model.Comment
	Stub    *Stub
}

// Meta 是写入任何行之前就已知的上下文。
//
// 它存在的理由是让输出文件自解释：几个月后拿到一份 comments.tsv，
// 没有人能记起它是按热度还是按时间排的、抓的是哪个视频。这些信息不该
// 只活在命令行历史里。
type Meta struct {
	Video     *Video
	Mode      string
	FetchedAt model.Time
	// Full 为假表示头像与表情图片地址已被裁掉，schema.md 里会说明。
	Full bool

	// Resuming 表示输出文件里已经有上次写下的内容。
	//
	// 所有「开头的块」——JSONL 的视频行、TSV 的表头与注释块、Markdown 的标题——
	// 都不能再写一遍：那是重复行，恰好是续传最该避免的事。
	Resuming bool
}

// Video 是输出的第一行：视频上下文与本次抓取参数。
//
// 字段顺序即 JSONL 的键顺序，短的、用于定位视频的排在前面。
type Video struct {
	Type      string     `json:"type"`
	BVID      string     `json:"bvid"`
	AID       int64      `json:"aid"`
	Title     string     `json:"title"`
	UpMid     int64      `json:"up_mid"`
	UpName    string     `json:"up_name"`
	PubTime   model.Time `json:"pub_time"`
	StatReply int64      `json:"stat_reply"`
	Mode      string     `json:"mode"`
	FetchedAt model.Time `json:"fetched_at"`
}

// Stub 是未展开楼中楼的墓碑行。
//
// 过滤是破坏性的且对下游不可见。LLM 看到这行就知道「这楼有 37 条我没看到」，
// 结论自然会谨慎；看不到这行则会以为评论区只有它读到的那些。
// 成本是每楼一行。
type Stub struct {
	Type       string `json:"type"`
	Rpid       uint64 `json:"rpid"`
	ReplyCount int    `json:"reply_count"`
	Expanded   bool   `json:"expanded"`
	Reason     string `json:"reason"`
}

// Summary 是输出的最后一行，记录这次抓取到底拿到了多少、为什么停下。
//
// 这一行是整个输出的关键：下游拿到一份残缺数据却不知道它残缺，
// 比拿不到数据更危险——基于部分评论得出的「用户普遍认为」是纯粹的幻觉。
type Summary struct {
	Type     string `json:"type"`
	Expected int    `json:"expected"`
	Fetched  int    `json:"fetched"`
	Pages    int    `json:"pages"`
	Reason   string `json:"reason"`

	// Truncated **只**描述一级评论，不描述楼中楼——楼中楼看 Replies.Truncated。
	//
	// 判断「整份数据能不能用」时两个都要看。踩过的坑：某个格式拿它当
	// 「整体是否完整」用，于是在「一级评论抓全了、楼中楼被 --replies-top 裁掉」
	// 时输出 complete=true，而同一份文档里的嵌套字段写着 truncated=true。
	// 字段名没说清它覆盖什么，就会被当成覆盖全部。
	Truncated bool `json:"truncated,omitempty"`

	Error   string        `json:"error,omitempty"`
	Replies *ReplySummary `json:"replies,omitempty"`
}

// ReplySummary 是楼中楼的完整性元数据。
//
// 期望值与实际值分开报：Expected 是「计划展开的那些楼一共该有多少条回复」，
// Fetched 是实际拿到的。只报 Fetched 的话，抓了一半看起来也像完整。
type ReplySummary struct {
	Expected         int            `json:"expected"`
	Fetched          int            `json:"fetched"`
	Expanded         int            `json:"expanded"`
	Skipped          int            `json:"skipped,omitempty"`
	RootsWithReplies int            `json:"roots_with_replies"`
	OffsetLimited    int            `json:"offset_limited,omitempty"`
	Truncated        bool           `json:"truncated,omitempty"`
	Reasons          map[string]int `json:"skip_reasons,omitempty"`

	// SkippedReplies 是所有没展开的楼**自称**的回复数之和。
	//
	// 这个数字与 Expected 不可比，也不该比：它来自评论对象上的 reply_count，
	// 是上界（实测比真实可抓条数多 13–19%），而 Expected 来自楼中楼接口的
	// page.count。放在一起只是为了让「一共漏了多少」有个量级感。
	SkippedReplies int `json:"skipped_replies,omitempty"`
}

// All 是 Summary 里所有非零的原因计数，按数量倒序，供输出层排版。
func (s *ReplySummary) ReasonsSorted() []ReasonCount {
	out := make([]ReasonCount, 0, len(s.Reasons))
	for k, v := range s.Reasons {
		out = append(out, ReasonCount{Reason: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

// ReasonCount 是某个跳过原因及其楼数。
type ReasonCount struct {
	Reason string
	Count  int
}

// Flags 把标记拼成给人看的一行。
func Flags(cm *model.Comment) string { return strings.Join(cm.Flags, "|") }
