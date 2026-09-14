package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ultramanDecker/bili-comment/internal/annotate"
	"github.com/ultramanDecker/bili-comment/internal/bilibili"
	"github.com/ultramanDecker/bili-comment/internal/model"
	"github.com/ultramanDecker/bili-comment/internal/output"
	"github.com/ultramanDecker/bili-comment/internal/store"
)

func newCommentsCmd(g *globals) *cobra.Command {
	var (
		out    string
		format []string
		limit  int
		mode   string
		since  string
		full   bool

		noReplies        bool
		repliesTop       int
		repliesMinLikes  int
		repliesMinCount  int
		repliesIncludeUp bool
		repliesIncludeTo bool

		concurrency int
		resume      bool
	)

	cmd := &cobra.Command{
		Use:   "comments <url|bvid|av号>",
		Short: "抓取视频的评论（一级评论 + 楼中楼）",
		Long: `抓取视频评论，输出为 JSONL（每行一条 JSON）。

输出是流式的：抓一页写一页。长任务中途中断或 Ctrl-C 时，
已经抓到的部分依然完整可用，最后一行会记录中断原因和实际抓到的条数。
配合 --resume 可以从断点继续，不重复也不遗漏。

文件由四类行组成，靠 type 字段区分：

  {"type":"video",...}      视频元数据与评论总数，首行
  {...,"rpid":123,...}      一级评论（没有 root 字段）
  {...,"root":123,...}      楼中楼（root 指向所属的一级评论）
  {"type":"reply_stub",...} 未展开的楼，记录「这里有多少条没看到」
  {"type":"summary",...}    抓取结果与完整性信息，末行

楼中楼是平铺输出的，层级靠 root 与 parent 两个字段还原：
root 是所属一级评论，parent 是被直接回复的那条评论。

默认抓全部楼中楼。想省时间可以用 --replies-top / --replies-min-likes
限制展开范围——被跳过的楼会留下 reply_stub 行，不会静默消失。

默认裁掉头像地址与表情图片地址：这两项在每条评论里各占几十个字符，
而它们对文本分析没有价值——正文里已经写了 [微笑] 这样的表情名，
头像更是纯粹的装饰。一万条评论下这部分是几十万 token 的噪声。
需要图片地址时加 --full。`,
		Example: `  bili comments BV1xx411c7mD
  bili comments BV1xx411c7mD --limit 500 -o comments.jsonl
  bili comments BV1xx411c7mD --replies-top 50 --concurrency 3
  bili comments BV1xx411c7mD -o comments.jsonl --resume`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runComments(cmd, g, args[0], commentsFlags{
				out: out, format: format, limit: limit, mode: mode, since: since, full: full,
				policy: bilibili.ReplyPolicy{
					Disabled:   noReplies,
					Top:        repliesTop,
					MinLikes:   repliesMinLikes,
					MinCount:   repliesMinCount,
					IncludeUp:  repliesIncludeUp,
					IncludeTop: repliesIncludeTo,
				},
				concurrency: concurrency,
				resume:      resume,
			})
		},
	}

	fl := cmd.Flags()
	fl.StringVarP(&out, "out", "o", "", "写入文件或目录，默认打印到 stdout")
	fl.StringSliceVar(&format, "format", nil,
		"额外输出这些格式（可重复或用逗号分隔）："+strings.Join(output.Names(), "、"))
	fl.IntVar(&limit, "limit", 0, "最多抓取多少条一级评论，0 表示不限")
	fl.StringVar(&mode, "mode", "hot", "排序方式：hot（热度）或 time（时间）")
	fl.StringVar(&since, "since", "", "只抓该日期之后的评论（YYYY-MM-DD），需配合 --mode time")
	fl.BoolVar(&full, "full", false, "保留头像与表情图片地址（默认裁掉，见下方说明）")

	fl.BoolVar(&noReplies, "no-replies", false, "完全不抓楼中楼，只抓一级评论")
	fl.IntVar(&repliesTop, "replies-top", 0, "只展开回复数最多的 N 楼，0 表示不限")
	fl.IntVar(&repliesMinLikes, "replies-min-likes", 0, "只展开点赞数 >= N 的楼")
	fl.IntVar(&repliesMinCount, "replies-min-count", 0, "只展开回复数 >= N 的楼")
	fl.BoolVar(&repliesIncludeUp, "replies-include-up", false, "强制展开 UP 主自己发的一级评论")
	fl.BoolVar(&repliesIncludeTo, "replies-include-top", false, "强制展开置顶评论")

	fl.IntVar(&concurrency, "concurrency", 1, "楼中楼的并发数。注意这不提高请求速率，限速器才是刹车")
	fl.BoolVar(&resume, "resume", false, "从上次中断处继续（需要 -o 指定输出文件）")

	return cmd
}

type commentsFlags struct {
	out         string
	format      []string
	limit       int
	mode        string
	since       string
	full        bool
	policy      bilibili.ReplyPolicy
	concurrency int
	resume      bool
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
	if f.resume && f.out == "" {
		return usageErrorf("--resume 需要 -o 指定输出文件：续传依赖已有的输出文件和进度文件")
	}
	if f.concurrency < 1 {
		return usageErrorf("--concurrency 至少为 1")
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

	// Ctrl-C 时不要直接死掉：取消 context，让抓取循环正常收尾，
	// 这样 summary 行能写出去，已抓到的数据也有明确的完整性说明，
	// 进度文件也停在最后一个完整的楼边界上。
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	out, err := openSink(cmd, g, f, video)
	if err != nil {
		return err
	}
	defer out.Close()

	// ---- 阶段一：一级评论 ----

	roots, resumeIndex, err := phaseRoots(ctx, g, c, out, f, video, commentMode, since)
	if err != nil {
		out.writeSummary(&output.Summary{Type: "summary", Reason: "error", Error: err.Error()})
		return err
	}

	// ---- 决策点 ----

	plan := bilibili.BuildPlan(roots, f.policy)
	reportPlan(g, plan, f.policy)

	// ---- 阶段二：楼中楼 ----

	rep, err := phaseReplies(ctx, g, c, out, f, video, plan, resumeIndex)
	if err != nil {
		out.writeSummary(&output.Summary{
			Type: "summary", Reason: "error", Error: err.Error(), Replies: rep,
		})
		return err
	}

	// ---- 收尾 ----

	out.writeSummary(&output.Summary{
		Type:      "summary",
		Expected:  out.rootExpected,
		Fetched:   out.rootFetched,
		Pages:     out.rootPages,
		Reason:    string(out.rootStopped),
		Truncated: out.rootTruncated,
		Replies:   rep,
	})
	if err := out.Flush(); err != nil {
		return err
	}

	// ---- 数据集附带文件 ----
	//
	// 放在进度文件清理之前：这几份是从已完成的主输出推导出来的，
	// 需要主输出是完整的。它们写失败不该让整次抓取算失败——
	// 评论数据本身已经好好地躺在文件里了，那才是用户要的东西。
	if out.dir != "" {
		if err := writeDatasetExtras(out, video, f.mode); err != nil {
			g.logf("警告：生成数据集附带文件失败：%v", err)
		}
	}

	// 只有完整跑完才删进度文件。中途出错时留着，下次才能续。
	if out.persistent() {
		if err := store.Remove(out.journalPath); err != nil {
			g.logf("警告：清理进度文件失败：%v", err)
		}
	}
	reportFetch(g, out)
	return nil
}

// phaseRoots 抓取一级评论并写入输出。返回评论列表，以及楼中楼阶段该从第几个计划项开始。
func phaseRoots(
	ctx context.Context, g *globals, c *bilibili.Client, out *sink,
	f commentsFlags, video *model.Video, commentMode bilibili.CommentMode,
	since time.Time,
) ([]*model.Comment, int, error) {
	// 一级评论阶段已经完成：文件里就有全部一级评论，读回来重建计划即可，
	// 不必为了算计划再抓一遍。
	if out.resuming && out.journal.Phase == store.PhaseReplies {
		roots, err := readRootsFrom(out.primary.path)
		if err != nil {
			return nil, 0, err
		}
		// 续传时把已经写进文件的那部分喂回复读索引。
		// 不这么做的话，断点之后的评论只会跟断点之后的评论比，
		// 前缀里的复读源就漏掉了。
		seedCollector(out, out.primary.path, out.primary.n)
		// 一级评论这次一条都没抓，但文件里有。收尾提示和 summary 行描述的是
		// 文件而不是本次运行，所以把上次定稿的统计取回来，
		// 否则会写出 fetched=0 而这种话——与文件内容直接矛盾。
		out.rootExpected = out.journal.RootExpected
		out.rootFetched = out.journal.RootFetched
		out.rootPages = out.journal.RootPages
		out.rootStopped = bilibili.StopReason(out.journal.RootStopped)
		out.rootTruncated = out.journal.RootTruncated
		g.logf("续传：一级评论已有 %d 条，从楼中楼第 %d 楼继续", len(roots), out.journal.PlanIndex+1)
		return roots, out.journal.PlanIndex, nil
	}

	startCursor := ""
	if out.resuming {
		startCursor = out.journal.RootCursor
		g.logf("续传：一级评论从断点继续，已有 %d 条", out.rootFetched)
		// 把前缀喂回复读索引，理由同上面一级评论已抓完的那条分支。
		seedCollector(out, out.primary.path, out.primary.n)
	}

	if !out.resuming {
		total := video.Stat.Reply
		if total > 0 {
			g.logf("《%s》服务端报告评论数 %d，按%s排序抓取%s", video.Title, total,
				modeLabel(commentMode), limitLabel(f.limit))
		}
	}

	var roots []*model.Comment

	res, err := c.FetchCommentsFrom(ctx, bilibili.FetchOptions{
		AID:   video.AID,
		Mode:  commentMode,
		Limit: f.limit,
		Since: since,
		UpMid: video.Up.Mid,
	}, startCursor, func(page []*model.Comment, cur bilibili.Cursor) error {
		for _, cm := range page {
			if err := out.writeComment(cm); err != nil {
				return err
			}
		}
		roots = append(roots, page...)
		if err := out.Flush(); err != nil {
			return err
		}

		// 进度只在这里推进：上面已经 Flush，Offset 指向的字节确实在磁盘上了。
		// 顺序反过来就会记录一个比实际内容更靠后的位置，续传时截断出空洞。
		out.rootFetched += len(page)
		if !out.persistent() {
			return nil
		}
		out.journal.Phase = store.PhaseRoots
		out.journal.RootCursor = cur.NextOffset
		g.logf("已抓取 %d 条一级评论", out.rootFetched)
		return out.saveJournal()
	})
	if err != nil {
		return roots, 0, err
	}
	out.rootExpected = res.Expected
	out.rootFetched = res.Fetched
	out.rootPages = res.Pages
	out.rootStopped = res.Stopped
	out.rootTruncated = res.Truncated

	if out.persistent() {
		// 一级评论阶段完成，进度切到楼中楼阶段。此时 RootCursor 不再有意义，
		// 要清掉，否则下次误以为还能从游标续抓。
		out.journal.Phase = store.PhaseReplies
		out.journal.RootCursor = ""
		out.journal.PlanIndex = 0
		out.journal.PolicyHash = f.policy.Fingerprint()
		// 一级评论的统计在这里定稿。之后即便续传时不再重抓一级评论，
		// summary 与收尾提示也要靠它们说话，所以必须落盘。
		out.journal.RootExpected = res.Expected
		out.journal.RootFetched = res.Fetched
		out.journal.RootPages = res.Pages
		out.journal.RootStopped = string(res.Stopped)
		out.journal.RootTruncated = res.Truncated
		if err := out.saveJournal(); err != nil {
			return roots, 0, err
		}
	}
	return roots, 0, nil
}

// phaseReplies 按计划展开楼中楼。
func phaseReplies(
	ctx context.Context, g *globals, c *bilibili.Client, out *sink,
	f commentsFlags, video *model.Video, plan *bilibili.ReplyPlan, startIndex int,
) (*output.ReplySummary, error) {
	rep := &output.ReplySummary{
		RootsWithReplies: countRootsWithReplies(plan),
		Skipped:          plan.StubCount(),
		Reasons:          map[string]int{},
	}
	for _, it := range plan.Items {
		if !it.Expand {
			// 本来就没有回复的楼不算「漏了东西」，它没有东西可漏。
			// 算进去会让「跳过 17 栋」这个数字虚高，掩盖真正被策略裁掉的部分。
			if it.Skip == bilibili.SkipNoReplies {
				continue
			}
			rep.Reasons[string(it.Skip)]++
			rep.SkippedReplies += it.ReplyCount
		}
	}

	// 续传时把已有的累计量接过来。summary 描述的是**输出文件**，不是本次运行，
	// 所以上一次已经写进文件的楼层必须计入。
	//
	// Expanded 不用存进进度文件：计划是确定性的，startIndex 之前的可展开楼
	// 按构造都已经写完落盘了，数一数就知道。真正拿不回来的是抓取结果里
	// 那些数字（回复条数、是否被上限截断），所以它们才要落盘。
	if out.resuming && out.journal.Phase == store.PhaseReplies {
		rep.Fetched = out.journal.ReplyFetched
		rep.Expected = out.journal.ReplyExpected
		rep.OffsetLimited = out.journal.ReplyOffsetLimited
		rep.Truncated = out.journal.ReplyTruncated
		for i := 0; i < startIndex && i < len(plan.Items); i++ {
			if plan.Items[i].Expand {
				rep.Expanded++
			}
		}
	}

	// 墓碑行在抓取之前一次性写完。它们描述的是「计划」而不是「结果」，
	// 先写完能让下游在抓取还在进行时就读到完整的范围说明；
	// 而且它们不依赖任何请求，早写早踏实。
	//
	// 续传时不能重写：它们已经在上次保留的前缀里了，再写一遍就是重复行，
	// 恰好是续传最该避免的事。判断依据是进度文件里的 StubsWritten——
	// **不能**用 startIndex == 0 代替：「第一楼抓到一半时中断」是最常见的
	// 断点，那时墓碑行已经落盘，而 PlanIndex 还是 0，按 PlanIndex 判断
	// 就会把 17 行墓碑写第二遍。
	if !f.policy.IsDefault() && !out.journal.StubsWritten {
		for _, it := range plan.Items {
			if it.Expand || it.Skip == bilibili.SkipNoReplies {
				continue
			}
			if err := out.writeStub(&output.Stub{
				Type:       "reply_stub",
				Rpid:       it.Root,
				ReplyCount: it.ReplyCount,
				Expanded:   false,
				Reason:     string(it.Skip),
			}); err != nil {
				return rep, err
			}
		}
		if err := out.Flush(); err != nil {
			return rep, err
		}
		if out.persistent() {
			out.journal.StubsWritten = true
			if err := out.saveJournal(); err != nil {
				return rep, err
			}
		}
	}

	// 只保留要展开的楼，下标仍对应计划顺序。
	var todo []int
	for i := startIndex; i < len(plan.Items); i++ {
		if plan.Items[i].Expand {
			todo = append(todo, i)
		}
	}
	if len(todo) == 0 {
		g.logf("没有需要展开的楼中楼")
		return rep, nil
	}

	total := plan.ExpandCount()
	if startIndex > 0 {
		g.logf("待展开 %d 楼楼中楼（共 %d 楼，已跳过前 %d 楼），预计还需 %s",
			len(todo), total, startIndex, eta(plan, startIndex, c))
	} else {
		g.logf("待展开 %d 楼楼中楼，预计耗时 %s", len(todo), eta(plan, 0, c))
	}

	done := 0
	err := bilibili.RunOrdered(ctx, f.concurrency, len(todo),
		func(ctx context.Context, i int) (rootReplies, error) {
			idx := todo[i]
			root := plan.Items[idx].Root
			var got []*model.Comment
			res, err := c.FetchReplies(ctx, video.AID, root, video.Up.Mid,
				func(page []*model.Comment) error {
					got = append(got, page...)
					return nil
				})
			return rootReplies{root: root, comments: got, res: res}, err
		},
		func(i int, rr rootReplies) error {
			for _, cm := range rr.comments {
				if err := out.writeComment(cm); err != nil {
					return err
				}
			}
			if err := out.Flush(); err != nil {
				return err
			}

			idx := todo[i]
			if rr.res != nil {
				rep.Fetched += rr.res.Fetched
				rep.Expected += rr.res.Expected
				rep.Expanded++
				if rr.res.Truncated {
					rep.Truncated = true
					if rr.res.Stopped == bilibili.StopOffsetLimit {
						rep.OffsetLimited++
					}
				}
			}

			// 一栋楼写完并落盘后才推进进度。中断时最后那栋半截的楼会被
			// 截断掉重抓，宁可重抓一楼，不要留下半栋楼的数据当成完整的。
			//
			// 累计量与 Offset 必须在同一次 Save 里落盘：它们描述的是同一个
			// 时刻的文件内容，分两次写就可能出现「偏移对得上、数字对不上」
			// 的中间态，续传读到的 summary 就是假的。
			if out.persistent() {
				out.journal.PlanIndex = idx + 1
				out.journal.ReplyFetched = rep.Fetched
				out.journal.ReplyExpected = rep.Expected
				out.journal.ReplyOffsetLimited = rep.OffsetLimited
				out.journal.ReplyTruncated = rep.Truncated
				if err := out.saveJournal(); err != nil {
					return err
				}
			}

			done++
			if done%10 == 0 || done == len(todo) {
				g.logf("楼中楼进度 %d/%d，已抓 %d 条回复", done, len(todo), rep.Fetched)
			}
			return nil
		})
	if err != nil {
		return rep, err
	}
	return rep, nil
}

// rootReplies 是一栋楼的抓取结果，用于在保序回调里传递。
type rootReplies struct {
	root     uint64
	comments []*model.Comment
	res      *bilibili.ReplyResult
}

func countRootsWithReplies(plan *bilibili.ReplyPlan) int {
	n := 0
	for _, it := range plan.Items {
		if it.ReplyCount > 0 {
			n++
		}
	}
	return n
}

// readRootsFrom 从已写好的输出文件里读回一级评论。
//
// 判据是「没有 type 字段且没有 root 字段」：楼中楼一定带 root，
// 元数据行一定带 type，只有一级评论两者皆无。
func readRootsFrom(path string) ([]*model.Comment, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("读取已有输出失败：%w", err)
	}
	defer fh.Close()

	var roots []*model.Comment
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var probe struct {
			Type string  `json:"type"`
			Root *uint64 `json:"root"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			// 最后一行可能被截断（正在写），忽略即可。
			continue
		}
		if probe.Type != "" || probe.Root != nil {
			continue
		}
		var cm model.Comment
		if err := json.Unmarshal(line, &cm); err != nil {
			continue
		}
		roots = append(roots, &cm)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读取已有输出失败：%w", err)
	}
	if len(roots) == 0 {
		return nil, errors.New("进度文件说一级评论已抓完，但输出文件里一条也没有")
	}
	return roots, nil
}

// seedCollector 把已经写进文件的那部分评论喂回复读索引。
//
// 续传时索引必须接着上次的用，否则断点之后的评论只会与断点之后的评论比，
// 前缀里的复读源就漏掉了——表现是「同一条复读，前半段标了后半段没标」，
// 而这种不一致在文件里看不出来，只会让下游以为后半段是干净的。
//
// 代价是重跑一遍前缀的 SimHash，几十万条评论大约一两秒，只在续传时发生一次。
// 换来的是整份文件的标记口径一致。
func seedCollector(s *sink, path string, limit int64) {
	if path == "" || limit <= 0 {
		return
	}
	fh, err := os.Open(path)
	if err != nil {
		return
	}
	defer fh.Close()

	sc := bufio.NewScanner(io.LimitReader(fh, limit))
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var probe struct {
			Type string  `json:"type"`
			Root *uint64 `json:"root"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			continue
		}
		if probe.Type != "" {
			continue
		}
		var cm model.Comment
		if err := json.Unmarshal(line, &cm); err != nil {
			continue
		}
		// 结果丢弃：这里要的只是让索引「见过」这些评论。
		// 标记本身已经在上次运行时写进文件了。
		s.ann.Observe(&cm)
	}
}

// ---- 输出 ----

// outputSpec 描述一个输出目标。
type outputSpec struct {
	name    string
	path    string // 空表示 stdout
	primary bool
}

// planOutputs 决定本次要写哪些文件、写到哪里。
//
// 规则只有两条，但值得写清楚，因为 -o 的含义在单文件与目录两种模式下不一样：
//
//	-o DIR/    目录模式。默认产出 jsonl + md 与 manifest/stats/schema，
//	           --format 给出的格式在前者基础上追加。
//	-o FILE    单文件模式。文件格式由扩展名决定（默认 jsonl），
//	           --format 给出的格式写成同目录同名的兄弟文件。
//
// 为什么多格式不能写到 stdout：两个格式抢一个流，输出就成了两种语法交错
// 的乱码，谁都解析不了。这不是限制，是这种事本来就没有正确的做法。
func planOutputs(out string, formats []string) (dir string, specs []outputSpec, err error) {
	if out == "" {
		names := formats
		if len(names) == 0 {
			names = []string{"jsonl"}
		}
		if len(names) > 1 {
			return "", nil, usageErrorf("多种格式不能同时写到 stdout，请用 -o 指定一个目录")
		}
		name, cerr := canonicalFormat(names[0])
		if cerr != nil {
			return "", nil, cerr
		}
		if !output.Streaming(name) {
			return "", nil, usageErrorf("%s 需要在内存里攒齐全部数据才能写出，"+
				"写到 stdout 会先卡住再一次性吐出；请用 -o 指定文件或目录", name)
		}
		return "", []outputSpec{{name: name, primary: true}}, nil
	}

	if isDirPath(out) {
		if len(formats) == 0 {
			formats = output.DefaultDatasetFormats()
		}
		seen := map[string]bool{}
		for _, raw := range formats {
			name, cerr := canonicalFormat(raw)
			if cerr != nil {
				return "", nil, cerr
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			specs = append(specs, outputSpec{
				name:    name,
				path:    filepath.Join(out, "comments."+extOf(name)),
				primary: name == "jsonl",
			})
		}
		// 目录模式一定带 jsonl：它是主数据，也是 manifest 与 stats 的来源。
		// 用户只写了 --format md 时补上它，而不是报错——他想要的显然是
		// 「给我一份数据集」，jsonl 是数据集的一部分。
		if !seen["jsonl"] {
			specs = append(specs, outputSpec{
				name: "jsonl", path: filepath.Join(out, "comments.jsonl"), primary: true,
			})
		}
		return out, specs, nil
	}

	primary := output.ByExt(out)
	if primary == "" {
		primary = "jsonl"
	}
	primary, err = canonicalFormat(primary)
	if err != nil {
		return "", nil, err
	}
	if !output.Streaming(primary) {
		return "", nil, usageErrorf(
			"%s 需要把全部评论留在内存里才能写出，不适合作为 -o 的主输出。"+
				"请用 -o 输出 jsonl，再加 --format json 要一份 JSON", primary)
	}

	seen := map[string]bool{primary: true}
	specs = append(specs, outputSpec{name: primary, path: out, primary: true})
	base := strings.TrimSuffix(out, filepath.Ext(out))
	for _, raw := range formats {
		name, cerr := canonicalFormat(raw)
		if cerr != nil {
			return "", nil, cerr
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		specs = append(specs, outputSpec{
			name: name,
			path: base + "." + extOf(name),
		})
	}
	return "", specs, nil
}

// canonicalFormat 归一格式名并校验。
func canonicalFormat(name string) (string, error) {
	f, err := output.New(name)
	if err != nil {
		return "", usageErrorf("%v", err)
	}
	return f.Name(), nil
}

func extOf(name string) string {
	f, err := output.New(name)
	if err != nil {
		return name
	}
	return f.Ext()
}

// isDirPath 判断 -o 指的是目录还是文件。
//
// 认三种写法：已有的目录、以路径分隔符结尾、以及指向不存在路径但带结尾
// 分隔符的写法。不靠「有没有扩展名」来猜——`-o out` 这种名字太常见了，
// 猜错的话用户会得到一个名叫 out 的文件而不是目录。
func isDirPath(p string) bool {
	if strings.HasSuffix(p, "/") || strings.HasSuffix(p, `\`) {
		return true
	}
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// sink 是所有输出的唯一出口。
//
// 它同时维护「每种格式已写入的字节数」，这个计数就是续传的截断位置，
// 所以所有写入都必须经过它。
type sink struct {
	dir     string // 目录模式下的数据集目录，空表示单文件或 stdout
	primary *formatWriter
	writers []*formatWriter

	full bool
	ann  *annotate.Collector

	// journalPath 是主输出的路径，进度文件挂在它旁边。空表示这次没有
	// 可续传的目标（写 stdout），于是所有进度相关的动作都跳过。
	journalPath string

	journal *store.Journal

	// resuming 表示本次是接着已有的进度跑的，与「journal 非 nil」不是一回事：
	// 全新抓取也会建一个 journal 用来写进度。把两者混为一谈会导致
	// 全新抓取误以为自己在中途，从而跳过开头各行的写入。
	resuming bool

	rootExpected  int
	rootFetched   int
	rootPages     int
	rootStopped   bilibili.StopReason
	rootTruncated bool
}

// formatWriter 是一种格式的输出目标。
type formatWriter struct {
	name string
	path string // 空表示 stdout
	f    *os.File
	fmt  output.Formatter
	n    int64 // 已写入的字节数
}

func (w *formatWriter) label() string {
	if w.path == "" {
		return w.name + "（stdout）"
	}
	return w.path
}

func openSink(cmd *cobra.Command, g *globals, f commentsFlags, video *model.Video) (*sink, error) {
	dir, specs, err := planOutputs(f.out, f.format)
	if err != nil {
		return nil, err
	}

	s := &sink{
		dir:  dir,
		full: f.full,
		ann:  annotate.New(video.PubTime.Time()),
	}

	// 续传的进度文件挂在主输出上。主输出在目录模式下固定是 comments.jsonl，
	// 所以目录可以整体搬走而进度仍然有效。
	//
	// 必须挂主输出的**完整路径**，不能挂 f.out：目录模式下 f.out 是目录，
	// 挂上去就成了 `<dir>/.resume.json` 这样一个藏在目录里的文件，而
	// 读回来的地方找的是 `<dir>/comments.jsonl.resume.json`——
	// 于是 --resume 永远找不到进度，安静地从头重抓并把半截文件截掉。
	var primaryPath string
	for _, sp := range specs {
		if sp.primary {
			primaryPath = sp.path
		}
	}
	if primaryPath == "" && len(specs) == 1 {
		primaryPath = specs[0].path
	}
	s.journalPath = primaryPath

	if f.resume {
		// 续传要把已有的一级评论读回来重建楼中楼计划（不然就得重抓一遍，
		// 白花几百个请求），而只有 JSONL 能被逐行读回。TSV 读不回 user.mid，
		// Markdown 读回正文都要靠猜标点。
		if p := primaryFormat(specs); p != "jsonl" {
			return nil, usageErrorf("--resume 需要 JSONL 作为主输出，而当前主格式是 %s："+
				"续传要把已有的一级评论读回来重建楼中楼计划，而只有 JSONL 能完整读回。"+
				"请把 -o 改成 .jsonl 文件，或用目录模式", p)
		}
		j, err := store.LoadJournal(primaryPath)
		switch {
		case errors.Is(err, store.ErrNoJournal):
			g.logf("没有找到进度文件，将开始一次全新的抓取")
		case err != nil:
			return nil, err
		default:
			if err := checkResumable(j, specs, f); err != nil {
				return nil, err
			}
			s.journal = j
			s.resuming = true
		}
	}

	for _, sp := range specs {
		w := &formatWriter{name: sp.name, path: sp.path}
		w.fmt, err = output.New(sp.name)
		if err != nil {
			s.Close()
			return nil, err
		}

		var dst io.Writer
		switch {
		case sp.path == "":
			dst = cmd.OutOrStdout()
		case s.resuming:
			// 截断到上次确认完整的位置：那半栋楼被干净地丢掉，
			// 重抓不会产生重复。每种格式各自截断，因为同一批记录在
			// 不同格式下的字节长度完全不同。
			off := j2offset(s.journal, sp.name)
			fh, terr := store.TruncateOutput(sp.path, off)
			if terr != nil {
				s.Close()
				return nil, terr
			}
			w.f, w.n = fh, off
			dst = fh
		default:
			if derr := store.EnsureDir(sp.path); derr != nil {
				s.Close()
				return nil, derr
			}
			fh, cerr := os.Create(sp.path)
			if cerr != nil {
				s.Close()
				return nil, fmt.Errorf("创建 %s 失败：%w", sp.path, cerr)
			}
			w.f, dst = fh, fh
		}

		meta := &output.Meta{
			Video:     videoRow(video, f.mode),
			Mode:      f.mode,
			FetchedAt: video.FetchedAt,
			Full:      f.full,
			Resuming:  s.resuming,
		}
		if berr := w.fmt.Begin(countingWriter{dst, &w.n}, meta); berr != nil {
			s.Close()
			return nil, fmt.Errorf("初始化 %s 输出失败：%w", w.label(), berr)
		}

		s.writers = append(s.writers, w)
		if sp.primary {
			s.primary = w
		}
	}
	if s.primary == nil {
		s.primary = s.writers[0]
	}

	if s.resuming {
		g.logf("续传：各输出已回退到上次确认完整的位置（丢弃上次未写完的那部分）")
	} else if len(specs) > 1 || s.dir != "" {
		names := make([]string, len(s.writers))
		for i, w := range s.writers {
			names[i] = w.name
		}
		g.logf("输出格式：%s", strings.Join(names, "、"))
	}

	if s.journal == nil {
		s.journal = &store.Journal{AID: video.AID, BVID: video.BVID}
	}
	return s, nil
}

// checkResumable 拦住两类会写出错位数据的续传。
func checkResumable(j *store.Journal, specs []outputSpec, f commentsFlags) error {
	// 策略变了就换了计划，进度下标会指向另一栋楼。
	if j.Phase == store.PhaseReplies && j.PolicyHash != "" &&
		j.PolicyHash != f.policy.Fingerprint() {
		return fmt.Errorf("进度文件记录的楼中楼参数与本次不一致，无法续传。"+
			"要么用回原来的参数，要么换一个输出文件重新抓（%s）", store.JournalPath(j.Output))
	}

	// 格式集合变了就无法续传：进度里记的是「每种格式写到第几字节」，
	// 集合变了这些位置就没有对应的文件了。
	planned := make([]string, 0, len(specs))
	for _, sp := range specs {
		planned = append(planned, sp.name)
	}
	if d := store.DiffFormats(j.Formats, planned); !d.Empty() {
		var why string
		switch {
		case len(d.Added) > 0 && len(d.Removed) > 0:
			why = fmt.Sprintf("这次多了 %s、少了 %s",
				strings.Join(d.Added, "、"), strings.Join(d.Removed, "、"))
		case len(d.Added) > 0:
			why = fmt.Sprintf("这次多了 %s，它没有可截断的位置，只能从头写"+
				"，而文件里已经有别的东西了", strings.Join(d.Added, "、"))
		default:
			why = fmt.Sprintf("这次少了 %s，那个文件会接在半截数据后面继续写",
				strings.Join(d.Removed, "、"))
		}
		return fmt.Errorf("上次抓取输出的是 %s，%s，无法续传。"+
			"请用回原来的 --format，或换一个输出文件重新抓（%s）",
			strings.Join(j.Formats, "、"), why, store.JournalPath(j.Output))
	}
	return nil
}

func j2offset(j *store.Journal, name string) int64 {
	if j == nil || j.Offsets == nil {
		return 0
	}
	return j.Offsets[name]
}

func primaryFormat(specs []outputSpec) string {
	for _, sp := range specs {
		if sp.primary {
			return sp.name
		}
	}
	return ""
}

// persistent 表示这次运行有进度文件可写，也就是有 -o。
func (s *sink) persistent() bool { return s.journalPath != "" }

// saveJournal 落一次进度。
//
// 调它之前必须先 Flush：Offset 记的是「已确认写到磁盘的字节数」，
// 顺序反了就会记下一个比文件实际内容更靠后的位置，续传时截断出空洞——
// 那段空洞里的数据永远不会被重抓，也不会被发现，只会安静地缺一块。
func (s *sink) saveJournal() error {
	if !s.persistent() {
		return nil
	}
	s.recordOffsets()
	return s.journal.Save(s.journalPath)
}

func videoRow(v *model.Video, mode string) *output.Video {
	return &output.Video{
		Type:      "video",
		BVID:      v.BVID,
		AID:       v.AID,
		Title:     v.Title,
		UpMid:     v.Up.Mid,
		UpName:    v.Up.Name,
		PubTime:   v.PubTime,
		StatReply: v.Stat.Reply,
		Mode:      mode,
		FetchedAt: v.FetchedAt,
	}
}

// countingWriter 统计写出的字节数。续传依赖它给出的位置，
// 所以它必须包在格式自己的缓冲之外——buffer 里的内容还没交出来，
// 不能算数，Flush 之后计数才等于文件长度。
type countingWriter struct {
	w io.Writer
	n *int64
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	*c.n += int64(n)
	return n, err
}

func (s *sink) write(r output.Row) error {
	for _, w := range s.writers {
		if err := w.fmt.Write(r); err != nil {
			return fmt.Errorf("写入 %s 失败：%w", w.label(), err)
		}
	}
	return nil
}

func (s *sink) writeComment(cm *model.Comment) error {
	s.ann.Mark(cm)
	if !s.full {
		trimForLLM(cm)
	}
	return s.write(output.Row{Kind: output.KindComment, Comment: cm})
}

func (s *sink) writeStub(st *output.Stub) error {
	return s.write(output.Row{Kind: output.KindStub, Stub: st})
}

// writeSummary 收尾。它不只是「写一行」——各格式的完整度说明都在这里产出，
// 对 TSV 和 Markdown 来说是整整一段。
func (s *sink) writeSummary(sm *output.Summary) {
	for _, w := range s.writers {
		// 收尾失败也要把所有格式都试一遍：某个文件写不进去（磁盘满、
		// 权限变了）不该让其他格式连「这次抓取不完整」都记不上。
		_ = w.fmt.End(sm)
	}
	_ = s.Flush()
}

func (s *sink) Flush() error {
	for _, w := range s.writers {
		if err := w.fmt.Flush(); err != nil {
			return fmt.Errorf("刷新 %s 失败：%w", w.label(), err)
		}
	}
	for _, w := range s.writers {
		if w.f != nil {
			// 数据要真的到磁盘上，续传读到的才算数。断电时靠 fsync 保证。
			if err := w.f.Sync(); err != nil {
				return fmt.Errorf("同步 %s 失败：%w", w.label(), err)
			}
		}
	}
	return nil
}

// recordOffsets 把各格式当前的字节数记进进度。
//
// 必须在 Flush 之后调用：Flush 之前这些数字还留在各格式自己的缓冲里，
// 记下来的位置比文件实际长度靠后，续传时会截断出空洞。
func (s *sink) recordOffsets() {
	if s.journal == nil {
		return
	}
	offs := make(map[string]int64, len(s.writers))
	names := make([]string, 0, len(s.writers))
	for _, w := range s.writers {
		offs[w.name] = w.n
		names = append(names, w.name)
	}
	s.journal.Offsets = offs
	s.journal.Formats = names
}

func (s *sink) Close() {
	for _, w := range s.writers {
		if w.f != nil {
			// 先冲再关。缓冲里可能压着几千条评论，直接 Close 会把它们丢掉——
			// 出错路径上写的那一行 summary 就是这么消失的，
			// 而它恰恰是判断这份数据能不能用的唯一依据。
			_ = w.fmt.Flush()
			w.f.Close()
		}
	}
}

// ---- 收尾报告 ----

func reportPlan(g *globals, plan *bilibili.ReplyPlan, p bilibili.ReplyPolicy) {
	if p.IsDefault() {
		return
	}
	if p.Disabled {
		g.logf("已按 --no-replies 关闭楼中楼抓取")
		return
	}
	g.logf("决策：%d 楼中 %d 楼展开楼中楼，%d 楼留下墓碑行（%-s）",
		len(plan.Items), plan.ExpandCount(), plan.StubCount(), policyLabel(p))
}

func policyLabel(p bilibili.ReplyPolicy) string {
	var parts []string
	if p.Top > 0 {
		parts = append(parts, fmt.Sprintf("top %d", p.Top))
	}
	if p.MinLikes > 0 {
		parts = append(parts, fmt.Sprintf("min-likes %d", p.MinLikes))
	}
	if p.MinCount > 0 {
		parts = append(parts, fmt.Sprintf("min-count %d", p.MinCount))
	}
	out := ""
	for i, s := range parts {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

func reportFetch(g *globals, s *sink) {
	where := "已输出到 stdout"
	if s.dir != "" {
		where = "已写入目录 " + s.dir
	} else if s.primary != nil && s.primary.path != "" {
		where = "已写入 " + s.primary.path
	}

	if s.rootStopped == bilibili.StopEnd {
		g.logf("%s：一级评论 %d 条（服务端报告 %d 条）", where, s.rootFetched, s.rootExpected)
	} else {
		g.logf("%s：一级评论 %d 条后停止（%s），服务端报告共 %d 条",
			where, s.rootFetched, stopLabel(s.rootStopped), s.rootExpected)
	}

	if s.rootTruncated {
		g.logf("注意：一级评论只抓到了 %d/%d 条。单种排序最多翻到约 5000 条，"+
			"且未登录会被截断。后续版本会用双排序合并来突破这个上限。",
			s.rootFetched, s.rootExpected)
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
	case bilibili.StopOffsetLimit:
		return "超出服务端翻页上限"
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

// eta 估计楼中楼阶段要跑多久。
//
// 估算方式就是「总请求数 × 请求间隔」：每一楼的页数由它的回复数决定
// （`ceil(回复数/20)`，20 是服务端的硬上限），而所有请求都要排队过同一个
// 限速器。所以 **并发度不出现在公式里**——它填的是响应延迟留下的空档，
// 不提高请求速率。把它写进分母会得到一个乐观好几倍的数字。
//
// 这个数字是给「要不要加 --replies-top」用的，估错方向很要紧：报小了，
// 用户按这个预期按下回车，然后等上一个小时；报大了，用户白白放弃本来
// 抓得完的数据。所以宁可算准，不猜。
func eta(plan *bilibili.ReplyPlan, startIndex int, c *bilibili.Client) string {
	d := c.Delay()
	if d <= 0 {
		return "未知"
	}

	var pages int
	for i := startIndex; i < len(plan.Items); i++ {
		it := plan.Items[i]
		if !it.Expand {
			continue
		}
		n := it.ReplyCount
		if n < 1 {
			n = 1
		}
		p := (n + bilibili.ReplyPageSize - 1) / bilibili.ReplyPageSize
		if p > bilibili.MaxReplyPages {
			p = bilibili.MaxReplyPages // 超出 offset 上限的部分抓不到，不计入耗时
		}
		pages += p
	}

	total := time.Duration(pages) * d
	switch {
	case total > time.Hour:
		return fmt.Sprintf("%.1f 小时", total.Hours())
	case total > time.Minute:
		return fmt.Sprintf("%d 分钟", int(total.Minutes())+1)
	default:
		return fmt.Sprintf("%d 秒", int(total.Seconds())+1)
	}
}
