package bilibili

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ultramanDecker/bili-comment/internal/config"
)

// 联网冒烟测试，默认跳过。启用方式：
//
//	BILI_LIVE=1 go test ./internal/bilibili -run Live -v
//
// 为什么值得保留一个联网测试：WBI 签名的正确性无法用单元测试证明——
// 置换表错一个下标，本地算出来的 w_rid 照样是 32 位合法十六进制，
// 只有服务端接受它才算数。BV/AV 换算同理：错误的进制也能做出可逆转换。

const liveBVID = "BV1GJ411x7h7"

func liveClient(t *testing.T) *Client {
	t.Helper()
	if os.Getenv("BILI_LIVE") == "" {
		t.Skip("跳过联网测试（设置 BILI_LIVE=1 启用）")
	}
	// 带上本地登录态：未登录时服务端会把评论列表截断成几条，
	// 测试跑在未登录状态下就验证不到真实行为。
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("读取配置失败：%v", err)
	}
	if cfg.Cookies["SESSDATA"] == "" {
		t.Log("提示：未检测到登录态，评论相关的断言可能因服务端截断而失真")
	}
	// 请求间隔取保守值，避免因为跑测试把自己送进风控。
	return NewClient(Options{
		Delay:   3 * time.Second,
		Cookies: cfg.Cookies,
		Logf:    func(format string, args ...any) { t.Logf(format, args...) },
	})
}

// 用真实接口验证 WBI 签名。签名错误时服务端返回 -403。
func TestLiveWBISignature(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()

	params := url.Values{}
	params.Set("bvid", liveBVID)

	data, err := c.call(ctx, "https://api.bilibili.com/x/web-interface/wbi/view", params, true)
	if err != nil {
		t.Fatalf("WBI 签名请求失败：%v", err)
	}

	var v struct {
		BVID string `json:"bvid"`
		AID  int64  `json:"aid"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if v.BVID != liveBVID {
		t.Errorf("返回的 bvid = %q，期望 %q", v.BVID, liveBVID)
	}
}

// 用真实接口验证 BV↔AV 换算的双向一致性。
func TestLiveBvAvMatchesAPI(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()

	for _, tc := range knownPairs {
		t.Run(tc.bv, func(t *testing.T) {
			// 按 BV 查询，核对服务端给出的 aid 与本地换算一致。
			byBv, err := c.Video(ctx, &VideoRef{BVID: tc.bv})
			if err != nil {
				t.Fatalf("按 BV 查询失败：%v", err)
			}
			if byBv.AID != tc.aid {
				t.Errorf("BvToAv 结果不符：服务端 %d，本地 %d", byBv.AID, tc.aid)
			}

			// 按 aid 查询（BVID 留空即走 aid 分支），核对服务端给出的 BV。
			byAid, err := c.Video(ctx, &VideoRef{AID: tc.aid})
			if err != nil {
				t.Fatalf("按 aid 查询失败：%v", err)
			}
			if byAid.BVID != tc.bv {
				t.Errorf("AvToBv 结果不符：服务端 %q，本地 %q", byAid.BVID, tc.bv)
			}
		})
	}
}

// 验证元数据接口在真实响应下能正确落到领域模型。
func TestLiveVideoMetadata(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()

	v, err := c.Video(ctx, &VideoRef{BVID: liveBVID})
	if err != nil {
		t.Fatalf("获取元数据失败：%v", err)
	}
	if v.Title == "" {
		t.Error("标题为空")
	}
	if v.Up.Mid == 0 || v.Up.Name == "" {
		t.Errorf("UP 主信息缺失：%+v", v.Up)
	}
	if v.PubTime.IsZero() {
		t.Error("发布时间为空")
	}
	if v.Stat.View == 0 {
		t.Error("播放数为 0，多半是字段没解析对")
	}
	if len(v.Pages) == 0 {
		t.Error("分P 列表为空")
	}
	t.Logf("%s / %s / 播放 %d / 评论 %d",
		v.BVID, v.Title, v.Stat.View, v.Stat.Reply)
}

// 验证扫码登录的两个接口都能通。
//
// 这里只走到「等待扫码」为止：真正完成登录需要人工扫码，
// 不适合放进自动化测试。但这一步已经能证明二维码申请与轮询的
// 请求格式、状态码解析路径是对的——剩下的失败面只有人为因素。
func TestLiveQRLogin(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()

	qr, err := c.QRLogin(ctx)
	if err != nil {
		t.Fatalf("申请二维码失败：%v", err)
	}
	if qr.URL == "" || qr.Key == "" {
		t.Fatalf("二维码信息不完整：%+v", qr)
	}
	// 二维码内容里应当带上轮询凭据，否则 App 扫了也无法把这次扫码关联回来。
	if !strings.Contains(qr.URL, qr.Key) {
		t.Errorf("二维码内容 %q 中没有轮询凭据 %q", qr.URL, qr.Key)
	}

	state, err := c.PollQR(ctx, qr.Key)
	if err != nil {
		t.Fatalf("轮询扫码状态失败：%v", err)
	}
	if state != LoginWaiting {
		t.Errorf("刚申请的二维码状态 = %v，期望 %v（没人扫码）", state, LoginWaiting)
	}
	t.Logf("二维码已申请，内容长度 %d", len(qr.URL))
}

// 验证各种输入形式最终都能定位到同一个视频。
func TestLiveResolveRef(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()

	inputs := []string{
		liveBVID,
		"https://www.bilibili.com/video/" + liveBVID,
		"https://www.bilibili.com/video/" + liveBVID + "?p=1&t=10",
		"av80433022",
	}
	for _, in := range inputs {
		ref, err := c.ResolveRef(ctx, in)
		if err != nil {
			t.Errorf("ResolveRef(%q) 失败：%v", in, err)
			continue
		}
		if ref.BVID != liveBVID {
			t.Errorf("ResolveRef(%q) 得到 %q，期望 %q", in, ref.BVID, liveBVID)
		}
	}
}
