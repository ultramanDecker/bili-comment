package bilibili

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	apiNav       = "https://api.bilibili.com/x/web-interface/nav"
	apiFingerSpi = "https://api.bilibili.com/x/frontend/finger/spi"

	// DefaultUserAgent 模拟一个常见的桌面浏览器。
	DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

	// DefaultDelay 是请求间隔。社区实测连续请求超过 20 次/分钟会触发临时封禁，
	// 2.5 秒间隔约合 24 次/分钟，是「慢但稳」的取值。用户显式调高才加速。
	DefaultDelay = 2500 * time.Millisecond

	maxRetries = 3
	saltTTL    = 12 * time.Hour
	timeout    = 30 * time.Second
)

// Options 是构造 Client 的参数。
type Options struct {
	Delay     time.Duration     // 请求间隔，<=0 时用 DefaultDelay
	UserAgent string            // 为空时用 DefaultUserAgent
	Cookies   map[string]string // 初始 cookie，通常来自登录后持久化的配置
	Debug     bool
	Logf      func(format string, args ...any)
}

// Client 是带签名、限速、重试、cookie 管理的 B 站 API 客户端。
type Client struct {
	hc      *http.Client
	ua      string
	limiter *limiter
	debug   bool
	logf    func(format string, args ...any)

	mu         sync.Mutex
	cookies    map[string]string
	saltVal    string
	saltTime   time.Time
	buvidTried bool
}

// NewClient 构造客户端。若 cookie 中没有 buvid3，会在首次调用时自动获取——
// 缺少这个字段会显著提高被风控的概率。
func NewClient(opt Options) *Client {
	delay := opt.Delay
	if delay <= 0 {
		delay = DefaultDelay
	}
	ua := opt.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	logf := opt.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	cookies := map[string]string{}
	for k, v := range opt.Cookies {
		cookies[k] = v
	}
	return &Client{
		hc:      &http.Client{Timeout: timeout},
		ua:      ua,
		limiter: newLimiter(delay),
		debug:   opt.Debug,
		logf:    logf,
		cookies: cookies,
	}
}

// SetDelay 调整请求间隔，用于遇到风控时自动降速。
func (c *Client) SetDelay(d time.Duration) { c.limiter.SetInterval(d) }

// SetCookie 写入单个 cookie。
func (c *Client) SetCookie(name, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cookies[name] = value
}

// Cookies 返回当前 cookie 的快照，用于持久化。
func (c *Client) Cookies() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]string, len(c.cookies))
	for k, v := range c.cookies {
		out[k] = v
	}
	return out
}

// LoggedIn 判断当前是否持有登录态。
func (c *Client) LoggedIn() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cookies["SESSDATA"] != ""
}

// apiEnvelope 是所有 B 站接口响应的外层结构。
type apiEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// call 发起一次 API 调用并解开外层信封，返回 data 字段的原始 JSON。
// 内置限速与重试：签名失效会刷新密钥重试，风控会指数退避重试。
func (c *Client) call(ctx context.Context, rawURL string, params url.Values, signed bool) (json.RawMessage, error) {
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			d := backoffDelay(attempt)
			c.logf("重试 %d/%d，等待 %s（上次：%v）", attempt, maxRetries, d, lastErr)
			t := time.NewTimer(d)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return nil, ctx.Err()
			}
			t.Stop()
		}

		if err := c.limiter.Wait(ctx); err != nil {
			return nil, err
		}

		final := rawURL
		if signed {
			salt, err := c.salt(ctx)
			if err != nil {
				return nil, err
			}
			final = rawURL + "?" + sign(params, salt, time.Now()).Encode()
		} else if len(params) > 0 {
			final = rawURL + "?" + params.Encode()
		}

		body, err := c.httpGet(ctx, final)
		if err != nil {
			// 网络层错误通常是瞬时的，值得重试。
			lastErr = err
			continue
		}

		var env apiEnvelope
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, fmt.Errorf("解析 %s 的响应失败：%w", rawURL, err)
		}

		if env.Code != 0 {
			ae := NewAPIError(env.Code, env.Message, rawURL)
			lastErr = ae
			switch ae.Kind {
			case KindSignature:
				// 密钥可能已轮换，丢弃缓存后重试。
				c.invalidateSalt()
				continue
			case KindRateLimit:
				continue
			default:
				return nil, ae
			}
		}
		return env.Data, nil
	}

	return nil, fmt.Errorf("重试 %d 次后仍失败：%w", maxRetries, lastErr)
}

// salt 返回 WBI 签名盐值，带缓存。密钥轮换周期较长，缓存 12 小时。
func (c *Client) salt(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.saltVal != "" && time.Since(c.saltTime) < saltTTL {
		s := c.saltVal
		c.mu.Unlock()
		return s, nil
	}
	c.mu.Unlock()

	if err := c.limiter.Wait(ctx); err != nil {
		return "", err
	}
	body, err := c.httpGet(ctx, apiNav)
	if err != nil {
		return "", fmt.Errorf("获取 WBI 密钥失败：%w", err)
	}

	// 注意：未登录时 nav 返回 code=-101，但 data.wbi_img 依然存在，
	// 所以这里不检查 code，直接取字段。
	var r struct {
		Data struct {
			WbiImg struct {
				ImgURL string `json:"img_url"`
				SubURL string `json:"sub_url"`
			} `json:"wbi_img"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("解析 WBI 密钥失败：%w", err)
	}

	imgKey := keyFromURL(r.Data.WbiImg.ImgURL)
	subKey := keyFromURL(r.Data.WbiImg.SubURL)
	if len(imgKey) < 32 || len(subKey) < 32 {
		return "", errors.New("WBI 密钥格式异常，官方可能已更换签名方案")
	}
	mk := mixinKey(imgKey, subKey)
	if len(mk) != 32 {
		return "", errors.New("WBI 混合密钥生成失败")
	}

	c.mu.Lock()
	c.saltVal = mk
	c.saltTime = time.Now()
	c.mu.Unlock()

	c.logf("已刷新 WBI 密钥")
	return mk, nil
}

func (c *Client) invalidateSalt() {
	c.mu.Lock()
	c.saltVal = ""
	c.mu.Unlock()
}

// ensureBuvid 确保 cookie 中存在 buvid3。只尝试一次：
// 失败不致命，反复重试反而会拖慢正事并增加风控暴露。
func (c *Client) ensureBuvid(ctx context.Context) error {
	c.mu.Lock()
	if c.buvidTried {
		c.mu.Unlock()
		return nil
	}
	c.buvidTried = true
	_, ok := c.cookies["buvid3"]
	c.mu.Unlock()
	if ok {
		return nil
	}

	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}
	body, err := c.doGet(ctx, apiFingerSpi)
	if err != nil {
		return fmt.Errorf("获取设备指纹失败：%w", err)
	}
	var r struct {
		Data struct {
			B3 string `json:"b_3"`
			B4 string `json:"b_4"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return fmt.Errorf("解析设备指纹失败：%w", err)
	}
	if r.Data.B3 == "" {
		// 拿不到不算致命，只是被风控的概率会上升。
		c.logf("警告：未能获取 buvid3，被风控的概率会上升")
		return nil
	}
	c.SetCookie("buvid3", r.Data.B3)
	if r.Data.B4 != "" {
		c.SetCookie("buvid4", r.Data.B4)
	}
	c.logf("已获取设备指纹 buvid3")
	return nil
}

// httpGet 发起原始 GET 请求，自动带上 cookie 与 UA，并回收响应中的 Set-Cookie。
// 首次调用会顺带获取 buvid3。只应在业务调用里使用。
func (c *Client) httpGet(ctx context.Context, rawURL string) ([]byte, error) {
	if err := c.ensureBuvid(ctx); err != nil {
		return nil, err
	}
	return c.doGet(ctx, rawURL)
}

// doGet 是真正的 HTTP 请求，不做 buvid 前置检查。
// 必须与 httpGet 拆开：获取 buvid3 的请求本身要走 HTTP，
// 若它也触发 ensureBuvid 就会无限递归。
func (c *Client) doGet(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Referer", "https://www.bilibili.com/")
	req.Header.Set("Origin", "https://www.bilibili.com")
	if ck := c.cookieHeader(); ck != "" {
		req.Header.Set("Cookie", ck)
	}

	if c.debug {
		// 调试时隐藏签名值，否则日志会长得没法看。
		c.logf("GET %s", redactURL(rawURL))
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 回收服务端下发的 cookie（登录、buvid 等）。
	for _, ck := range resp.Cookies() {
		c.SetCookie(ck.Name, ck.Value)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d：%s", resp.StatusCode, truncate(string(body), 200))
	}
	return body, nil
}

func (c *Client) cookieHeader() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cookies) == 0 {
		return ""
	}
	keys := make([]string, 0, len(c.cookies))
	for k := range c.cookies {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+c.cookies[k])
	}
	return strings.Join(parts, "; ")
}

// backoffDelay 是指数退避，遇风控时逐渐拉开间隔。
func backoffDelay(attempt int) time.Duration {
	d := time.Duration(1<<uint(attempt-1)) * 2 * time.Second
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	return d
}

// redactURL 去掉 URL 中的签名与时间戳，避免调试日志被噪声淹没。
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	for _, k := range []string{"w_rid", "wts"} {
		if q.Has(k) {
			q.Set(k, "...")
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// limiter 让请求均匀分布。均匀间隔比令牌桶的突发更不容易被识别为机器人。
type limiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func newLimiter(interval time.Duration) *limiter {
	return &limiter{interval: interval}
}

func (l *limiter) SetInterval(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.interval = d
}

func (l *limiter) Wait(ctx context.Context) error {
	l.mu.Lock()
	interval := l.interval
	if interval <= 0 {
		l.mu.Unlock()
		return nil
	}
	now := time.Now()
	if l.next.IsZero() || !l.next.After(now) {
		l.next = now.Add(interval)
		l.mu.Unlock()
		return nil
	}
	wait := l.next.Sub(now)
	l.next = l.next.Add(interval)
	l.mu.Unlock()

	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Delay 返回当前的请求间隔，供上层估算耗时。
func (c *Client) Delay() time.Duration {
	c.limiter.mu.Lock()
	defer c.limiter.mu.Unlock()
	return c.limiter.interval
}
