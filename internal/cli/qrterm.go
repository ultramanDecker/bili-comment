package cli

import (
	"fmt"
	"io"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// 二维码的终端渲染。
//
// 一个半块字符占一列一格，但字符格子高约为宽的两倍，所以用 "▀" 把上下两个
// 模块压进一格：它的前景色画上半格、背景色画下半格。这样渲染出来的二维码
// 在纵向拉长的字体下依然是正方模块，扫码器才能正确定位。
//
// 这里显式指定黑白两色（ANSI 30/37 与 40/47），而不是依赖终端默认配色：
// 深色主题下默认前景色是亮的，直接用半块字符会渲染成「黑模块=亮」，
// 二维码整体极性反转。多数手机能识别反色二维码，但并非全部，
// 不值得为了省几个字节去赌。
func renderQR(w io.Writer, content string) error {
	q, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return fmt.Errorf("生成二维码失败：%w", err)
	}

	// Bitmap 已包含 4 个模块宽的静区，这是二维码规范要求的，
	// 去掉它会让不少扫码器识别失败，所以这里不加也不减边距。
	bits := q.Bitmap()

	// 模块数是奇数（21 + 4k），加上两侧静区后仍是奇数，
	// 补一行全白凑成偶数，否则最后一行没有下半格可用。
	if len(bits)%2 == 1 {
		bits = append(bits, make([]bool, len(bits[0])))
	}

	var b strings.Builder
	for y := 0; y < len(bits); y += 2 {
		// 每行以 reset 结尾，所以 last 必须逐行重置，
		// 否则第二行开头会沿用上一行的转义序列（此时已被 reset 清掉）而渲染错色。
		last := ""
		for x := range bits[y] {
			if esc := sgr(bits[y][x], bits[y+1][x]); esc != last {
				b.WriteString(esc)
				last = esc
			}
			b.WriteString("▀")
		}
		b.WriteString("\x1b[0m\n")
	}

	_, err = io.WriteString(w, b.String())
	return err
}

// sgr 返回「上半格 top、下半格 bottom」所需的 ANSI 转义序列。
// true 表示二维码中的黑模块。
func sgr(top, bottom bool) string {
	fg, bg := 37, 47 // 白前景、白背景
	if top {
		fg = 30
	}
	if bottom {
		bg = 40
	}
	return fmt.Sprintf("\x1b[%d;%dm", fg, bg)
}
