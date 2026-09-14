// Package cli 定义命令行界面。这一层只做参数解析与流程编排，
// 具体逻辑都在 internal/bilibili 与 internal/output 里。
package cli

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/ultramanDecker/bili-comment/internal/bilibili"
	"github.com/ultramanDecker/bili-comment/internal/config"
)

// Version 是程序版本号。
//
// 默认 "dev"，发布时用 -ldflags "-X .../internal/cli.Version=v0.1.0" 注入。
// 它会写进数据集的 manifest.json——一份数据在被生成几个月后，
// 「当时用的是哪个版本」是复现问题的第一个线索。
var Version = "dev"

// ExitCode 是设计文档约定的退出码，便于脚本批量调用时判断。
const (
	ExitOK        = 0
	ExitError     = 1
	ExitUsage     = 2
	ExitRateLimit = 3
	ExitNeedLogin = 4
	ExitNoComment = 5
)

// globals 持有各子命令共享的全局选项。
type globals struct {
	delay time.Duration
	debug bool
}

// logf 把进度与警告写到 stderr，保持 stdout 干净以便重定向。
func (g *globals) logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// client 按当前配置构造 API 客户端。
func (g *globals) client() (*bilibili.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return bilibili.NewClient(bilibili.Options{
		Delay:   g.delay,
		Cookies: cfg.Cookies,
		Debug:   g.debug,
		Logf:    g.logf,
	}), nil
}

// usageError 标记参数用法错误，用于返回退出码 2。
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usageErrorf(format string, args ...any) error {
	return &usageError{fmt.Sprintf(format, args...)}
}

// exactArgs 与 cobra.ExactArgs 行为一致，但返回可识别的用法错误。
func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			return usageErrorf("需要 %d 个参数，实际收到 %d 个\n用法：%s",
				n, len(args), cmd.UseLine())
		}
		return nil
	}
}

// NewRootCmd 构造根命令。
func NewRootCmd() *cobra.Command {
	g := &globals{}

	root := &cobra.Command{
		Use:           "bili",
		Short:         "把 B 站视频的评论区提取成结构化文件",
		Long:          "bili 抓取 B 站视频的评论与元数据，输出成便于程序与 LLM 消费的结构化文件。",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	pf := root.PersistentFlags()
	pf.DurationVar(&g.delay, "delay", bilibili.DefaultDelay,
		"请求间隔。默认值约合 24 次/分钟，是社区实测的安全线，调低会被风控")
	pf.BoolVar(&g.debug, "debug", false, "打印调试日志到 stderr")

	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &usageError{err.Error()}
	})

	root.AddCommand(
		newVideoCmd(g),
		newCommentsCmd(g),
		newLoginCmd(g),
		newLogoutCmd(g),
		newWhoamiCmd(g),
	)
	return root
}

// Execute 运行命令行并返回进程退出码。
func Execute() int {
	root := NewRootCmd()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "\n错误："+err.Error())
		return exitCode(err)
	}
	return ExitOK
}

func exitCode(err error) int {
	var ue *usageError
	if errors.As(err, &ue) {
		return ExitUsage
	}
	switch bilibili.KindOf(err) {
	case bilibili.KindRateLimit:
		return ExitRateLimit
	case bilibili.KindNeedLogin:
		return ExitNeedLogin
	case bilibili.KindNoComment:
		return ExitNoComment
	}
	return ExitError
}
