package annotate

import (
	"hash/fnv"
	"math/bits"
	"strings"
	"unicode"
)

// repeatIndex 用 SimHash + 分带索引检测复读。
//
// 为什么不能两两比对：10k 条评论是 5000 万次比较，20 万条是 200 亿次，
// 跑不动。而分带能把复杂度降到 O(n)——理由是一个鸽笼原理：
// 两条 SimHash 的汉明距离 ≤ 3 时，把 64 位切成 4 段 16 位，
// 至少有一段是完全相同的（3 位差异最多落在 3 段里，第 4 段必然相同）。
//
// 于是只需要在「某一段相同」的桶内比对。桶内平均只有几条，
// 整体就是线性的。
type repeatIndex struct {
	bands [bandCount]map[uint64]uint64
}

const (
	bandCount  = 4
	bandBits   = 64 / bandCount
	bandMask   = 1<<bandBits - 1
	gramRunes  = 3 // 用字符三元组做特征，比二元组更能区分语序
	maxGrams   = 400
	minGramLen = 3
)

func newRepeatIndex() *repeatIndex {
	ri := &repeatIndex{}
	for i := range ri.bands {
		ri.bands[i] = make(map[uint64]uint64)
	}
	return ri
}

// add 把一条正文放进索引，并返回它是否与之前见过的某条近似相同。
//
// 索引里每个桶只留**第一个**见过的 SimHash。看起来是丢信息，其实不是：
// 复读的特征是同一句话出现很多遍，第一遍进桶之后，后面每一遍都会与它比中，
// 一个桶里留再多副本也不会多抓出什么。
//
// 代价是两种边界情形：桶里留的代表被后来的「更像的」顶替不了（先到先得），
// 以及两条不同的话挤进同一个桶时代表是谁有随机性。两者都只影响哪一条被标成
// 复读，不影响「这里存在复读」这个事实。
func (ri *repeatIndex) add(msg string) bool {
	h := simhash(msg)

	repeat := false
	for i := range ri.bands {
		prev, ok := ri.bands[i][bandOf(h, i)]
		if !ok {
			continue
		}
		if bits.OnesCount64(h^prev) <= RepeatDistance {
			repeat = true
			break
		}
	}

	for i := range ri.bands {
		k := bandOf(h, i)
		if _, ok := ri.bands[i][k]; !ok {
			ri.bands[i][k] = h
		}
	}
	return repeat
}

func bandOf(h uint64, i int) uint64 {
	return (h >> (uint(i) * bandBits)) & bandMask
}

// simhash 算一段文本的 SimHash。
//
// 做法是对每个特征算一个哈希，然后逐位投票：某一位上「1」多就出 1，
// 「0」多就出 0。结果是「多数特征的共同倾向」，所以改动少量特征只会
// 翻动少数几位——这正是它能容忍「改一两个字的变体」的原因。
func simhash(s string) uint64 {
	var votes [64]int
	n := 0
	forEachGram(normalize(s), func(g string) {
		h := fnv1a(g)
		for i := 0; i < 64; i++ {
			if h&(1<<uint(i)) != 0 {
				votes[i]++
			} else {
				votes[i]--
			}
		}
		n++
	})
	if n == 0 {
		return 0
	}
	var out uint64
	for i := 0; i < 64; i++ {
		if votes[i] > 0 {
			out |= 1 << uint(i)
		}
	}
	return out
}

// forEachGram 遍历文本的特征。
//
// 用字符三元组而不是分词：中文分词要词典，而这个词典要么很大要么很偏，
// 并且对「复读」这种判重任务来说，三元组已经足够——「这个转场做得真好」和
// 「这个转场做的真好」共享 6 个三元组中的 5 个，SimHash 距离很小。
func forEachGram(s string, fn func(string)) {
	rs := []rune(s)
	if len(rs) == 0 {
		return
	}
	if len(rs) < minGramLen {
		// 太短，凑不出三元组，整串当一个特征。
		fn(string(rs))
		return
	}
	// 超长文本封顶。复读的判据在开头几十个字里就已经确定了，
	// 而长评论（有人在评论区写小作文）如果全文参与，几十个特征里
	// 差异会被平均掉，反而更难判重。
	if len(rs) > maxGrams {
		rs = rs[:maxGrams]
	}
	for i := 0; i+gramRunes <= len(rs); i++ {
		fn(string(rs[i : i+gramRunes]))
	}
}

// normalize 把对判重无意义的差异抹平。
//
// 只做三件事：小写化、把连续空白压成一个空格、去掉首尾空白。
// **不去标点**——感叹号的数量本身就是情绪信号，把「哈哈哈哈哈」和
// 「哈哈哈哈哈！！！！！」判成同一条是可以接受的，但把标点全丢掉会让
// 很多本来不同的话变得相同。
func normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

func fnv1a(s string) uint64 {
	h := fnv.New64a()
	// hash.Hash 的 Write 不会返回错误，忽略即可。
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}
