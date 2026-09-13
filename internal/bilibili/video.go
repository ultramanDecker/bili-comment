package bilibili

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ultramanDecker/bili-comment/internal/model"
)

const apiView = "https://api.bilibili.com/x/web-interface/view"

// apiVideoView 是 /x/web-interface/view 的响应结构。
type apiVideoView struct {
	BVID     string `json:"bvid"`
	AID      int64  `json:"aid"`
	Title    string `json:"title"`
	Desc     string `json:"desc"`
	Pic      string `json:"pic"`
	Pubdate  int64  `json:"pubdate"`
	Duration int    `json:"duration"`
	Owner    struct {
		Mid  int64  `json:"mid"`
		Name string `json:"name"`
		Face string `json:"face"`
	} `json:"owner"`
	Stat struct {
		View     int64 `json:"view"`
		Danmaku  int64 `json:"danmaku"`
		Reply    int64 `json:"reply"`
		Favorite int64 `json:"favorite"`
		Coin     int64 `json:"coin"`
		Share    int64 `json:"share"`
		Like     int64 `json:"like"`
	} `json:"stat"`
	Pages []struct {
		CID      int64  `json:"cid"`
		Page     int    `json:"page"`
		Part     string `json:"part"`
		Duration int    `json:"duration"`
	} `json:"pages"`
}

func (r *apiVideoView) toModel() *model.Video {
	v := &model.Video{
		BVID:     r.BVID,
		AID:      r.AID,
		Title:    r.Title,
		Desc:     r.Desc,
		Cover:    r.Pic,
		Up:       model.User{Mid: r.Owner.Mid, Name: r.Owner.Name, Avatar: r.Owner.Face},
		PubTime:  time.Unix(r.Pubdate, 0),
		Duration: r.Duration,
		Stat: model.Stats{
			View:     r.Stat.View,
			Danmaku:  r.Stat.Danmaku,
			Reply:    r.Stat.Reply,
			Favorite: r.Stat.Favorite,
			Coin:     r.Stat.Coin,
			Share:    r.Stat.Share,
			Like:     r.Stat.Like,
		},
		FetchedAt: time.Now(),
	}
	for _, p := range r.Pages {
		v.Pages = append(v.Pages, model.Page{
			CID:      p.CID,
			Index:    p.Page,
			Title:    p.Part,
			Duration: p.Duration,
		})
	}
	return v
}

// Video 拉取视频元数据。
func (c *Client) Video(ctx context.Context, ref *VideoRef) (*model.Video, error) {
	params := url.Values{}
	if ref.BVID != "" {
		params.Set("bvid", ref.BVID)
	} else {
		params.Set("aid", strconv.FormatInt(ref.AID, 10))
	}

	data, err := c.call(ctx, apiView, params, false)
	if err != nil {
		return nil, err
	}

	var raw apiVideoView
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("解析视频信息失败：%w", err)
	}
	return raw.toModel(), nil
}

// ResolveRef 解析用户输入。短链（b23.tv）会跟随一次跳转。
func (c *Client) ResolveRef(ctx context.Context, input string) (*VideoRef, error) {
	ref, err := ParseRef(input)
	if err == nil {
		return ref, nil
	}
	if !errors.Is(err, ErrNeedResolve) {
		return nil, err
	}

	final, err := c.ResolveShortLink(ctx, input)
	if err != nil {
		return nil, err
	}
	c.logf("短链跳转到 %s", final)
	return ParseRef(final)
}

// ResolveShortLink 跟随跳转并返回最终 URL。
// http.Client 默认就会跟随 302，最终地址在 resp.Request.URL 上。
func (c *Client) ResolveShortLink(ctx context.Context, raw string) (string, error) {
	target := strings.TrimSpace(raw)
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = "https://" + target
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", c.ua)

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("解析短链失败：%w", err)
	}
	defer resp.Body.Close()

	final := resp.Request.URL.String()
	if !strings.Contains(final, "bilibili.com") {
		return "", fmt.Errorf("短链跳转到了非 B 站地址：%s", final)
	}
	return final, nil
}
