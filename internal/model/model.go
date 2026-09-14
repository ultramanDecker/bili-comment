// Package model 定义归一化后的领域模型。
//
// 这一层与 B 站的接口字段解耦：bilibili 包负责把平台响应翻译成这里的结构，
// output 包只认识这里的结构。平台改字段名时只需要改一个文件，
// 输出格式永远稳定。
package model

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 本包内所有时间字段统一用 Time（秒级 Unix 时间戳），理由见 Time 的注释。

// Video 是视频元数据。
type Video struct {
	BVID      string `json:"bvid"`
	AID       int64  `json:"aid"`
	Title     string `json:"title"`
	Desc      string `json:"desc,omitempty"`
	Cover     string `json:"cover,omitempty"` // slim 模式下剔除
	Up        User   `json:"up"`
	PubTime   Time   `json:"pub_time"`
	Duration  int    `json:"duration_sec"`
	Stat      Stats  `json:"stat"`
	Pages     []Page `json:"pages,omitempty"`
	FetchedAt Time   `json:"fetched_at"`
}

// User 是评论或视频的发布者。slim 模式下只保留 Mid 与 Name。
type User struct {
	Mid    int64  `json:"mid"`
	Name   string `json:"name"`
	Avatar string `json:"avatar,omitempty"` // slim 模式下剔除
	Level  int    `json:"level,omitempty"`
	IsUp   bool   `json:"is_up,omitempty"`
	IsVip  bool   `json:"is_vip,omitempty"`
}

// Stats 是视频的互动计数。
type Stats struct {
	View     int64 `json:"view"`
	Danmaku  int64 `json:"danmaku"`
	Reply    int64 `json:"reply"`
	Favorite int64 `json:"favorite"`
	Coin     int64 `json:"coin"`
	Share    int64 `json:"share"`
	Like     int64 `json:"like"`
}

// Page 是分P 信息。
type Page struct {
	CID      int64  `json:"cid"`
	Index    int    `json:"index"`
	Title    string `json:"title"`
	Duration int    `json:"duration_sec"`
}

// Comment 是一条评论。一级评论与楼中楼共用这个结构，
// 靠 Root 区分：Root 为 0 即一级评论。
//
// 字段顺序即 JSONL 输出的键顺序，把短的、常用于筛选的字段排在前面，
// 人用 less 翻看时更容易扫。
type Comment struct {
	Rpid    uint64 `json:"rpid"`
	Root    uint64 `json:"root,omitempty"`   // 所属一级评论的 rpid；0 表示自己就是一级评论
	Parent  uint64 `json:"parent,omitempty"` // 直接回复的对象；0 表示直接回复一级评论
	User    User   `json:"user"`
	Message string `json:"message"`
	Like    int    `json:"like"`
	Ctime   Time   `json:"ctime"`

	// ReplyCount 是该楼的回复总数，与实际抓到的楼中楼条数无关。
	// 未展开楼中楼时，这个数字是判断「漏了什么」的唯一依据。
	ReplyCount int `json:"reply_count,omitempty"`

	Location string `json:"location,omitempty"` // IP 属地，老评论没有这个字段

	// Rel 是相对视频发布时间的说法（「3小时」「2年」），由 annotate 包算出。
	//
	// 放在模型里而不是留给输出层现算，是因为它要出现在每一种格式里，
	// 而输出层拿不到视频发布时间之外的东西——它的每一条记录都是独立的。
	Rel string `json:"rel,omitempty"`

	Emotes   map[string]string `json:"emotes,omitempty"`   // 表情名 → 图片地址
	Pictures []string          `json:"pictures,omitempty"` // 评论配图

	IsUp  bool     `json:"is_up,omitempty"`
	IsTop bool     `json:"is_top,omitempty"`
	Flags []string `json:"flags,omitempty"` // 给 LLM 的提示标记，见 DESIGN.md
}

// Time 包装 time.Time，序列化成秒级 Unix 时间戳。
//
// 不用 RFC3339 是因为 JSONL 会被整个塞进 LLM 上下文：
// "2020-11-29T16:26:45+08:00" 是 25 个 token，"1606652805" 是 4 个。
// LLM 完全能自己转换时间戳，而评论动辄上万条，这个差距是决定性的。
type Time time.Time

func (t Time) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatInt(time.Time(t).Unix(), 10)), nil
}

func (t *Time) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("无法解析时间戳 %q：%w", s, err)
	}
	*t = Time(time.Unix(n, 0))
	return nil
}

func (t Time) IsZero() bool    { return time.Time(t).IsZero() }
func (t Time) Time() time.Time { return time.Time(t) }
func (t Time) String() string  { return time.Time(t).Format(time.RFC3339) }
