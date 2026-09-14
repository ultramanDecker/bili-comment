package output

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/ultramanDecker/bili-comment/internal/model"
)

// Markdown 写出带层级的、人和 LLM 都能读的版本。
//
// 它和 TSV 的分工是：TSV 胜在密度，用来「读完全部」；Markdown 胜在结构，
// 用来「读懂一条讨论」。LLM 对缩进列表的理解通常好于对嵌套 JSON 的理解，
// 而人更是如此。
//
// 章节顺序是抓取顺序决定的，不是排版偏好：一级评论先于楼中楼到达，
// 所以楼中楼只能另起一节。好在楼中楼本来就是按楼分组的，
// 「哪几条回复属于哪栋楼」这个结构一点没丢。
type Markdown struct {
	bw *bufio.Writer

	// roots 记住每栋楼的楼主信息，供楼中楼章节做小节标题。
	//
	// 存一个精简副本而不是整条评论：楼中楼章节要用到的只有楼主是谁、
	// 这楼多热，而完整评论里的正文可能很长，为了一段标题把几万条正文
	// 留在内存里不划算。
	roots map[uint64]rootBrief
	order []uint64

	// 当前正在攒的楼。楼中楼是按楼成组到达的（RunOrdered 保证顺序），
	// 所以只要缓存当前这一组，遇到下一组时把上一组吐出去。
	groupRoot uint64
	group     []*model.Comment

	rootCount  int
	firstRoot  bool
	firstGroup bool
}

type rootBrief struct {
	name  string
	like  int
	ctime model.Time
	rel   string
	count int
}

// 缩进上限。真实讨论的深度很少超过四五层，而数据里 parent 指向一条
// 已被删除的评论是常事，那种情况下链条会断——设个上限纯粹是防御
// 数据里出现环。
const maxDepth = 8

func (f *Markdown) Name() string    { return "md" }
func (f *Markdown) Ext() string     { return "md" }
func (f *Markdown) Schema() []Field { return CommentSchema() }
func (f *Markdown) Flush() error    { return f.bw.Flush() }

func (f *Markdown) Begin(w io.Writer, m *Meta) error {
	f.bw = bufio.NewWriter(w)
	f.roots = map[uint64]rootBrief{}
	f.firstRoot = true
	f.firstGroup = true
	if m.Resuming {
		// 标题与简介在上次保留的前缀里，不能重写。
		return nil
	}

	if m.Video != nil {
		v := m.Video
		fmt.Fprintf(f.bw, "# 《%s》评论\n\n", v.Title)
		fmt.Fprintf(f.bw, "> `%s` · UP **%s** · 发布 %s · 服务端报告 %d 条评论\n",
			v.BVID, v.UpName, v.PubTime.Time().Format("2006-01-02"), v.StatReply)
		fmt.Fprintf(f.bw, "> 抓取于 %s，%s 排序\n\n",
			v.FetchedAt.Time().Format("2006-01-02 15:04"), m.Mode)
	}
	return nil
}

func (f *Markdown) Write(r Row) error {
	switch r.Kind {
	case KindComment:
		if r.Comment == nil {
			return nil
		}
		if r.Comment.Root == 0 {
			return f.writeRoot(r.Comment)
		}
		return f.bufferReply(r.Comment)
	case KindStub:
		// 墓碑在 Markdown 里不逐条列出，只汇总到文末的完整性一节。
		// 这一节本来就是给「先看规模再决定要不要细看」用的，
		// 逐条列出三千栋楼会把要读的内容淹掉。
		return nil
	default:
		return nil
	}
}

func (f *Markdown) writeRoot(cm *model.Comment) error {
	if err := f.flushGroup(); err != nil {
		return err
	}
	if f.firstRoot {
		f.bw.WriteString("## 一级评论\n\n")
		f.firstRoot = false
	}

	f.roots[cm.Rpid] = rootBrief{
		name: cm.User.Name, like: cm.Like, ctime: cm.Ctime, rel: cm.Rel,
		count: cm.ReplyCount,
	}
	f.rootCount++

	f.writeHeading(cm, f.rootCount)
	f.writeBody(cm, "  ")
	f.bw.WriteByte('\n')
	return nil
}

// writeHeading 写一条一级评论的标题行。
//
// 把点赞数、时间、回复数都塞进标题，是为了让「扫一遍标题」这件事有意义：
// 很多时候读者（人或模型）只需要知道哪几栋楼值得细看，标题就是索引。
func (f *Markdown) writeHeading(cm *model.Comment, n int) {
	fmt.Fprintf(f.bw, "### %d. %s · 👍%d · %s", n, cm.User.Name, cm.Like, dateCell(cm))
	if cm.ReplyCount > 0 {
		fmt.Fprintf(f.bw, " · %d 条回复", cm.ReplyCount)
	}
	if fl := Flags(cm); fl != "" {
		fmt.Fprintf(f.bw, " · `%s`", fl)
	}
	f.bw.WriteByte('\n')
}

func (f *Markdown) bufferReply(cm *model.Comment) error {
	if cm.Root != f.groupRoot {
		if err := f.flushGroup(); err != nil {
			return err
		}
		f.groupRoot = cm.Root
	}
	f.group = append(f.group, cm)
	return nil
}

// flushGroup 把攒好的楼中楼写出去。
//
// 必须整组攒完才能写：层级要靠 parent 链条还原，而一条回复的上一层可能
// 出现在它后面（先生成了子回复、后生成父回复的时间倒挂是有的），
// 边收边写的话那时候还不知道该缩进几格。
func (f *Markdown) flushGroup() error {
	if len(f.group) == 0 {
		return nil
	}
	group := f.group
	f.group = nil

	if f.firstGroup {
		f.bw.WriteString("## 楼中楼\n\n")
		f.firstGroup = false
	}

	brief := f.roots[f.groupRoot]
	fmt.Fprintf(f.bw, "### ↳ %s 的楼 · %s\n\n", brief.name, replyLabel(brief, len(group)))

	byRpid := make(map[uint64]*model.Comment, len(group))
	for _, cm := range group {
		byRpid[cm.Rpid] = cm
	}
	// 楼主那条评论本身不在楼中楼数据里，但要作为父链条的终点参与判断，
	// 否则「直接回复楼主」和「回复楼里另一个人」分不出来。
	for _, cm := range group {
		d := depthOf(cm, byRpid, f.groupRoot)
		indent := strings.Repeat("  ", d)
		fmt.Fprintf(f.bw, "%s- **%s** · 👍%d · %s", indent, cm.User.Name, cm.Like, dateCell(cm))
		if fl := Flags(cm); fl != "" {
			fmt.Fprintf(f.bw, " · `%s`", fl)
		}
		f.bw.WriteByte('\n')
		f.writeBody(cm, indent+"  ")
	}
	f.bw.WriteString("\n")
	return nil
}

// writeBody 写评论正文，按缩进对齐。
//
// 正文里的换行原样保留并重新缩进：有人会在评论里写分行的小作文，
// 把换行压掉会改变它的意思。
func (f *Markdown) writeBody(cm *model.Comment, indent string) {
	for _, line := range strings.Split(strings.TrimRight(cm.Message, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f.bw.WriteString(indent)
		f.bw.WriteString(line)
		f.bw.WriteByte('\n')
	}
	for _, p := range cm.Pictures {
		fmt.Fprintf(f.bw, "%s![](%s)\n", indent, p)
	}
}

func (f *Markdown) End(s *Summary) error {
	if err := f.flushGroup(); err != nil {
		return err
	}
	f.bw.WriteString("---\n\n## 完整性\n\n")
	if s == nil {
		f.bw.WriteString("本次抓取没有写出收尾统计（可能是出错中断）。\n")
		return f.bw.Flush()
	}

	if f.rootCount == 0 {
		// 一条一级评论都没有的时候补一个章节标题，否则整份文件看起来
		// 像是内容丢了，而实际是「这个视频没有评论」。
		f.bw.WriteString("（没有抓到一级评论。）\n\n")
	}
	fmt.Fprintf(f.bw, "- 一级评论：**%d / %d**", s.Fetched, s.Expected)
	if s.Truncated {
		fmt.Fprintf(f.bw, "，被截断（%s）", ReasonText(s.Reason))
	}
	f.bw.WriteByte('\n')

	if r := s.Replies; r != nil {
		fmt.Fprintf(f.bw, "- 楼中楼：**%d / %d**，展开 %d 栋，未展开 %d 栋",
			r.Fetched, r.Expected, r.Expanded, r.Skipped)
		if r.Truncated {
			f.bw.WriteString("，被截断")
		}
		f.bw.WriteByte('\n')
		if r.SkippedReplies > 0 {
			fmt.Fprintf(f.bw, "- 未展开的 %d 栋楼自称共有 **%d** 条回复，本文件里没有这些数据\n",
				r.Skipped, r.SkippedReplies)
			for _, rc := range r.ReasonsSorted() {
				fmt.Fprintf(f.bw, "  - %s：%d 栋\n", SkipReasonText(rc.Reason), rc.Count)
			}
		}
	}
	if s.Error != "" {
		fmt.Fprintf(f.bw, "- 中断原因：%s\n", s.Error)
	}
	f.bw.WriteString("\n基于本文档得出「评论区主流观点是……」之前，请先确认上面的完整性数据。\n")
	return f.bw.Flush()
}

func dateCell(cm *model.Comment) string {
	d := cm.Ctime.Time().Format("2006-01-02")
	if cm.Rel != "" {
		return fmt.Sprintf("%s（%s）", d, cm.Rel)
	}
	return d
}

func replyLabel(b rootBrief, got int) string {
	if b.count > 0 && b.count != got {
		return fmt.Sprintf("%d 条回复（服务端报告 %d）", got, b.count)
	}
	return fmt.Sprintf("%d 条回复", got)
}

// depthOf 算一条回复在楼里的缩进层级。
//
// 链条断掉是常态而不是异常：被回复的那条可能已被删除，或者根本没被抓到
// （比如它不在前 20 条里）。断掉时就停在已知的深度上，不猜。
func depthOf(cm *model.Comment, byRpid map[uint64]*model.Comment, root uint64) int {
	d := 0
	cur := cm
	for cur.Parent != 0 && cur.Parent != root && d < maxDepth {
		p, ok := byRpid[cur.Parent]
		if !ok {
			break
		}
		cur = p
		d++
	}
	return d
}

// SkipReasonText 把跳过原因翻译成人话。
func SkipReasonText(reason string) string {
	switch reason {
	case "not_in_top_n":
		return "不在 --replies-top 选中的范围内"
	case "below_threshold":
		return "未达到 --replies-min-likes / --replies-min-count 的门槛"
	case "no_replies":
		return "本来就没有回复"
	case "replies_disabled":
		return "已用 --no-replies 关闭"
	default:
		return reason
	}
}
