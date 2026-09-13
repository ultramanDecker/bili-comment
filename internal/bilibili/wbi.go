package bilibili

import (
	"crypto/md5"
	"encoding/hex"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// B 站的 WBI 签名。官方随时可能更换置换表或流程，所以整个机制被隔离在这个文件里，
// 出问题时只需要改这里。参考实现散见社区逆向文章，字段语义以实测为准。

// mixinKeyEncTab 是官方的固定置换表：把 imgKey+subKey 拼接后的 64 个字符按此顺序重排，
// 取前 32 位作为签名盐值。
var mixinKeyEncTab = [64]int{
	46, 47, 18, 2, 53, 8, 23, 32, 15, 50, 10, 31, 58, 3, 45, 35, 27, 43, 5, 49,
	33, 9, 42, 19, 29, 28, 14, 39, 12, 38, 41, 13, 37, 48, 7, 16, 24, 55, 40,
	61, 26, 17, 0, 1, 60, 51, 30, 4, 22, 25, 54, 21, 56, 59, 6, 63, 57, 62, 11,
	36, 20, 34, 44, 52,
}

// mixinKey 由 imgKey 与 subKey 派生出签名盐值。入参必须是各 32 字符的原始 key。
func mixinKey(imgKey, subKey string) string {
	orig := imgKey + subKey
	if len(orig) < 64 {
		return ""
	}
	var b strings.Builder
	b.Grow(64)
	for _, idx := range mixinKeyEncTab {
		b.WriteByte(orig[idx])
	}
	// 只取前 32 位作为盐值。
	return b.String()[:32]
}

// keyFromURL 从 wbi_img 的 URL 中取出文件名部分（去掉扩展名），即真正的 key。
// 例如 https://i0.hdslb.com/bfs/wbi/7cd084941338484aae1ad9425b84077c.png
// 取出 7cd084941338484aae1ad9425b84077c
func keyFromURL(raw string) string {
	if raw == "" {
		return ""
	}
	base := path.Base(raw)
	return strings.TrimSuffix(base, path.Ext(base))
}

// sign 为参数集计算 w_rid 并写入 wts。返回可直接用于 URL query 的 Values。
//
// 流程：附加 wts → 过滤值中的 !'()* → 按 key 升序 → urlencode → 拼盐值取 MD5。
func sign(params url.Values, salt string, now time.Time) url.Values {
	signed := url.Values{}
	for k, vs := range params {
		for _, v := range vs {
			signed.Add(k, sanitizeValue(v))
		}
	}
	signed.Set("wts", strconv.FormatInt(now.Unix(), 10))

	// url.Values.Encode 内部按 key 升序排列，与官方要求一致。
	query := signed.Encode()
	sum := md5.Sum([]byte(query + salt))
	signed.Set("w_rid", hex.EncodeToString(sum[:]))
	return signed
}

// sanitizeValue 过滤掉官方签名前会剔除的特殊字符。注意：只过滤值，不过滤键。
func sanitizeValue(v string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '!', '\'', '(', ')', '*':
			return -1
		}
		return r
	}, v)
}
