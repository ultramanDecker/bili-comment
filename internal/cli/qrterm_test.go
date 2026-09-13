package cli

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	qrcode "github.com/skip2/go-qrcode"
)

// 渲染错了的二维码在终端上看起来同样「像那么回事」，只有扫不动才知道出问题。
// 所以这里把渲染结果反解回点阵，和编码器给出的原始点阵逐格比对——
// 极性、行配对、静区任意一处出错都会立刻暴露。

func TestRenderQRRoundTrip(t *testing.T) {
	const content = "https://www.bilibili.com/h5/account-h5/auth?qrcode_key=abcdef012345&navhide=1"

	var sb strings.Builder
	if err := renderQR(&sb, content); err != nil {
		t.Fatalf("renderQR 失败：%v", err)
	}

	want, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		t.Fatalf("生成参照二维码失败：%v", err)
	}
	bits := want.Bitmap()
	if len(bits)%2 == 1 {
		bits = append(bits, make([]bool, len(bits[0])))
	}

	got := decodeQRArt(t, sb.String())

	if len(got) != len(bits) {
		t.Fatalf("反解出 %d 行，期望 %d 行", len(got), len(bits))
	}
	for y := range bits {
		if len(got[y]) != len(bits[y]) {
			t.Fatalf("第 %d 行有 %d 列，期望 %d 列", y, len(got[y]), len(bits[y]))
		}
		for x := range bits[y] {
			if got[y][x] != bits[y][x] {
				t.Fatalf("第 %d 行第 %d 列不符：渲染为 %v，编码器给出 %v",
					y, x, got[y][x], bits[y][x])
			}
		}
	}
}

// 静区是规范要求的一部分，缺了它不少扫码器会识别失败。
func TestRenderQRKeepsQuietZone(t *testing.T) {
	var sb strings.Builder
	if err := renderQR(&sb, "https://example.com/"); err != nil {
		t.Fatalf("renderQR 失败：%v", err)
	}
	rows := decodeQRArt(t, sb.String())

	// 四角必须全白：既是静区，也顺带证明没有把整幅图上下或左右翻转。
	for _, y := range []int{0, len(rows) - 1} {
		for x := range rows[y] {
			if rows[y][x] {
				t.Fatalf("第 %d 行第 %d 列应为静区（白），实际为黑", y, x)
			}
		}
	}
	for y := range rows {
		for _, x := range []int{0, len(rows[y]) - 1} {
			if rows[y][x] {
				t.Fatalf("第 %d 行第 %d 列应为静区（白），实际为黑", y, x)
			}
		}
	}
}

// 二维码一旦宽过终端就会折行，折行后的图案必定扫不出来。
// 这里用真实的登录地址长度（约 131 字符）作为上限，长度涨上去时测试会先报警。
func TestRenderQRFitsInTerminal(t *testing.T) {
	// 与真实登录二维码等长：轮询凭据是 32 位十六进制。
	const content = "https://www.bilibili.com/h5/account-h5/auth?qrcode_key=0123456789abcdef0123456789abcdef&navhide=1&from_spmid=tm.recommend.0.0"
	if len(content) < 120 {
		t.Fatalf("样本长度 %d 已不代表真实二维码，请更新", len(content))
	}

	var sb strings.Builder
	if err := renderQR(&sb, content); err != nil {
		t.Fatalf("renderQR 失败：%v", err)
	}
	rows := decodeQRArt(t, sb.String())

	// 80 列是终端的事实标准；再宽就有折行风险。
	const maxCols = 80
	cols := len(rows[0])
	if cols > maxCols {
		t.Errorf("二维码宽 %d 列，超过 %d 列的终端宽度，会折行导致无法扫描", cols, maxCols)
	}

	// 模块数是奇数，渲染时会补一行白模块凑成偶数，所以行数是宽度或宽度加一。
	// 超出这个范围说明点阵被截断或串行了。
	if got := len(rows); got != cols && got != cols+1 {
		t.Errorf("二维码不是正方形：%d 行 × %d 列（行数应等于列数或列数加一）", got, cols)
	}
}

// 每行必须以 reset 收尾，否则颜色会渗到后续输出里。
func TestRenderQREndsEachLineWithReset(t *testing.T) {
	var sb strings.Builder
	if err := renderQR(&sb, "https://example.com/"); err != nil {
		t.Fatalf("renderQR 失败：%v", err)
	}
	for i, line := range strings.Split(strings.TrimSuffix(sb.String(), "\n"), "\n") {
		if !strings.HasSuffix(line, "\x1b[0m") {
			t.Errorf("第 %d 行没有以 reset 收尾：%q", i, line)
		}
	}
}

// decodeQRArt 把渲染结果反解回点阵。true 表示黑模块，
// 与 qrcode.QRCode.Bitmap 的语义一致。
func decodeQRArt(t *testing.T, art string) [][]bool {
	t.Helper()

	var rows [][]bool
	for _, line := range strings.Split(strings.TrimSuffix(art, "\n"), "\n") {
		var top, bottom []bool
		fg, bg := 37, 47 // 与 renderQR 的初始默认一致：白底白字

		for i := 0; i < len(line); {
			if strings.HasPrefix(line[i:], "\x1b[") {
				end := strings.IndexByte(line[i:], 'm')
				if end < 0 {
					t.Fatalf("转义序列没有结束符：%q", line[i:])
				}
				var err error
				fg, bg, err = parseSGR(line[i+2 : i+end])
				if err != nil {
					t.Fatalf("%v（序列 %q）", err, line[i:i+end+1])
				}
				i += end + 1
				continue
			}

			r, size := utf8.DecodeRuneInString(line[i:])
			if r != '▀' {
				t.Fatalf("出现非预期字符 %q，渲染应当只用半块字符", r)
			}
			// 前景色画上半格、背景色画下半格。
			top = append(top, fg == 30)
			bottom = append(bottom, bg == 40)
			i += size
		}

		if len(top) == 0 {
			t.Fatalf("空行：%q", line)
		}
		rows = append(rows, top, bottom)
	}
	if len(rows) == 0 {
		t.Fatal("没有任何输出")
	}
	return rows
}

func parseSGR(spec string) (fg, bg int, err error) {
	fg, bg = 37, 47
	if spec == "0" {
		return fg, bg, nil
	}
	for _, p := range strings.Split(spec, ";") {
		n, convErr := strconv.Atoi(p)
		if convErr != nil {
			return 0, 0, convErr
		}
		switch n {
		case 30, 37:
			fg = n
		case 40, 47:
			bg = n
		default:
			return 0, 0, &unexpectedSGR{n}
		}
	}
	return fg, bg, nil
}

type unexpectedSGR struct{ n int }

func (e *unexpectedSGR) Error() string {
	return "渲染只应使用黑白色（SGR " + strconv.Itoa(e.n) + "），否则终端主题会影响二维码极性"
}
