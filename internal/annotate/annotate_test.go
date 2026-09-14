package annotate

import (
	"strings"
	"testing"
	"time"

	"github.com/ultramanDecker/bili-comment/internal/model"
)

var pub = time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)

func cm(msg string) *model.Comment {
	return &model.Comment{Rpid: 1, Message: msg, Ctime: model.Time(pub.Add(time.Hour))}
}

func hasFlag(c *model.Comment, want string) bool {
	for _, f := range c.Flags {
		if f == want {
			return true
		}
	}
	return false
}

func flagLine(c *model.Comment) string { return strings.Join(c.Flags, "|") }

// 相对时间要给的是量级感，「发布后 3 小时 47 分」不是有用的信息，
// 「发布后 3 小时」才是。所以每个分档的边界都要落在它该落的地方。
func TestRelTimeBuckets(t *testing.T) {
	cases := []struct {
		after time.Duration
		want  string
	}{
		{0, "刚刚"},
		{30 * time.Second, "刚刚"},
		{time.Minute, "1分钟"},
		{59 * time.Minute, "59分钟"},
		{time.Hour, "1小时"},
		{23 * time.Hour, "23小时"},
		{24 * time.Hour, "1天"},
		{29 * 24 * time.Hour, "29天"},
		{30 * 24 * time.Hour, "1个月"},
		{364 * 24 * time.Hour, "12个月"},
		{365 * 24 * time.Hour, "1年"},
		{3 * 365 * 24 * time.Hour, "3年"},
	}
	for _, c := range cases {
		if got := RelTime(pub.Add(c.after), pub); got != c.want {
			t.Errorf("发布后 %v = %q，期望 %q", c.after, got, c.want)
		}
	}
}

// 时间倒挂只可能是接口数据有问题或视频改过发布时间。宁可留空，
// 也不要编出一个「-1小时」或者「刚刚」来掩盖它。
func TestRelTimeRefusesBadInput(t *testing.T) {
	if got := RelTime(pub.Add(-time.Hour), pub); got != "" {
		t.Errorf("早于发布时间 = %q，期望空", got)
	}
	if got := RelTime(pub, time.Time{}); got != "" {
		t.Errorf("没有发布时间 = %q，期望空", got)
	}
	if got := RelTime(time.Time{}, pub); got != "" {
		t.Errorf("没有评论时间 = %q，期望空", got)
	}
}

// [支持] 里的两个字不是用户在说话，是他在点一个按钮。把它算进正文的话，
// 这条评论会变成「2 字的短评」，跟「顶」「哈哈」混在一起——而它们
// 其实是两类东西：一个是没说话，一个是说了很短的话。
func TestIsEmojiOnlyStripsEmoteNames(t *testing.T) {
	only := []string{
		"",
		"[doge]",
		"[支持][doge]",
		" [大哭] ",
		"！！！",
		"🎉🎉",
		"[笑]！？",
	}
	for _, s := range only {
		if !IsEmojiOnly(s) {
			t.Errorf("%q 应当被判为只有表情", s)
		}
	}

	// 找不到右括号时原样保留：宁可把表情当正文，也不要把正文当表情丢掉。
	withText := []string{
		"[支持] 说得对",
		"前排",
		"哈哈哈[笑]哈哈哈",
		"3",
		"[笑", // 未闭合的方括号，后面的字是正文
	}
	for _, s := range withText {
		if IsEmojiOnly(s) {
			t.Errorf("%q 里有正文，不该被判为只有表情", s)
		}
	}
}

func TestIsLottery(t *testing.T) {
	yes := []string{
		"转发抽奖",
		"参与抽奖送大会员",
		"前排，中奖名单什么时候出",
		"转发+关注，抽一人送抱枕",
		"求关注，顺手转发一下",
	}
	for _, s := range yes {
		if !IsLottery(s) {
			t.Errorf("%q 应当被判为抽奖评论", s)
		}
	}

	// 判据刻意保守：宁可漏掉几条，也不要把正常发言标记掉。
	no := []string{
		"我中了一个亿",
		"已转发",
		"关注了",
		"这个视频做得真好",
	}
	for _, s := range no {
		if IsLottery(s) {
			t.Errorf("%q 是正常评论，不该被判为抽奖", s)
		}
	}
}

func TestMarkSetsRel(t *testing.T) {
	c := New(pub)
	x := cm("这个转场做得真好")
	c.Mark(x)
	if x.Rel != "1小时" {
		t.Errorf("rel = %q，期望 1小时", x.Rel)
	}

	// 没有发布时间就算不出相对时间，字段保持为空而不是写个假的。
	c = New(time.Time{})
	y := cm("这个转场做得真好")
	c.Mark(y)
	if y.Rel != "" {
		t.Errorf("没有发布时间时 rel = %q，期望空", y.Rel)
	}
}

func TestMarkCombinesFlags(t *testing.T) {
	c := New(pub)

	// 短 + 抽奖可以同时成立：一条评论「抽奖」两个字，两个判断都对。
	x := cm("抽奖")
	c.Mark(x)
	if !hasFlag(x, FlagShort) || !hasFlag(x, FlagLottery) {
		t.Errorf("flags = %q，期望同时有 short 与 lottery", flagLine(x))
	}

	// 表情评论提前返回，不会再被算成短评：它压根没有说话，
	// 「≤5 字」是个没有意义的描述。
	y := cm("[doge]")
	c.Mark(y)
	if flagLine(y) != FlagEmojiOnly {
		t.Errorf("flags = %q，期望只有 emoji_only", flagLine(y))
	}
}

func TestMarkDetectsRepeat(t *testing.T) {
	c := New(pub)

	first := cm("这个转场做得真好，配乐也绝了")
	c.Mark(first)
	if hasFlag(first, FlagRepeat) {
		t.Error("第一条评论是复读源的建立者，不该被标记为复读")
	}

	// 完全一样。
	dup := cm("这个转场做得真好，配乐也绝了")
	c.Mark(dup)
	if !hasFlag(dup, FlagRepeat) {
		t.Errorf("逐字相同的评论应当被判为复读，flags = %q", flagLine(dup))
	}

	// 只差空白与大小写：这是同一句话在两次输入下的样子。
	spaced := cm("  这个转场做得真好，配乐也绝了  ")
	c.Mark(spaced)
	if !hasFlag(spaced, FlagRepeat) {
		t.Errorf("只差空白的评论应当被判为复读，flags = %q", flagLine(spaced))
	}

	if c.Repeats() != 2 {
		t.Errorf("复读计数 = %d，期望 2", c.Repeats())
	}

	// 不同的话不能混为一谈。
	other := cm("这个视频的第三段剪辑有点拖沓，建议再看一遍")
	c.Mark(other)
	if hasFlag(other, FlagRepeat) {
		t.Errorf("不同的评论被误判为复读，flags = %q", flagLine(other))
	}
}

// 纯表情评论不进复读索引。它们天然就是同一批表情刷出来的，
// 进索引会把复读榜刷满，真正需要看见的「同一句话被刷了几百遍」反而被淹掉。
func TestEmojiOnlyDoesNotEnterRepeatIndex(t *testing.T) {
	c := New(pub)
	for i := 0; i < 5; i++ {
		x := cm("[doge]")
		c.Mark(x)
		if hasFlag(x, FlagRepeat) {
			t.Fatalf("第 %d 条表情评论被标成了复读：%q", i+1, flagLine(x))
		}
	}
	if c.Repeats() != 0 {
		t.Errorf("复读计数 = %d，期望 0", c.Repeats())
	}
}

// 续传时要把前缀里已经写进文件的评论喂回索引，否则断点之后的评论
// 只会与断点之后的评论比，「前半段标了复读、后半段同样的复读没标」。
func TestObserveSeedsTheIndex(t *testing.T) {
	seeder := New(pub)
	seeder.Observe(cm("这个转场做得真好，配乐也绝了"))

	// 补完索引之后，新的一条要与前缀里的那条比。
	c := New(pub)
	c.Observe(cm("这个转场做得真好，配乐也绝了"))

	dup := cm("这个转场做得真好，配乐也绝了")
	c.Mark(dup)
	if !hasFlag(dup, FlagRepeat) {
		t.Errorf("前缀没被喂进索引，跨断点的复读漏掉了：%q", flagLine(dup))
	}
}

// 续传时 seedCollector 会把前缀里的评论喂给 Mark（结果丢弃），
// 再由正常写入路径重新标记。两条路径必须走同一套判断——
// 若 Observe 和 Mark 分叉，续传出来的标记就会与不中断的那次不一致。
func TestObserveAndMarkAgree(t *testing.T) {
	a := New(pub)
	v1 := cm("哈哈哈哈哈哈")
	a.Mark(v1)

	b := New(pub)
	v2 := cm("哈哈哈哈哈哈")
	b.Observe(v2)

	if flagLine(v1) != flagLine(v2) || v1.Rel != v2.Rel {
		t.Errorf("Observe 得到 %q/%q，Mark 得到 %q/%q",
			flagLine(v1), v1.Rel, flagLine(v2), v2.Rel)
	}
}

// 分带索引的正确性依赖一条鸽笼性质：汉明距离 ≤ 3 时，64 位切成 4 段 16 位，
// 至少有一段完全相同。所以「比中」与「距离 ≤ RepeatDistance」必须完全等价。
//
// 这里直接拿它当断言：索引里只有 base 一条，变体查表必然能找到它，
// 于是「判定为复读」等价于「距离达标」。若哪天索引实现改坏了分带，
// 这个测试会在某个距离上对不上。
func TestRepeatDetectionMatchesHammingDistance(t *testing.T) {
	base := "这个转场的剪辑节奏把握得很好，尤其是第二段配乐进来的那一瞬间"
	variants := []string{
		base,
		"这个转场的剪辑节奏把握得很好，尤其是第二段配乐进来的那一瞬间。",
		"这个转场的剪辑节奏把握得很好，尤其是第二段配乐进来的那一刹那",
		"这个转场剪辑节奏把握得很好，尤其是第二段配乐进来的那一瞬间",
		"这个转场的剪辑节奏把握得挺好，尤其是第二段配乐进来的一瞬间",
		"这个转场的剪辑节奏把握得很好，尤其是第三段配乐进来的那一瞬间",
		"完全不相干的一句话，说的是别的事情，字数也不同",
		"这个转场",
	}
	for _, v := range variants {
		idx := newRepeatIndex()
		if idx.add(base) {
			t.Fatalf("索引为空时不该判为复读：%q", base)
		}
		got := idx.add(v)

		d := hamming(simhash(base), simhash(v))
		want := d <= RepeatDistance
		if got != want {
			t.Errorf("变体 %q：距离 %d，判定 %v，期望 %v", v, d, got, want)
		}
	}
}

// 桶里只留第一个见过的 SimHash，后面每一遍都与它比中——
// 也就是「第一次见」与「之后每一次」的分界必须干净，不能时灵时不灵。
func TestRepeatIsStableAcrossManyDuplicates(t *testing.T) {
	c := New(pub)
	msg := "前排出售瓜子饮料矿泉水"
	for i := 0; i < 50; i++ {
		x := cm(msg)
		c.Mark(x)
		got := hasFlag(x, FlagRepeat)
		if i == 0 && got {
			t.Fatal("第一次出现不该是复读")
		}
		if i > 0 && !got {
			t.Fatalf("第 %d 次出现的同一句话没被判为复读", i+1)
		}
	}
	if c.Repeats() != 49 {
		t.Errorf("复读计数 = %d，期望 49", c.Repeats())
	}
}

func hamming(a, b uint64) int {
	n := 0
	for x := a ^ b; x != 0; x &= x - 1 {
		n++
	}
	return n
}
