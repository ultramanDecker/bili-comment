package bilibili

import (
	"encoding/hex"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSanitizeValue(t *testing.T) {
	cases := map[string]string{
		"a!b'c(d)e*f":       "abcdef",
		"普通文本":              "普通文本",
		"keep-dash_and.dot": "keep-dash_and.dot",
		"":                  "",
	}
	for in, want := range cases {
		if got := sanitizeValue(in); got != want {
			t.Errorf("sanitizeValue(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestKeyFromURL(t *testing.T) {
	cases := map[string]string{
		"https://i0.hdslb.com/bfs/wbi/7cd084941338484aae1ad9425b84077c.png": "7cd084941338484aae1ad9425b84077c",
		"https://i0.hdslb.com/bfs/wbi/4932caff0ff746eab6f01bf08b70ac45.png": "4932caff0ff746eab6f01bf08b70ac45",
		"": "",
	}
	for in, want := range cases {
		if got := keyFromURL(in); got != want {
			t.Errorf("keyFromURL(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// 置换表必须是 64 个下标的排列，否则 mixinKey 会取到重复字符，
// 生成的签名会被服务端拒绝，而且症状很难排查。
func TestMixinKeyEncTabIsPermutation(t *testing.T) {
	seen := make(map[int]bool, 64)
	for _, idx := range mixinKeyEncTab {
		if idx < 0 || idx >= 64 {
			t.Fatalf("置换表含越界下标 %d", idx)
		}
		if seen[idx] {
			t.Fatalf("置换表含重复下标 %d", idx)
		}
		seen[idx] = true
	}
	if len(seen) != 64 {
		t.Fatalf("置换表覆盖 %d 个下标，期望 64", len(seen))
	}
}

// mixinKey 用的是固定置换表，输出字符在原文中的位置并非单调，
// 所以这里只验证结构性质：长度、字符来源、无重复、确定性。
// 置换表本身是否正确只能靠真实接口的签名校验来证明（见 live_test.go）。
func TestMixinKey(t *testing.T) {
	// 64 个互不相同的字符，这样任何下标重复都会暴露成结果里的重复字符。
	const raw = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ!@"
	imgKey, subKey := raw[:32], raw[32:]

	got := mixinKey(imgKey, subKey)
	if len(got) != 32 {
		t.Fatalf("mixinKey 长度 = %d，期望 32", len(got))
	}

	seen := make(map[rune]bool, 32)
	for _, c := range got {
		if !strings.ContainsRune(raw, c) {
			t.Fatalf("结果含原文之外的字符 %q：%q", c, got)
		}
		if seen[c] {
			t.Fatalf("结果含重复字符 %q，说明置换表有重复下标：%q", c, got)
		}
		seen[c] = true
	}

	if mixinKey(imgKey, subKey) != got {
		t.Error("相同输入应当得到相同结果")
	}

	// 交换 imgKey 与 subKey 会得到不同的拼接串，结果必须不同。
	if mixinKey(subKey, imgKey) == got {
		t.Error("交换 imgKey 与 subKey 后结果不应相同")
	}

	if mixinKey("tooshort", "alsoshort") != "" {
		t.Error("密钥过短时应当返回空串而非崩溃或截断结果")
	}
}

func TestSignProducesRequiredParams(t *testing.T) {
	params := url.Values{
		"oid":  {"80433022"},
		"type": {"1"},
		"mode": {"3"},
	}
	now := time.Unix(1735689600, 0) // 固定时间，保证结果可复现
	signed := sign(params, "0123456789abcdef0123456789abcdef", now)

	if got := signed.Get("wts"); got != "1735689600" {
		t.Errorf("wts = %q，期望 1735689600", got)
	}
	rid := signed.Get("w_rid")
	if len(rid) != 32 {
		t.Fatalf("w_rid 长度 = %d，期望 32（MD5 十六进制）：%q", len(rid), rid)
	}
	if _, err := hex.DecodeString(rid); err != nil {
		t.Errorf("w_rid 不是合法十六进制：%v", err)
	}
	// 原始参数必须保留。
	if signed.Get("oid") != "80433022" || signed.Get("type") != "1" {
		t.Error("签名不应丢失原始参数")
	}
}

// 签名必须对参数顺序不敏感，但对值敏感。
func TestSignIsOrderIndependent(t *testing.T) {
	salt := "0123456789abcdef0123456789abcdef"
	now := time.Unix(1735689600, 0)

	a := url.Values{"oid": {"1"}, "type": {"1"}, "mode": {"3"}}
	b := url.Values{"mode": {"3"}, "type": {"1"}, "oid": {"1"}}

	if sign(a, salt, now).Get("w_rid") != sign(b, salt, now).Get("w_rid") {
		t.Error("参数顺序不应影响签名")
	}

	c := url.Values{"oid": {"2"}, "type": {"1"}, "mode": {"3"}}
	if sign(a, salt, now).Get("w_rid") == sign(c, salt, now).Get("w_rid") {
		t.Error("参数值改变时签名必须改变")
	}
}
