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
	"time"

	"github.com/spf13/cobra"

	"github.com/ultramanDecker/bili-comment/internal/bilibili"
	"github.com/ultramanDecker/bili-comment/internal/model"
	"github.com/ultramanDecker/bili-comment/internal/store"
)

func newCommentsCmd(g *globals) *cobra.Command {
	var (
		out   string
		limit int
		mode  string
		since string
		full  bool

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
				out: out, limit: limit, mode: mode, since: since, full: full,
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
	fl.StringVarP(&out, "out", "o", "", "写入文件而非打印到 stdout")
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

	roots, resumeIndex, err := phaseRoots(ctx, g, c, out, f, video, commentMode, since, cmd)
	if err != nil {
		out.writeSummary(summary{Type: "summary", Reason: "error", Error: err.Error()})
		return err
	}

	// ---- 决策点 ----

	plan := bilibili.BuildPlan(roots, f.policy)
	reportPlan(g, plan, f.policy)

	// ---- 阶段二：楼中楼 ----

	rep, err := phaseReplies(ctx, g, c, out, f, video, plan, resumeIndex)
	if err != nil {
		out.writeSummary(summary{
			Type: "summary", Reason: "error", Error: err.Error(), Replies: rep,
		})
		return err
	}

	// ---- 收尾 ----

	out.writeSummary(summary{
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

	// 只有完整跑完才删进度文件。中途出错时留着，下次才能续。
	if f.out != "" {
		if err := store.Remove(f.out); err != nil {
			g.logf("警告：清理进度文件失败：%v", err)
		}
	}
	reportFetch(g, f.out, out)
	return nil
}

// phaseRoots 抓取一级评论并写入输出。返回评论列表，以及楼中楼阶段该从第几个计划项开始。
func phaseRoots(
	ctx context.Context, g *globals, c *bilibili.Client, out *sink,
	f commentsFlags, video *model.Video, commentMode bilibili.CommentMode,
	since time.Time, cmd *cobra.Command,
) ([]*model.Comment, int, error) {
	// 一级评论阶段已经完成：文件里就有全部一级评论，读回来重建计划即可，
	// 不必为了算计划再抓一遍。
	if out.resuming && out.journal.Phase == store.PhaseReplies {
		roots, err := readRootsFrom(out.path)
		if err != nil {
			return nil, 0, err
		}
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
	} else {
		if err := out.writeHeader(header{
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
			return nil, 0, err
		}
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
			if !f.full {
				trimForLLM(cm)
			}
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
		if f.out == "" {
			return nil
		}
		out.journal.Phase = store.PhaseRoots
		out.journal.Offset = out.n
		out.journal.RootCursor = cur.NextOffset
		g.logf("已抓取 %d 条一级评论", out.rootFetched)
		return out.journal.Save(f.out)
	})
	if err != nil {
		return roots, 0, err
	}
	out.rootExpected = res.Expected
	out.rootFetched = res.Fetched
	out.rootPages = res.Pages
	out.rootStopped = res.Stopped
	out.rootTruncated = res.Truncated

	if f.out != "" {
		// 一级评论阶段完成，进度切到楼中楼阶段。此时 RootCursor 不再有意义，
		// 要清掉，否则下次误以为还能从游标续抓。
		out.journal.Phase = store.PhaseReplies
		out.journal.RootCursor = ""
		out.journal.PlanIndex = 0
		out.journal.Offset = out.n
		out.journal.PolicyHash = f.policy.Fingerprint()
		// 一级评论的统计在这里定稿。之后即便续传时不再重抓一级评论，
		// summary 与收尾提示也要靠它们说话，所以必须落盘。
		out.journal.RootExpected = res.Expected
		out.journal.RootFetched = res.Fetched
		out.journal.RootPages = res.Pages
		out.journal.RootStopped = string(res.Stopped)
		out.journal.RootTruncated = res.Truncated
		if err := out.journal.Save(f.out); err != nil {
			return roots, 0, err
		}
	}
	return roots, 0, nil
}

// phaseReplies 按计划展开楼中楼。
func phaseReplies(
	ctx context.Context, g *globals, c *bilibili.Client, out *sink,
	f commentsFlags, video *model.Video, plan *bilibili.ReplyPlan, startIndex int,
) (*replySummary, error) {
	rep := &replySummary{
		RootsWithReplies: countRootsWithReplies(plan),
		Skipped:          plan.StubCount(),
		Reasons:          map[string]int{},
	}
	for _, it := range plan.Items {
		if !it.Expand && it.Skip != bilibili.SkipNoReplies {
			rep.Reasons[string(it.Skip)]++
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
			if err := out.writeStub(stub{
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
		if f.out != "" {
			out.journal.StubsWritten = true
			out.journal.Offset = out.n
			if err := out.journal.Save(f.out); err != nil {
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
				if !f.full {
					trimForLLM(cm)
				}
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
			if f.out != "" {
				out.journal.PlanIndex = idx + 1
				out.journal.Offset = out.n
				out.journal.ReplyFetched = rep.Fetched
				out.journal.ReplyExpected = rep.Expected
				out.journal.ReplyOffsetLimited = rep.OffsetLimited
				out.journal.ReplyTruncated = rep.Truncated
				if err := out.journal.Save(f.out); err != nil {
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

// ---- 输出 ----

// sink 是 JSONL 输出的唯一出口：它同时维护「已写入字节数」，
// 这个计数就是续传的截断位置，所以所有写入都必须经过它。
type sink struct {
	path string
	f    *os.File // 仅在写文件时非 nil
	bw   *bufio.Writer
	enc  *json.Encoder
	n    int64 // 已写入的字节数

	journal *store.Journal

	// resuming 表示本次是接着已有的进度跑的，与「journal 非 nil」不是一回事：
	// 全新抓取也会建一个 journal 用来写进度。把两者混为一谈会导致
	// 全新抓取误以为自己在中途，从而跳过 header 行的写入。
	resuming bool

	rootExpected  int
	rootFetched   int
	rootPages     int
	rootStopped   bilibili.StopReason
	rootTruncated bool
}

func openSink(cmd *cobra.Command, g *globals, f commentsFlags, video *model.Video) (*sink, error) {
	s := &sink{}

	if f.out == "" {
		s.bw = bufio.NewWriter(cmd.OutOrStdout())
		s.enc = json.NewEncoder(s.bw)
		return s, nil
	}

	s.path = f.out
	if err := store.EnsureDir(f.out); err != nil {
		return nil, err
	}

	if f.resume {
		j, err := store.LoadJournal(f.out)
		switch {
		case errors.Is(err, store.ErrNoJournal):
			g.logf("没有找到进度文件，将开始一次全新的抓取")
		case err != nil:
			return nil, err
		default:
			// 策略变了就换了计划，进度下标会指向另一栋楼——必须拦住，
			// 否则续传出来的数据是错位的。
			if j.Phase == store.PhaseReplies && j.PolicyHash != "" &&
				j.PolicyHash != f.policy.Fingerprint() {
				return nil, fmt.Errorf("进度文件记录的楼中楼参数与本次不一致，无法续传。"+
					"要么用回原来的参数，要么换一个输出文件重新抓（%s）", store.JournalPath(f.out))
			}
			fh, err := store.TruncateOutput(f.out, j.Offset)
			if err != nil {
				return nil, err
			}
			s.f = fh
			s.journal = j
			s.resuming = true
			s.n = j.Offset
			g.logf("续传：输出已回退到第 %d 字节（丢弃上次未写完的部分）", j.Offset)
		}
	}

	if s.f == nil {
		fh, err := os.Create(f.out)
		if err != nil {
			return nil, fmt.Errorf("创建 %s 失败：%w", f.out, err)
		}
		s.f = fh
		s.journal = &store.Journal{AID: video.AID, BVID: video.BVID}
	}

	s.bw = bufio.NewWriter(s.f)
	s.enc = json.NewEncoder(countingWriter{s.bw, &s.n})
	return s, nil
}

// countingWriter 统计写出的字节数。续传依赖它给出的位置，
// 所以它必须包在 bufio 之外——buffer 里的内容还没落盘，不能算数，
// Flush 之后计数才等于文件长度。
type countingWriter struct {
	w io.Writer
	n *int64
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	*c.n += int64(n)
	return n, err
}

func (s *sink) writeHeader(h header) error { return s.enc.Encode(h) }
func (s *sink) writeComment(cm *model.Comment) error {
	return s.enc.Encode(cm)
}
func (s *sink) writeStub(st stub) error { return s.enc.Encode(st) }
func (s *sink) writeSummary(sm summary) { _ = s.enc.Encode(sm) }

func (s *sink) Flush() error {
	if err := s.bw.Flush(); err != nil {
		return err
	}
	if s.f != nil {
		// 数据要真的到磁盘上，续传读到的才算数。断电时靠 fsync 保证。
		return s.f.Sync()
	}
	return nil
}

func (s *sink) Close() {
	if s.f != nil {
		s.f.Close()
	}
}

// ---- 输出的行结构 ----

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

// stub 是未展开楼的墓碑行。
//
// 过滤是破坏性的且对下游不可见。LLM 看到这行就知道「这楼有 37 条我没看到」，
// 结论自然会谨慎；看不到这行则会以为评论区只有它读到的那些。
// 成本是每楼一行。
type stub struct {
	Type       string `json:"type"`
	Rpid       uint64 `json:"rpid"`
	ReplyCount int    `json:"reply_count"`
	Expanded   bool   `json:"expanded"`
	Reason     string `json:"reason"`
}

// summary 是 JSONL 的最后一行，记录这次抓取到底拿到了多少、为什么停下。
//
// 这一行是整个输出的关键：下游拿到一份残缺数据却不知道它残缺，
// 比拿不到数据更危险——基于部分评论得出的「用户普遍认为」是纯粹的幻觉。
type summary struct {
	Type      string        `json:"type"`
	Expected  int           `json:"expected"`
	Fetched   int           `json:"fetched"`
	Pages     int           `json:"pages"`
	Reason    string        `json:"reason"`
	Truncated bool          `json:"truncated,omitempty"`
	Error     string        `json:"error,omitempty"`
	Replies   *replySummary `json:"replies,omitempty"`
}

// replySummary 是楼中楼的完整性元数据。
//
// 期望值与实际值分开报：Expected 是「计划展开的那些楼一共该有多少条回复」，
// Fetched 是实际拿到的。只报 Fetched 的话，抓了一半看起来也像完整。
type replySummary struct {
	Expected         int            `json:"expected"`
	Fetched          int            `json:"fetched"`
	Expanded         int            `json:"expanded"`
	Skipped          int            `json:"skipped,omitempty"`
	RootsWithReplies int            `json:"roots_with_replies"`
	OffsetLimited    int            `json:"offset_limited,omitempty"`
	Truncated        bool           `json:"truncated,omitempty"`
	Reasons          map[string]int `json:"skip_reasons,omitempty"`
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

func reportFetch(g *globals, out string, s *sink) {
	where := "已输出到 stdout"
	if out != "" {
		where = "已写入 " + out
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
