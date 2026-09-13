package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/ultramanDecker/bili-comment/internal/bilibili"
	"github.com/ultramanDecker/bili-comment/internal/model"
)

func newCommentsCmd(g *globals) *cobra.Command {
	var (
		out   string
		limit int
		mode  string
		since string
		full  bool
	)

	cmd := &cobra.Command{
		Use:   "comments <url|bvid|av号>",
		Short: "抓取视频的一级评论",
		Long: `抓取视频的一级评论，输出为 JSONL（每行一条 JSON）。

输出是流式的：抓一页写一页。长任务中途中断或 Ctrl-C 时，
已经抓到的部分依然完整可用，最后一行会记录中断原因和实际抓到的条数。

文件由三类行组成，靠 type 字段区分：

  {"type":"video",...}     视频元数据与评论总数，首行
  {...,"rpid":123,...}     一条评论，无 type 字段
  {"type":"summary",...}   抓取结果与完整性信息，末行

默认裁掉头像地址与表情图片地址：这两项在每条评论里各占几十个字符，
而它们对文本分析没有价值——正文里已经写了 [微笑] 这样的表情名，
头像更是纯粹的装饰。一万条评论下这部分是几十万 token 的噪声。
需要图片地址时加 --full。

流式 JSONL 而不是一个大 JSON 数组，是为了让消费方能边下边处理，
并且不必为了读第一条评论而把上百兆的数组解析完。`,
		Example: `  bili comments BV1xx411c7mD
  bili comments BV1xx411c7mD --limit 500 -o comments.jsonl
  bili comments BV1xx411c7mD --mode time --since 2024-01-01`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runComments(cmd, g, args[0], commentsFlags{
				out: out, limit: limit, mode: mode, since: since, full: full,
			})
		},
	}

	fl := cmd.Flags()
	fl.StringVarP(&out, "out", "o", "", "写入文件而非打印到 stdout")
	fl.IntVar(&limit, "limit", 0, "最多抓取多少条一级评论，0 表示不限")
	fl.StringVar(&mode, "mode", "hot", "排序方式：hot（热度）或 time（时间）")
	fl.StringVar(&since, "since", "", "只抓该日期之后的评论（YYYY-MM-DD），需配合 --mode time")
	fl.BoolVar(&full, "full", false, "保留头像与表情图片地址（默认裁掉，见下方说明）")

	return cmd
}

type commentsFlags struct {
	out   string
	limit int
	mode  string
	since string
	full  bool
}

func runComments(cmd *cobra.Command, g *globals, input string, f commentsFlags) error {
	ctx := cmd.Context()

	commentMode, err := bilibili.ParseCommentMode(f.mode)
	if err != nil {
		return usageErrorf("%v", err)
	}

	var since time.Time
	if f.since != "" {
		since, err = time.ParseInLocation("2006-01-02", f.since, time.Local)
		if err != nil {
			return usageErrorf("--since 需要 YYYY-MM-DD 格式的日期，实际是 %q", f.since)
		}
		if commentMode != bilibili.ModeTime {
			return usageErrorf("--since 只在 --mode time 下有意义：按热度排序时评论不是按时间递减的，" +
				"看到旧评论就停会漏掉后面的新评论")
		}
	}

	c, err := g.client()
	if err != nil {
		return err
	}

	ref, err := c.ResolveRef(ctx, input)
	if err != nil {
		return err
	}
	video, err := c.Video(ctx, ref)
	if err != nil {
		return err
	}

	// 未登录时服务端只返回个位数的评论并且直接说「到底了」，
	// 看起来像这个视频没人评论，实际上是被截断了。
	if !c.LoggedIn() {
		g.logf("警告：未登录。服务端会对未登录请求截断评论列表（实测只返回 3 条且报告已到底），" +
			"抓取结果会严重少于实际。请先执行 bili login。")
	}

	w, closeOut, err := openOutput(cmd, f.out)
	if err != nil {
		return err
	}
	defer closeOut()

	bw := bufio.NewWriter(w)

	// Ctrl-C 时不要直接死掉：取消 context，让分页循环正常收尾，
	// 这样 summary 行能写出去，已抓到的数据也有明确的完整性说明。
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	enc := json.NewEncoder(bw)

	if err := enc.Encode(header{
		Type:      "video",
		BVID:      video.BVID,
		AID:       video.AID,
		Title:     video.Title,
		UpMid:     video.Up.Mid,
		UpName:    video.Up.Name,
		PubTime:   video.PubTime,
		StatReply: video.Stat.Reply,
		Mode:      commentMode.String(),
		FetchedAt: model.Time(time.Now()),
	}); err != nil {
		return err
	}

	written := 0

	total := video.Stat.Reply
	if total > 0 {
		g.logf("《%s》服务端报告评论数 %d，按%s排序抓取%s", video.Title, total,
			modeLabel(commentMode), limitLabel(f.limit))
	}

	res, err := c.FetchComments(ctx, bilibili.FetchOptions{
		AID:   video.AID,
		Mode:  commentMode,
		Limit: f.limit,
		Since: since,
		UpMid: video.Up.Mid,
	}, func(page []*model.Comment) error {
		for _, cm := range page {
			if !f.full {
				trimForLLM(cm)
			}
			if err := enc.Encode(cm); err != nil {
				return err
			}
		}
		// 每页落一次盘。长任务跑到一半断电时，前面抓到的都能保住。
		if err := bw.Flush(); err != nil {
			return err
		}
		// 自己计数而不用 res.Fetched：那个字段要等回调返回后才更新，
		// 依赖它等于依赖一个「此刻恰好是上一页的累计值」的时序细节。
		written += len(page)
		g.logf("已抓取 %d 条", written)
		return nil
	})
	if err != nil {
		// 已经抓到的部分照常写 summary，把失败说清楚而不是丢掉。
		_ = enc.Encode(summary{
			Type: "summary", Expected: res.Expected, Fetched: res.Fetched,
			Pages: res.Pages, Reason: "error", Error: err.Error(),
		})
		_ = bw.Flush()
		return err
	}

	if err := enc.Encode(summary{
		Type:      "summary",
		Expected:  res.Expected,
		Fetched:   res.Fetched,
		Pages:     res.Pages,
		Reason:    string(res.Stopped),
		Truncated: res.Truncated,
	}); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}

	reportFetch(g, f.out, res)
	return nil
}

// header 是 JSONL 的第一行，把视频上下文和抓取参数一起带上，
// 让这个文件脱离命令行也能自解释。
type header struct {
	Type      string     `json:"type"`
	BVID      string     `json:"bvid"`
	AID       int64      `json:"aid"`
	Title     string     `json:"title"`
	UpMid     int64      `json:"up_mid"`
	UpName    string     `json:"up_name"`
	PubTime   model.Time `json:"pub_time"`
	StatReply int64      `json:"stat_reply"`
	Mode      string     `json:"mode"`
	FetchedAt model.Time `json:"fetched_at"`
}

// summary 是 JSONL 的最后一行，记录这次抓取到底拿到了多少、为什么停下。
//
// 这一行是整个输出的关键：下游拿到一份残缺数据却不知道它残缺，
// 比拿不到数据更危险——基于部分评论得出的「用户普遍认为」是纯粹的幻觉。
type summary struct {
	Type      string `json:"type"`
	Expected  int    `json:"expected"`
	Fetched   int    `json:"fetched"`
	Pages     int    `json:"pages"`
	Reason    string `json:"reason"`
	Truncated bool   `json:"truncated,omitempty"`
	Error     string `json:"error,omitempty"`
}

func openOutput(cmd *cobra.Command, path string) (io.Writer, func(), error) {
	if path == "" {
		return cmd.OutOrStdout(), func() {}, nil
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, fmt.Errorf("创建目录失败：%w", err)
		}
	}
	fh, err := os.Create(path)
	if err != nil {
		return nil, nil, fmt.Errorf("创建 %s 失败：%w", path, err)
	}
	return fh, func() { fh.Close() }, nil
}

func reportFetch(g *globals, out string, res *bilibili.FetchResult) {
	where := "已输出到 stdout"
	if out != "" {
		where = "已写入 " + out
	}

	switch res.Stopped {
	case bilibili.StopEnd:
		g.logf("%s：抓取完成，共 %d 条（服务端报告 %d 条）", where, res.Fetched, res.Expected)
	default:
		g.logf("%s：抓取 %d 条后停止（%s），服务端报告共 %d 条",
			where, res.Fetched, stopLabel(res.Stopped), res.Expected)
	}

	// 明确告诉用户漏了东西。默认按热度排序时，5000 条是服务端的硬上限，
	// 不说明的话用户会以为「评论都在这儿了」。
	if res.Truncated {
		g.logf("注意：只抓到了 %d/%d 条。单种排序最多翻到约 5000 条，且未登录会被截断。"+
			"后续版本会用双排序合并来突破这个上限。", res.Fetched, res.Expected)
	}
}

func stopLabel(r bilibili.StopReason) string {
	switch r {
	case bilibili.StopLimit:
		return "达到 --limit"
	case bilibili.StopSince:
		return "达到 --since"
	case bilibili.StopEnd:
		return "服务端无更多数据"
	case bilibili.StopCursorStuck:
		return "游标不再前进"
	}
	return string(r)
}

func modeLabel(m bilibili.CommentMode) string {
	if m == bilibili.ModeTime {
		return "时间"
	}
	return "热度"
}

// trimForLLM 去掉对文本分析没有价值、但每条都要占几十个字符的字段。
//
// 这个裁剪发生在输出层而不是抓取层：数据本身是完整的，
// 只是默认不写进文件。想还原细节时加 --full 重跑即可，不需要重新抓。
func trimForLLM(cm *model.Comment) {
	cm.User.Avatar = ""
	cm.Emotes = nil
}

func limitLabel(n int) string {
	if n <= 0 {
		return "全部（可能耗时很久）"
	}
	return fmt.Sprintf("最多 %d 条", n)
}
