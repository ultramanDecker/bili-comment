package bilibili

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// BV 号与 AV 号的互转，以及各种输入形式的解析。
// 目标是「直接粘贴浏览器地址栏就能跑」——这是好用与难用的分界线。

const (
	bvXor int64 = 177451812
	bvAdd int64 = 8728348608
)

// bvTable 是 BV 号的 58 进制字符表。
const bvTable = "fZodR9XQDSUm21yCkr6zBqiveYah8bt4xsWpHnJE7jL5VG3guMTKNPAwcF"

// bvPos 是 BV 号中承载数值的字符位置。
var bvPos = [6]int{11, 10, 3, 8, 4, 6}

// pow58 是 58 的幂，预先算好避免浮点运算带来的精度问题。
// 进制是 58（与字符表长度一致）而非 36——用 36 的话 6 位数装不下
// xor/add 后的数值域，会得出看似合理但完全错误的 BV 号。
var pow58 = [6]int64{1, 58, 3364, 195112, 11316496, 656356768}

// bvMaxValue 是 6 位 58 进制能表示的最大值。超出这个范围的 aid 无法编码成 BV 号；
// B 站当前的 aid 量级（约十亿）远小于此，这里属于防御性检查——
// 越界时静默截断会产出一个格式合法但指向错误视频的 BV 号，非常难排查。
var bvMaxValue = func() int64 {
	var sum int64
	for _, p := range pow58 {
		sum += p
	}
	return sum*57 + 56
}()

var bvIndex = func() map[byte]int64 {
	m := make(map[byte]int64, len(bvTable))
	for i := 0; i < len(bvTable); i++ {
		m[bvTable[i]] = int64(i)
	}
	return m
}()

// BV 号必须整体匹配，避免从更长的字符串里截出半截。
var bvRe = regexp.MustCompile(`(?:^|[^0-9A-Za-z])(BV[0-9A-Za-z]{10})(?:[^0-9A-Za-z]|$)`)

var avRe = regexp.MustCompile(`(?i)(?:^|[^0-9A-Za-z])av(\d+)(?:[^0-9]|$)`)

// ErrNeedResolve 表示输入是短链，必须发一次请求跟随跳转才能拿到视频标识。
var ErrNeedResolve = errors.New("需要跟随短链跳转")

// VideoRef 是解析后的视频定位信息。
type VideoRef struct {
	BVID string
	AID  int64
	Page int // 分P 序号，从 1 开始
}

// BvToAv 把 BV 号转成 AV 号。
func BvToAv(bv string) (int64, error) {
	if len(bv) != 12 {
		return 0, fmt.Errorf("BV 号长度应为 12，实际 %d：%q", len(bv), bv)
	}
	var r int64
	for i, pos := range bvPos {
		v, ok := bvIndex[bv[pos]]
		if !ok {
			return 0, fmt.Errorf("BV 号含非法字符 %q：%q", bv[pos], bv)
		}
		r += v * pow58[i]
	}
	// 合法的 BV 号必然满足 r >= bvAdd，因为 r = (aid ^ xor) + add 且 aid > 0。
	// 不满足说明这不是一个真实的 BV 号。
	if r < bvAdd {
		return 0, fmt.Errorf("BV 号数值域异常：%q", bv)
	}
	return (r - bvAdd) ^ bvXor, nil
}

// AvToBv 把 AV 号转成 BV 号。aid 超出 BV 格式的表示范围时返回空串，
// 由调用方决定如何报告——静默截断会得到指向错误视频的合法 BV 号。
func AvToBv(aid int64) string {
	if aid <= 0 {
		return ""
	}
	x := (aid ^ bvXor) + bvAdd
	if x > bvMaxValue {
		return ""
	}
	out := []byte("BV1  4 1 7  ")
	for i, pos := range bvPos {
		out[pos] = bvTable[(x/pow58[i])%58]
	}
	return string(out)
}

// ParseRef 解析用户输入为视频定位信息。接受 BV 号、AV 号、纯数字、完整 URL
// （含带分P 的 URL）。短链返回 ErrNeedResolve，由调用方发请求解析。
func ParseRef(input string) (*VideoRef, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return nil, errors.New("输入为空")
	}

	if strings.Contains(s, "b23.tv") {
		return nil, ErrNeedResolve
	}

	// 先从 URL 里取分P 参数，取不到就默认第 1 P。
	page := 1
	if u, err := url.Parse(s); err == nil {
		if p := u.Query().Get("p"); p != "" {
			if n, err := strconv.Atoi(p); err == nil && n > 0 {
				page = n
			}
		}
	}

	if m := bvRe.FindStringSubmatch(s); m != nil {
		aid, err := BvToAv(m[1])
		if err != nil {
			return nil, err
		}
		return &VideoRef{BVID: m[1], AID: aid, Page: page}, nil
	}

	if m := avRe.FindStringSubmatch(s); m != nil {
		aid, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("AV 号超出数值范围：%q", m[1])
		}
		return refFromAID(aid, page)
	}

	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return refFromAID(n, page)
	}

	return nil, fmt.Errorf("无法识别的视频标识：%q\n支持 BV 号、av 号、完整 URL、b23.tv 短链", input)
}

// refFromAID 由 AV 号构造定位信息，并校验其能表示为 BV 号。
func refFromAID(aid int64, page int) (*VideoRef, error) {
	bv := AvToBv(aid)
	if bv == "" {
		return nil, fmt.Errorf("AV 号 %d 不是有效的视频号", aid)
	}
	return &VideoRef{BVID: bv, AID: aid, Page: page}, nil
}
