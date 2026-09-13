// Package model 定义归一化后的领域模型。
//
// 这一层与 B 站的接口字段解耦：bilibili 包负责把平台响应翻译成这里的结构，
// output 包只认识这里的结构。平台改字段名时只需要改一个文件，
// 输出格式永远稳定。
package model

import "time"

// Video 是视频元数据。
type Video struct {
	BVID      string    `json:"bvid"`
	AID       int64     `json:"aid"`
	Title     string    `json:"title"`
	Desc      string    `json:"desc,omitempty"`
	Cover     string    `json:"cover,omitempty"` // slim 模式下剔除
	Up        User      `json:"up"`
	PubTime   time.Time `json:"pub_time"`
	Duration  int       `json:"duration_sec"`
	Stat      Stats     `json:"stat"`
	Pages     []Page    `json:"pages,omitempty"`
	FetchedAt time.Time `json:"fetched_at"`
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
