// Package annotate 给评论打提示标记。
//
// 这个包的立场是「标注而不是过滤」。
//
// 被 --min-likes 丢掉的 3000 条评论，下游看不见它们曾经存在；而带着 flags
// 的评论至少让 LLM 知道「这条是复读」「这条只有表情」。数据质量的取舍留给
// 下游做，工具负责把取舍的依据摆出来——因为工具不知道下游要拿数据干什么，
// 贸然丢弃就是在替下游做它没授权的决定。
//
// 标记写在 model.Comment.Flags 里，是纯粹的新增字段：不过滤、不重排、
// 不改变任何已有字段，下游不认识 flags 也可以完全忽略它。
package annotate

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ultramanDecker/bili-comment/internal/model"
)

// 标记名。
//
// 用常量而不是散落的字面量：这些字符串会写进输出文件，下游按它们做筛选，
// 改名就是破坏兼容。
const (
	// FlagEmojiOnly 表示正文里除了表情和标点什么都没有。
	FlagEmojiOnly = "emoji_only"
	// FlagShort 表示正文极短，通常只有情绪没有信息。
	FlagShort = "short"
	// FlagLottery 表示这是一条抽奖评论。
	FlagLottery = "lottery"
	// FlagRepeat 表示这条与之前某条评论近似相同，也就是「复读」。
	FlagRepeat = "repeat"
)

// ShortRunes 是「极短」的字数上限。
//
// 取 5 是因为中文五个字以内基本只够说「顶」「第一」「哈哈哈哈」——有情绪，
// 没有观点。真正有内容的评论几乎不可能在五个字内说完。
//
// 注意这是**标记**不是过滤：短评在「大家用什么语气说话」这类问题里是有效数据，
// 只是在「大家在讨论什么」里是噪声。要不要用由下游决定。
const ShortRunes = 5

// RepeatDistance 是判定复读的 SimHash 汉明距离上限。
//
// 取 3 是 SimHash 的常规取值：能抓住改一两个字、多一个表情的变体，
// 又不会把「说得像但意思不同」的两条算成同一条。
const RepeatDistance = 3

// Collector 在流式写入的过程中给评论打标记。
//
// 它是有状态的——复读检测要看「之前」的评论。所以续传时不能让它从空开始，
// 否则断点之后的评论与前缀里的评论比不出重复。上层会在续传时把前缀喂给
// Observe 一次，把索引补回来，见 cli.seedCollector。
type Collector struct {
	pub time.Time
	idx *repeatIndex

	// repeats 是本次被标记为复读的条数，用于收尾提示。
	repeats int
}

// New 建一个打标器。pub 是视频发布时间，用来算相对时间。
func New(pub time.Time) *Collector {
	return &Collector{pub: pub, idx: newRepeatIndex()}
}

// Repeats 返回被标记为复读的评论数。
func (c *Collector) Repeats() int { return c.repeats }

// Mark 给一条评论打上标记，就地修改它。
//
// 注意它**不是幂等的**：同一个 *Comment 调两次会打上两份重复的 flags。
// 因此调用方对每条评论只能调一次，而续传补索引（Observe）只能喂那些
// 不会写进输出的对象——也就是从已有文件里读回来的那一份，
// 而不是内存里那份稍后还要再写一次的对象。这个约束由 cli.seedCollector
// 保证：它读的是文件，读出来的评论不会再经过 writeComment。
func (c *Collector) Mark(cm *model.Comment) {
	// 相对时间是给 LLM 的：模型对「5年前」的理解远好于对 1606652805 的理解，
	// 而让它自己做减法既费 token 又容易算错。绝对时间戳仍然保留，
	// 相对时间只是多一个视角，不替代它。
	if !c.pub.IsZero() && !cm.Ctime.IsZero() {
		cm.Rel = RelTime(cm.Ctime.Time(), c.pub)
	}

	msg := strings.TrimSpace(cm.Message)

	if IsEmojiOnly(msg) {
		cm.Flags = append(cm.Flags, FlagEmojiOnly)
		// 纯表情评论不进复读索引。
		//
		// 它们天然就是「同一批表情刷出来的」，进索引的话会把复读榜刷满，
		// 真正需要看见的「同一句话被刷了几百遍」反而被淹掉。而它们本来
		// 已经带着 emoji_only 这个更准确的标记了，不差 repeat 这一个。
		return
	}
	if utf8.RuneCountInString(msg) <= ShortRunes {
		cm.Flags = append(cm.Flags, FlagShort)
	}
	if IsLottery(msg) {
		cm.Flags = append(cm.Flags, FlagLottery)
	}
	if c.idx.add(msg) {
		cm.Flags = append(cm.Flags, FlagRepeat)
		c.repeats++
	}
}

// Observe 把一条历史评论喂进索引，但不产生任何输出。
//
// 续传时用它把上一次已经写进文件的那部分补回索引里。不这么做的话，
// 断点之后的评论只会与断点之后的评论比，前缀里的复读源就漏掉了——
// 表现是「续传跑出来的文件里，前半段的复读被标了，后半段同样的复读没标」。
func (c *Collector) Observe(cm *model.Comment) {
	c.Mark(cm)
}

// RelTime 把绝对时间换算成相对视频发布时间的说法。
//
// 粒度刻意做粗（分钟/小时/天/个月/年），因为下游要的是量级感：
// 「视频发布几小时后出现的高赞评论」是一个有意义的观察，
// 「视频发布后 3 小时 47 分」不是。
func RelTime(t, pub time.Time) string {
	if pub.IsZero() || t.IsZero() {
		return ""
	}
	d := t.Sub(pub)
	if d < 0 {
		// 评论时间早于发布时间只可能是接口数据有问题，或者视频改过发布时间。
		// 不编造说法，留空。
		return ""
	}
	switch {
	case d < time.Minute:
		return "刚刚"
	case d < time.Hour:
		return fmt.Sprintf("%d分钟", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d小时", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%d天", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%d个月", int(d.Hours()/(24*30)))
	default:
		return fmt.Sprintf("%d年", int(d.Hours()/(24*365)))
	}
}

// IsEmojiOnly 判断正文是否只有表情与标点。
//
// 判据是「把 [表情名] 整段拿掉之后，剩下的字符全是标点或符号」。
// 空正文也算——纯图片评论没有 message，它同样没有可分析的文本。
//
// 不把 [表情名] 里的字算进正文是关键：「[支持]」两个字不是用户在说话，
// 是他在点一个按钮。算进去的话这条评论就成了「2 个字的短评」，
// 会跟「顶」「哈哈」混在一起，而它们其实是两类东西。
func IsEmojiOnly(msg string) bool {
	rest := stripEmotes(msg)
	for _, r := range rest {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
		// 标点、符号（含 emoji）都算「没有说话」。
		if !unicode.IsPunct(r) && !unicode.IsSymbol(r) && !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// stripEmotes 去掉 [xxx] 形式的表情名。
//
// 只认单层方括号：B 站的表情名里不会出现嵌套括号，而按嵌套去解析反而会把
// 「[笑[哭]]」这种正常文本吃掉。找不到右括号时原样保留——
// 宁可把表情当正文，也不要把正文当表情丢掉。
func stripEmotes(s string) string {
	if !strings.ContainsRune(s, '[') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r != '[' {
			b.WriteRune(r)
			i += size
			continue
		}
		end := strings.IndexByte(s[i+size:], ']')
		if end < 0 {
			b.WriteString(s[i:])
			break
		}
		i += size + end + 1
	}
	return b.String()
}

// 抽奖评论的特征词。
//
// 这类评论是固定格式的模板文，对分析评论区观点毫无价值，但数量可以很大。
// 判据刻意保守：宁可漏掉几条，也不要把正常的「我中奖了」这种真实发言标记掉。
var lotteryWords = []string{"抽奖", "转发抽", "参与抽", "中奖名单"}

// IsLottery 判断是不是抽奖评论。
//
// 两条判据：命中特征词，或者同时出现「转发」与「关注」——后者是抽奖模板
// 的标准句式（「转发+关注，抽一人送……」）。分开看的话，单说「转发」的
// 评论太常见了，单说「关注」也是。
func IsLottery(msg string) bool {
	for _, w := range lotteryWords {
		if strings.Contains(msg, w) {
			return true
		}
	}
	return strings.Contains(msg, "转发") && strings.Contains(msg, "关注")
}
