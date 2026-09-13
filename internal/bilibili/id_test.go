package bilibili

import (
	"errors"
	"testing"
)

// 这些对应关系是用真实接口核对过的，不能靠「自洽」证明——
// 错误的进制同样能做出可逆的转换，但产出的 BV 号会被服务端拒绝。
var knownPairs = []struct {
	bv  string
	aid int64
}{
	{"BV1GJ411x7h7", 80433022},
	{"BV17x411w7KC", 170001},
}

func TestBvToAv(t *testing.T) {
	for _, tc := range knownPairs {
		got, err := BvToAv(tc.bv)
		if err != nil {
			t.Fatalf("BvToAv(%q) 返回错误：%v", tc.bv, err)
		}
		if got != tc.aid {
			t.Errorf("BvToAv(%q) = %d，期望 %d", tc.bv, got, tc.aid)
		}
	}
}

func TestAvToBv(t *testing.T) {
	for _, tc := range knownPairs {
		got := AvToBv(tc.aid)
		if got != tc.bv {
			t.Errorf("AvToBv(%d) = %q，期望 %q", tc.aid, got, tc.bv)
		}
	}
}

func TestBvAvRoundTrip(t *testing.T) {
	for _, aid := range []int64{1, 2, 170001, 80433022, 1130000000} {
		bv := AvToBv(aid)
		if len(bv) != 12 {
			t.Fatalf("AvToBv(%d) 长度 = %d，期望 12：%q", aid, len(bv), bv)
		}
		back, err := BvToAv(bv)
		if err != nil {
			t.Fatalf("BvToAv(%q) 返回错误：%v", bv, err)
		}
		if back != aid {
			t.Errorf("往返失败：%d → %q → %d", aid, bv, back)
		}
	}
}

// 超出 6 位 58 进制表示范围的 aid 必须被拒绝，而不是静默截断成
// 一个指向其它视频的合法 BV 号。
func TestAvToBvRejectsOutOfRange(t *testing.T) {
	for _, aid := range []int64{0, -1, 1 << 40, 999999999999} {
		if got := AvToBv(aid); got != "" {
			t.Errorf("AvToBv(%d) = %q，期望空串", aid, got)
		}
	}
	if got := AvToBv(1130000000); got == "" {
		t.Error("当前量级的 aid 不应被拒绝")
	}
}

func TestBvToAvRejectsBadInput(t *testing.T) {
	for _, bad := range []string{"", "BV1GJ411x7h", "BV1GJ411x7h77", "BV1GJ411x7h!"} {
		if _, err := BvToAv(bad); err == nil {
			t.Errorf("BvToAv(%q) 应当报错，却成功了", bad)
		}
	}
}

func TestParseRef(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantBv  string
		wantAid int64
		wantPg  int
	}{
		{"裸 BV 号", "BV1GJ411x7h7", "BV1GJ411x7h7", 80433022, 1},
		{"小写 av 号", "av170001", "BV17x411w7KC", 170001, 1},
		{"大写 AV 号", "AV170001", "BV17x411w7KC", 170001, 1},
		{"纯数字", "170001", "BV17x411w7KC", 170001, 1},
		{"BV 前后有空白", "  BV1GJ411x7h7  ", "BV1GJ411x7h7", 80433022, 1},
		{"完整 URL", "https://www.bilibili.com/video/BV1GJ411x7h7", "BV1GJ411x7h7", 80433022, 1},
		{"带分P 的 URL", "https://www.bilibili.com/video/BV1GJ411x7h7?p=3", "BV1GJ411x7h7", 80433022, 3},
		{"URL 带其它参数", "https://www.bilibili.com/video/BV1GJ411x7h7?spm_id_from=333&p=2&vd_source=abc", "BV1GJ411x7h7", 80433022, 2},
		{"av 形式的 URL", "https://www.bilibili.com/video/av170001", "BV17x411w7KC", 170001, 1},
		{"手机端 URL", "https://m.bilibili.com/video/BV1GJ411x7h7", "BV1GJ411x7h7", 80433022, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := ParseRef(tc.input)
			if err != nil {
				t.Fatalf("ParseRef(%q) 返回错误：%v", tc.input, err)
			}
			if ref.BVID != tc.wantBv {
				t.Errorf("BVID = %q，期望 %q", ref.BVID, tc.wantBv)
			}
			if ref.AID != tc.wantAid {
				t.Errorf("AID = %d，期望 %d", ref.AID, tc.wantAid)
			}
			if ref.Page != tc.wantPg {
				t.Errorf("Page = %d，期望 %d", ref.Page, tc.wantPg)
			}
		})
	}
}

func TestParseRefErrors(t *testing.T) {
	for _, bad := range []string{"", "   ", "hehe", "0", "-5", "999999999999999999999"} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q) 应当报错，却成功了", bad)
		}
	}

	// 短链必须由调用方发请求解析，不能静默当成普通输入处理。
	if _, err := ParseRef("https://b23.tv/abcdefg"); !errors.Is(err, ErrNeedResolve) {
		t.Errorf("短链应当返回 ErrNeedResolve，实际：%v", err)
	}
}

// BV 号不应从更长的字符串里截出半截。
func TestParseRefDoesNotMatchPartialBv(t *testing.T) {
	if _, err := ParseRef("BV1GJ411x7h7extra"); err == nil {
		t.Error("超长的 BV 串不应被截断匹配")
	}
}
