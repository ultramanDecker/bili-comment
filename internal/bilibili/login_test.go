package bilibili

import "testing"

func TestQRState(t *testing.T) {
	cases := map[int]LoginState{
		86101: LoginWaiting,
		86090: LoginScanned,
		0:     LoginSuccess,
		86038: LoginExpired,
	}
	for code, want := range cases {
		got, ok := qrState(code)
		if !ok {
			t.Errorf("qrState(%d) 报告为未知状态", code)
			continue
		}
		if got != want {
			t.Errorf("qrState(%d) = %v，期望 %v", code, got, want)
		}
	}

	// 未列出的状态码必须报告为「未知」，不能默默当成等待扫码——
	// 那样接口变更会表现为一直卡在扫码界面。
	for _, code := range []int{-1, 1, 86200} {
		if _, ok := qrState(code); ok {
			t.Errorf("qrState(%d) 不应被识别", code)
		}
	}
}

// 登录成功后 cookie 主要来自回调地址的查询参数，
// 这条解析错了会存下一组残缺的登录态。
func TestAbsorbLoginCookies(t *testing.T) {
	const callback = "https://passport.biligame.com/crossDomain" +
		"?DedeUserID=1234567&DedeUserID__ckMd5=abcdef0123456789" +
		"&Expires=1780000000&SESSDATA=deadbeef%2C1780000000%2Cabc12%2A31" +
		"&bili_jct=0123456789abcdef0123456789abcdef" +
		"&gourl=https%3A%2F%2Fwww.bilibili.com%2F"

	c := NewClient(Options{})
	if err := c.absorbLoginCookies(callback); err != nil {
		t.Fatalf("absorbLoginCookies 失败：%v", err)
	}

	got := c.Cookies()
	want := map[string]string{
		"DedeUserID":        "1234567",
		"DedeUserID__ckMd5": "abcdef0123456789",
		"Expires":           "1780000000",
		// SESSDATA 里的逗号和星号是 cookie 值的一部分，必须原样保留。
		"SESSDATA": "deadbeef,1780000000,abc12*31",
		"bili_jct": "0123456789abcdef0123456789abcdef",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("cookie %s = %q，期望 %q", k, got[k], v)
		}
	}

	// gourl 是登录后的跳转目标，不是 cookie，混进去会污染请求头。
	if _, ok := got["gourl"]; ok {
		t.Error("gourl 不应被当作 cookie 保存")
	}

	if !c.LoggedIn() {
		t.Error("吸收了 SESSDATA 之后应当处于已登录状态")
	}
}

func TestAbsorbLoginCookiesEmpty(t *testing.T) {
	c := NewClient(Options{})
	if err := c.absorbLoginCookies(""); err != nil {
		t.Errorf("空地址不应报错：%v", err)
	}
	if c.LoggedIn() {
		t.Error("没有 cookie 时不应判定为已登录")
	}
}

func TestLoginStateString(t *testing.T) {
	// 状态文案会直接显示给用户，空串或占位符会让扫码过程看起来卡死。
	for _, s := range []LoginState{LoginWaiting, LoginScanned, LoginSuccess, LoginExpired} {
		if s.String() == "" {
			t.Errorf("状态 %d 没有可读文案", int(s))
		}
	}
	if LoginState(99).String() == "" {
		t.Error("未知状态也应当有文案")
	}
}
