package bilibili

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
)

// 扫码登录与账号信息。
//
// 登录链路和抓取链路是两类流量，节奏完全不同：抓取是 2.5 秒一次的重请求，
// 而扫码轮询是固定 2 秒一次的极短请求。共用同一个限速器会让扫码体验变得很糟
// （每步都要多等一个抓取间隔），所以这里直接走 doGet，请求节奏由调用方的
// ticker 控制——同时也意味着调用方有责任不要打得太快。

const (
	apiQRGenerate = "https://passport.bilibili.com/x/passport-login/web/qrcode/generate"
	apiQRPoll     = "https://passport.bilibili.com/x/passport-login/web/qrcode/poll"
)

// 轮询响应的状态码在 data.code 里，外层 code 恒为 0——
// 只看外层会永远以为「等待扫码」。
const (
	qrWaiting = 86101 // 尚未扫码
	qrScanned = 86090 // 已扫码，等待手机端确认
	qrOK      = 0     // 确认成功
	qrExpired = 86038 // 二维码已过期
)

// LoginState 是一次扫码轮询的结果。
type LoginState int

const (
	LoginWaiting LoginState = iota
	LoginScanned
	LoginSuccess
	LoginExpired
)

// qrState 把服务端状态码映射成 LoginState。
// 单独抽出来是因为这张表写错极难发现：把 86101 和 86090 弄反，
// 表现只是「一直显示等待扫码」，看上去和用户没扫码一模一样。
func qrState(code int) (LoginState, bool) {
	switch code {
	case qrWaiting:
		return LoginWaiting, true
	case qrScanned:
		return LoginScanned, true
	case qrOK:
		return LoginSuccess, true
	case qrExpired:
		return LoginExpired, true
	}
	return LoginWaiting, false
}

func (s LoginState) String() string {
	switch s {
	case LoginWaiting:
		return "等待扫码"
	case LoginScanned:
		return "已扫码，等待手机端确认"
	case LoginSuccess:
		return "登录成功"
	case LoginExpired:
		return "二维码已过期"
	}
	return "未知状态"
}

// QRCode 是一次登录会话的二维码。
type QRCode struct {
	URL string // 编码进二维码的内容，由 B 站 App 解析
	Key string // 轮询凭据，不对外展示
}

// QRLogin 申请一个登录二维码。URL 需要渲染成二维码供用户扫描。
func (c *Client) QRLogin(ctx context.Context) (*QRCode, error) {
	// 先拿设备指纹再登录：登录态与 buvid3 绑定，缺指纹的会话在后续接口上
	// 更容易被判定为异常。拿不到不算致命，只是风控概率上升。
	if err := c.ensureBuvid(ctx); err != nil {
		c.logf("警告：%v（继续登录）", err)
	}

	body, err := c.doGet(ctx, apiQRGenerate)
	if err != nil {
		return nil, fmt.Errorf("申请二维码失败：%w", err)
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			URL string `json:"url"`
			Key string `json:"qrcode_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("解析二维码响应失败：%w", err)
	}
	if env.Code != 0 {
		return nil, NewAPIError(env.Code, "", apiQRGenerate)
	}
	if env.Data.URL == "" || env.Data.Key == "" {
		return nil, errors.New("二维码响应缺少 url 或 qrcode_key，接口可能已变更")
	}
	return &QRCode{URL: env.Data.URL, Key: env.Data.Key}, nil
}

// PollQR 查询一次扫码状态。返回 LoginSuccess 时，登录 cookie 已经写入客户端，
// 调用方只需把它持久化。
func (c *Client) PollQR(ctx context.Context, key string) (LoginState, error) {
	if key == "" {
		return LoginWaiting, errors.New("轮询凭据为空")
	}
	body, err := c.doGet(ctx, apiQRPoll+"?qrcode_key="+url.QueryEscape(key))
	if err != nil {
		return LoginWaiting, err
	}

	var env struct {
		Code int `json:"code"`
		Data struct {
			URL     string `json:"url"` // 成功时是带回调参数的跨域地址
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return LoginWaiting, fmt.Errorf("解析扫码状态失败：%w", err)
	}

	state, ok := qrState(env.Data.Code)
	if !ok {
		return LoginWaiting, fmt.Errorf("未知的扫码状态 %d：%s", env.Data.Code, env.Data.Message)
	}
	if state != LoginSuccess {
		return state, nil
	}

	if err := c.absorbLoginCookies(env.Data.URL); err != nil {
		return LoginWaiting, err
	}
	// Set-Cookie 已由 doGet 回收，跨域地址里的参数也已并入，
	// 两处都没有 SESSDATA 说明这次登录没有真正生效——
	// 报成功却存下一组空 cookie，问题要到下次抓取才会暴露。
	if !c.LoggedIn() {
		return LoginWaiting, errors.New("服务端报告登录成功，但未取到 SESSDATA")
	}
	return LoginSuccess, nil
}

// absorbLoginCookies 从登录回调地址的查询参数里提取 cookie。
// 这条路径不能省：跨域回调携带的字段比轮询响应的 Set-Cookie 更全，
// 只靠后者偶尔会缺 DedeUserID。
func (c *Client) absorbLoginCookies(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("解析登录回调地址失败：%w", err)
	}
	n := 0
	for k, vs := range u.Query() {
		if k == "gourl" || len(vs) == 0 || vs[0] == "" {
			continue // gourl 是登录后的跳转目标，不是 cookie
		}
		c.SetCookie(k, vs[0])
		n++
	}
	if n == 0 {
		c.logf("警告：登录回调地址中没有携带 cookie，只能依赖 Set-Cookie")
	}
	return nil
}

// Account 是当前登录账号的信息。
type Account struct {
	Mid    int64  `json:"mid"`
	Name   string `json:"name"`
	Avatar string `json:"avatar,omitempty"`
	Level  int    `json:"level"`
	Vip    bool   `json:"vip"`
}

// WhoAmI 查询当前登录账号。未登录或登录态失效时返回 KindNeedLogin 的错误。
func (c *Client) WhoAmI(ctx context.Context) (*Account, error) {
	data, err := c.call(ctx, apiNav, nil, false)
	if err != nil {
		return nil, err
	}
	var r struct {
		IsLogin   bool   `json:"isLogin"`
		Mid       int64  `json:"mid"`
		Uname     string `json:"uname"`
		Face      string `json:"face"`
		VipStatus int    `json:"vipStatus"`
		LevelInfo struct {
			CurrentLevel int `json:"current_level"`
		} `json:"level_info"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("解析账号信息失败：%w", err)
	}
	// 未登录时 nav 返回 code=-101，走不到这里；但服务端也出现过 code=0
	// 且 isLogin=false 的响应，一并按未登录处理。
	if !r.IsLogin || r.Mid == 0 {
		return nil, NewAPIError(-101, "未登录或登录态已失效", apiNav)
	}
	return &Account{
		Mid:    r.Mid,
		Name:   r.Uname,
		Avatar: r.Face,
		Level:  r.LevelInfo.CurrentLevel,
		Vip:    r.VipStatus > 0,
	}, nil
}
