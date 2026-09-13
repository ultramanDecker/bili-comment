package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/ultramanDecker/bili-comment/internal/bilibili"
	"github.com/ultramanDecker/bili-comment/internal/config"
)

const (
	// B 站服务端的二维码有效期约 3 分钟，客户端等更久没有意义。
	defaultLoginTimeout = 3 * time.Minute
	qrPollInterval      = 2 * time.Second
	// 网络抖动不该中断整个登录流程，但连续失败说明真的连不上。
	maxPollFailures = 3
)

func newLoginCmd(g *globals) *cobra.Command {
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "login",
		Short: "扫码登录并缓存登录态",
		Long: `扫码登录 B 站，把登录态缓存到本地配置目录。

登录不是抓取评论的必需步骤，但登录后能看到的评论更多：
未登录时部分评论会被折叠或不可见，且更容易被风控。

登录态是一组 cookie，保存在配置文件里（权限 0600，仅当前用户可读）。
它等同于账号凭据，不要提交到版本库或分享给他人。`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLogin(cmd, g, timeout)
		},
	}

	cmd.Flags().DurationVar(&timeout, "timeout", defaultLoginTimeout,
		"等待扫码的最长时间")
	return cmd
}

func runLogin(cmd *cobra.Command, g *globals, timeout time.Duration) error {
	ctx := cmd.Context()
	client, err := g.client()
	if err != nil {
		return err
	}

	// 重新登录会覆盖已有登录态，先让用户知道覆盖的是谁。
	if client.LoggedIn() {
		if acc, err := client.WhoAmI(ctx); err == nil {
			g.logf("当前已登录：%s，继续登录将覆盖它", accountLine(acc))
		}
	}

	qr, err := client.QRLogin(ctx)
	if err != nil {
		return err
	}

	// 二维码走 stdout（它是这个命令的主要产出），进度提示走 stderr，
	// 这样重定向 stdout 时不会污染二维码，交互时两者又都看得见。
	if err := renderQR(cmd.OutOrStdout(), qr.URL); err != nil {
		return err
	}
	g.logf("请用 B 站手机客户端扫码，并在手机上确认。")

	// 状态在终端里原地刷新，重定向时退化成逐条打印。
	status := &statusLine{w: cmd.ErrOrStderr(), live: isTerminal(os.Stderr)}
	defer status.clear()

	ticker := time.NewTicker(qrPollInterval)
	defer ticker.Stop()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	fails := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-deadline.C:
			status.clear()
			return fmt.Errorf("等待扫码超时（%s）", timeout)

		case <-ticker.C:
			state, err := client.PollQR(ctx, qr.Key)
			if err != nil {
				fails++
				if fails >= maxPollFailures {
					status.clear()
					return fmt.Errorf("连续 %d 次查询扫码状态失败：%w", fails, err)
				}
				g.logf("查询扫码状态失败（%d/%d）：%v", fails, maxPollFailures, err)
				continue
			}
			fails = 0

			switch state {
			case bilibili.LoginSuccess:
				status.clear()
				return saveLogin(ctx, cmd, client)
			case bilibili.LoginExpired:
				status.clear()
				return errors.New("二维码已过期，请重新运行 bili login")
			}
			status.set(state.String())
		}
	}
}

// saveLogin 持久化登录态并立刻回查一次账号，确认这组 cookie 真的可用——
// 只写盘不验证的话，一个失效的登录态要到下次抓取才会暴露。
func saveLogin(ctx context.Context, cmd *cobra.Command, client *bilibili.Client) error {
	// 整体读入再写回，避免覆盖掉配置里与登录无关的字段。
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg.Cookies = client.Cookies()
	if err := config.Save(cfg); err != nil {
		return err
	}

	acc, err := client.WhoAmI(ctx)
	if err != nil {
		return fmt.Errorf("登录态已写入，但校验失败：%w", err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "已登录：%s\n", accountLine(acc))
	if p, err := config.Path(); err == nil {
		fmt.Fprintf(out, "登录态已保存到 %s\n", p)
	}
	return nil
}

func newLogoutCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "清除本地缓存的登录态",
		Long: `删除本地保存的 cookie。

只影响本机：B 站服务端不会因此让其它设备退出登录，
这组 cookie 在过期前仍然有效，如需彻底失效请在网页端「退出所有设备」。`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := config.Path()
			if err != nil {
				return err
			}
			if err := config.Clear(); err != nil {
				return fmt.Errorf("清除登录态失败：%w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已清除 %s\n", p)
			return nil
		},
	}
}

func newWhoamiCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "显示当前登录的账号",
		Long: `查询当前登录的账号，用于确认登录态是否有效。

登录态失效时以退出码 4 结束，脚本可据此判断是否需要重新登录。`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := g.client()
			if err != nil {
				return err
			}
			acc, err := client.WhoAmI(cmd.Context())
			if err != nil {
				if bilibili.KindOf(err) == bilibili.KindNeedLogin {
					// 保留分类，退出码才是 4；换成 fmt.Errorf 就退化成 1 了。
					return bilibili.WithMessage(err, "未登录或登录态已失效，请先运行 bili login")
				}
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), accountLine(acc))
			return nil
		},
	}
}

func accountLine(a *bilibili.Account) string {
	s := fmt.Sprintf("%s（UID %d，Lv%d", a.Name, a.Mid, a.Level)
	if a.Vip {
		s += "，大会员"
	}
	return s + "）"
}

// statusLine 在终端里用回车原地刷新同一行状态，重定向到文件时改为每条只打印一次。
type statusLine struct {
	w    io.Writer
	live bool
	last string
}

func (s *statusLine) set(msg string) {
	if s.live {
		fmt.Fprintf(s.w, "\r\x1b[K%s", msg)
	} else if msg != s.last {
		fmt.Fprintln(s.w, msg)
	}
	s.last = msg
}

func (s *statusLine) clear() {
	if s.live {
		fmt.Fprint(s.w, "\r\x1b[K")
	}
}

// isTerminal 判断输出是否为终端，仅用于决定进度提示能否原地刷新。
// 判断失误最多让日志里多几个回车，不影响功能，所以不引入终端检测依赖。
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
