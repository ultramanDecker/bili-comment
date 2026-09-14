package output

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ultramanDecker/bili-comment/internal/model"
)

// 字段元信息。TSV 的表头、schema.md 的内容、以及各格式之间的一致性
// 都以这张表为准——想加一个列，只改这里。
//
// 顺序即列顺序。把短的、常用来筛选的排在前面，TSV 用 less 翻看时更容易扫，
// 而且前几列就能决定要不要读这一行的剩下部分。
var commentFields = []Field{
	{"rpid", "int", "评论 id，全站唯一"},
	{"root", "int", "所属一级评论的 rpid；空表示这条自己就是一级评论"},
	{"parent", "int", "被直接回复的那条评论的 rpid；空表示直接回复一级评论"},
	{"mid", "int", "发布者 uid"},
	{"name", "str", "发布者昵称"},
	{"like", "int", "点赞数"},
	{"ctime", "time", "发布时间（Unix 秒）"},
	{"rel", "str", "相对视频发布时间，如「3小时」「2年」"},
	{"reply_count", "int", "这条评论的楼的回复总数（服务端口径，是上界）"},
	{"location", "str", "IP 属地，老评论没有这个字段"},
	{"flags", "flags", "提示标记，| 分隔，见下方说明"},
	{"message", "str", "正文。制表符与换行被转义成 \\t \\n"},
}

// CommentSchema 返回评论记录的字段元信息。
func CommentSchema() []Field { return commentFields }

// CommentValues 按 schema 的顺序取出一条评论的值。
//
// 与 commentFields 放在一起，是为了让「加字段」这件事只需要改相邻的两处。
// 两处不同步的话，TSV 的表头与数据会错位，而这种错位不会报错，
// 只会让下游把点赞数当成 uid 用。
func CommentValues(cm *model.Comment) []string {
	return []string{
		strconv.FormatUint(cm.Rpid, 10),
		optUint(cm.Root),
		optUint(cm.Parent),
		strconv.FormatInt(cm.User.Mid, 10),
		cm.User.Name,
		strconv.Itoa(cm.Like),
		strconv.FormatInt(cm.Ctime.Time().Unix(), 10),
		cm.Rel,
		optInt(cm.ReplyCount),
		cm.Location,
		Flags(cm),
		cm.Message,
	}
}

func optUint(v uint64) string {
	if v == 0 {
		return ""
	}
	return strconv.FormatUint(v, 10)
}

func optInt(v int) string {
	if v == 0 {
		return ""
	}
	return strconv.Itoa(v)
}

// flagDocs 解释每一个标记的含义。
//
// 这些标记是工具对数据的判断，判断依据必须写在数据旁边：只写 "repeat"
// 而不说怎么算出来的，下游要么盲信要么无视，两种都不对。
var flagDocs = []struct{ Name, Desc string }{
	{"up", "UP 主本人发的评论"},
	{"top", "置顶评论"},
	{"emoji_only", "正文只有表情与标点，没有可分析的文本（纯图片评论也属于这类）"},
	{"short", "正文 ≤ 5 字，通常只有情绪没有观点"},
	{"lottery", "抽奖评论，模板化文本"},
	{"repeat", "与之前某条评论近似相同（SimHash 汉明距离 ≤ 3）"},
}

// FlagDoc 返回标记说明，供 schema.md 使用。
func FlagDoc() []struct{ Name, Desc string } { return flagDocs }

// SchemaMarkdown 生成 schema.md 的内容。
//
// 它存在的理由很实际：这份数据可能在下个月被另一个人的 LLM 读到，
// 那时没有任何人能解释「rel 是什么」「被过滤掉的楼去哪了」。
// 自描述不是体面，是可用性的前提。
func SchemaMarkdown(m *Meta, s *Summary) string {
	var b strings.Builder

	b.WriteString("# bili-comment 数据集\n\n")
	if m != nil && m.Video != nil {
		v := m.Video
		fmt.Fprintf(&b, "《%s》\n\n", v.Title)
		fmt.Fprintf(&b, "- BV 号：`%s`（aid %d）\n", v.BVID, v.AID)
		fmt.Fprintf(&b, "- UP 主：%s（uid %d）\n", v.UpName, v.UpMid)
		fmt.Fprintf(&b, "- 视频发布：%s\n", v.PubTime.Time().Format("2006-01-02 15:04:05"))
		fmt.Fprintf(&b, "- 服务端报告评论数：%d\n", v.StatReply)
		fmt.Fprintf(&b, "- 抓取排序：%s\n", m.Mode)
		fmt.Fprintf(&b, "- 抓取时间：%s\n", v.FetchedAt.Time().Format("2006-01-02 15:04:05"))
	}

	if s != nil {
		b.WriteString("\n## 完整性\n\n")
		fmt.Fprintf(&b, "- 一级评论：%d / %d\n", s.Fetched, s.Expected)
		if s.Truncated {
			fmt.Fprintf(&b, "- **一级评论被截断**：%s\n", ReasonText(s.Reason))
		}
		if r := s.Replies; r != nil {
			fmt.Fprintf(&b, "- 楼中楼：%d / %d，展开 %d 栋，跳过 %d 栋\n",
				r.Fetched, r.Expected, r.Expanded, r.Skipped)
			if r.SkippedReplies > 0 {
				fmt.Fprintf(&b, "- 跳过的那 %d 栋楼自称共有 %d 条回复，这些数据不在本数据集里\n",
					r.Skipped, r.SkippedReplies)
			}
			if r.Truncated {
				b.WriteString("- **楼中楼被截断**\n")
			}
		}
		b.WriteString("\n完整性的含义见文末「完整性说明」。\n")
	}

	b.WriteString("\n## 字段\n\n")
	b.WriteString("| 字段 | 类型 | 说明 |\n|---|---|---|\n")
	for _, f := range commentFields {
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", f.Name, f.Type, f.Desc)
	}

	b.WriteString("\n## flags 标记\n\n")
	b.WriteString("`flags` 用 `|` 分隔，可能同时有多个。标记是**提示不是过滤**：\n")
	b.WriteString("带标记的评论仍然在数据里，怎么用由下游决定。\n\n")
	b.WriteString("| 标记 | 含义 |\n|---|---|\n")
	for _, f := range flagDocs {
		fmt.Fprintf(&b, "| `%s` | %s |\n", f.Name, f.Desc)
	}

	b.WriteString(`
## 完整性说明

这份数据**可能是不完整的**，所有下游结论都应该先看上面的完整性一节。

- **一级评论**受 --limit 与排序上限（每种排序方式约 5000 条）限制。
- **楼中楼**默认全展开，但 --replies-top 之类的参数会主动跳过一部分。
  被跳过的楼会留下墓碑行，写明它有多少条回复、为什么没抓——
  下游因此能分辨「这楼没有回复」和「这楼有 802 条回复但我们没抓」。
- 两种回复数不可比：墓碑行里的 reply_count 来自评论对象，是**上界**；
  完整性里的 expected 来自楼中楼接口，是**真正能抓到的**条数，
  实测前者比后者多 13–19%，差额是已删除或被过滤的回复，不代表抓取漏了。
- **truncated 只管一级评论，楼中楼有它自己的 replies.truncated。**
  判断「这份数据能不能用」时两个都要看。json 格式的 complete 字段已经替你把
  两者算在一起了，其他格式没有这个字段，别只看一个。

基于部分评论得出「评论区主流观点是 X」这类结论之前，请先确认完整性数据。
`)
	return b.String()
}

// ReasonText 把停止原因翻译成人话。
func ReasonText(reason string) string {
	switch reason {
	case "limit":
		return "达到 --limit 设定的条数"
	case "since":
		return "到达 --since 指定的时间边界"
	case "end":
		return "服务端表示已经到底"
	case "offset_limit":
		return "触及服务端的翻页上限（约 5000 条）"
	case "cursor_stuck":
		return "游标不再前进（继续翻会死循环）"
	case "error":
		return "出错中断"
	case "":
		return "未知"
	default:
		return reason
	}
}
