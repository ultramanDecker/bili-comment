package bilibili

import (
	"context"
	"errors"
	"testing"

	"github.com/ultramanDecker/bili-comment/internal/model"
)

// fakeReplyPages 按页号返回预置数据，并记录请求过的页号。
type fakeReplyPages struct {
	pages []*ReplyPage
	pns   []int
	err   error
	errAt int // >0 时在第几页返回 err
}

func (f *fakeReplyPages) fetch(_ context.Context, pn int) (*ReplyPage, error) {
	f.pns = append(f.pns, pn)
	if f.errAt > 0 && pn == f.errAt {
		return nil, f.err
	}
	if pn-1 >= len(f.pages) {
		// 越界页返回空列表，模拟服务端「没有更多了」。
		return &ReplyPage{Page: pn, Size: ReplyPageSize}, nil
	}
	return f.pages[pn-1], nil
}

// mkReplyPage 按给定的 rpid 造一页。
//
// Size 一律取 ReplyPageSize：实测 page.size 是**请求的** ps，与返回条数无关。
// 一楼 2128 条回复、第 7 页只回 19 条时，那一页的 size 仍然是 20。
// 把 Size 写成返回条数会造出一个不存在的服务端行为，让「满页」的判断失真。
func mkReplyPage(total int, rpids ...uint64) *ReplyPage {
	return &ReplyPage{Total: total, Size: ReplyPageSize, Replies: mkComments(rpids...)}
}

// mkComments 造一批带连续 rpid 的回复，用来拼页。
func mkComments(rpids ...uint64) []*model.Comment {
	out := make([]*model.Comment, 0, len(rpids))
	for _, r := range rpids {
		out = append(out, mkComment(r, 100))
	}
	return out
}

func collectReplies(t *testing.T, f *fakeReplyPages, upMid int64) (*ReplyResult, []uint64) {
	t.Helper()
	var got []uint64
	res, err := fetchReplyPages(context.Background(), f.fetch, upMid, func(page []*model.Comment) error {
		for _, cm := range page {
			got = append(got, cm.Rpid)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fetchReplyPages 失败：%v", err)
	}
	return res, got
}

func TestFetchRepliesWalksPages(t *testing.T) {
	f := &fakeReplyPages{pages: []*ReplyPage{
		mkReplyPage(4, 1, 2),
		mkReplyPage(4, 3, 4),
	}}
	res, got := collectReplies(t, f, 0)

	if len(got) != 4 {
		t.Fatalf("抓到 %v，期望 4 条", got)
	}
	for i, want := range []uint64{1, 2, 3, 4} {
		if got[i] != want {
			t.Fatalf("抓到 %v，期望顺序 [1 2 3 4]", got)
		}
	}
	if res.Fetched != 4 || res.Expected != 4 || res.Pages != 2 {
		t.Errorf("结果 = %+v，期望 Fetched=4 Expected=4 Pages=2", res)
	}
	if res.Stopped != StopEnd {
		t.Errorf("停止原因 = %q，期望 %q", res.Stopped, StopEnd)
	}
	if res.Truncated {
		t.Error("抓全了却报了 truncated")
	}
	// 页码必须从 1 开始，且连续。
	if len(f.pns) != 2 || f.pns[0] != 1 || f.pns[1] != 2 {
		t.Errorf("请求页号 = %v，期望 [1 2]", f.pns)
	}
}

// 回复数正好是页大小整数倍时，抓满就该停，不该再请求一个空页。
// 少这一条判断，每个整倍数的楼都白打一次请求——乘以上千栋楼很可观。
func TestFetchRepliesStopsExactlyAtTotal(t *testing.T) {
	f := &fakeReplyPages{pages: []*ReplyPage{
		mkReplyPage(3, 1, 2, 3),
	}}
	res, got := collectReplies(t, f, 0)

	if len(got) != 3 {
		t.Fatalf("抓到 %v，期望 3 条", got)
	}
	if len(f.pns) != 1 {
		t.Errorf("请求了 %v，期望只请求 1 页", f.pns)
	}
	if res.Stopped != StopEnd {
		t.Errorf("停止原因 = %q，期望 %q", res.Stopped, StopEnd)
	}
}

// 中间页短一条**不能**当成末页——这是实测踩到的坑，写成回归测试钉住。
//
// 某楼 2128 条回复，第 7 页只回 19 条，第 8 页又是 20 条：服务端在页内部
// 过滤掉了已删除的回复，但页码仍按过滤前的偏移切分。曾经靠「短页即末页」
// 判断，结果那一楼只抓到 139 条就停了，不报错、不留痕，只是安静地少了 93%。
func TestFetchRepliesDoesNotStopOnShortPage(t *testing.T) {
	f := &fakeReplyPages{pages: []*ReplyPage{
		// 第 1 页比 size 少一条，但后面还有内容。
		{Total: 41, Size: ReplyPageSize, Replies: mkComments(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19)},
		{Total: 41, Size: ReplyPageSize, Replies: mkComments(20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34, 35, 36, 37, 38, 39)},
		{Total: 41, Size: ReplyPageSize, Replies: mkComments(40, 41)},
	}}
	res, got := collectReplies(t, f, 0)

	if len(got) != 41 {
		t.Fatalf("抓到 %d 条，期望 41 条（短页不该终止翻页）", len(got))
	}
	if res.Stopped != StopEnd {
		t.Errorf("停止原因 = %q，期望 %q", res.Stopped, StopEnd)
	}
	if res.Truncated {
		t.Error("抓全了却报了 truncated")
	}
}

// count 会少报。实测一楼 page.count=3286，翻完却拿到 3287 条。
// 少报时若恰好「累计正好等于 count 而这一页又是满的」，照 count 停手就会
// 漏掉后面真实的回复。所以满载页要多确认一页。
func TestFetchRepliesDoesNotTrustUnderstatedCountOnFullPage(t *testing.T) {
	f := &fakeReplyPages{pages: []*ReplyPage{
		// count 说只有 20 条，实际上还有第 2 页。
		{Total: 20, Size: ReplyPageSize, Replies: mkComments(seq(1, 20)...)},
		{Total: 20, Size: ReplyPageSize, Replies: mkComments(seq(21, 25)...)},
	}}
	res, got := collectReplies(t, f, 0)

	if len(got) != 25 {
		t.Fatalf("抓到 %d 条，期望 25 条（count 少报了 5 条）", len(got))
	}
	if res.Fetched != 25 {
		t.Errorf("Fetched = %d，期望 25", res.Fetched)
	}
	// 抓到的比 count 说的多，不该报成截断。
	if res.Truncated {
		t.Error("抓到的比 count 多，不该报 truncated")
	}
	if res.Stopped != StopEnd {
		t.Errorf("停止原因 = %q，期望 %q", res.Stopped, StopEnd)
	}
}

// 反过来：末页没装满时我们本来就已经越过了 count，没有理由再多问一页。
// 这是绝大多数楼的形态（回复数不是 20 的整数倍），省下的请求很可观。
func TestFetchRepliesNoExtraProbeWhenLastPageShort(t *testing.T) {
	f := &fakeReplyPages{pages: []*ReplyPage{
		mkReplyPage(3, 1, 2, 3), // count 说 3 条，也确实返回了 3 条，只是没装满 20
	}}
	res, _ := collectReplies(t, f, 0)

	if len(f.pns) != 1 {
		t.Errorf("请求了 %v，末页没装满时不该再问一页", f.pns)
	}
	if res.Stopped != StopEnd {
		t.Errorf("停止原因 = %q，期望 %q", res.Stopped, StopEnd)
	}
}

// seq 生成 [lo, hi] 的连续 rpid。
func seq(lo, hi int) []uint64 {
	out := make([]uint64, 0, hi-lo+1)
	for i := lo; i <= hi; i++ {
		out = append(out, uint64(i))
	}
	return out
}

// count 缺失时没有「抓够了」这个信号，只能一路翻到空页。
// 多花一次请求，但不会漏——这是缺 count 时唯一安全的选择。
func TestFetchRepliesWithoutCountProbesUntilEmpty(t *testing.T) {
	f := &fakeReplyPages{pages: []*ReplyPage{
		{Total: 0, Size: ReplyPageSize, Replies: mkComments(1, 2)},
	}}
	res, got := collectReplies(t, f, 0)

	if len(got) != 2 {
		t.Fatalf("抓到 %v，期望 2 条", got)
	}
	// 第 2 页为空，正是在那里确认了「到底了」。
	if len(f.pns) != 2 || f.pns[1] != 2 {
		t.Errorf("请求页 = %v，期望 [1 2]", f.pns)
	}
	if res.Stopped != StopEnd {
		t.Errorf("停止原因 = %q，期望 %q", res.Stopped, StopEnd)
	}
	// count 缺失时不该断言 truncated——无从判断是否抓全。
	if res.Truncated {
		t.Error("count 缺失时不该报 truncated")
	}
}

// offset 超限是「服务端就只给到这里」，不是故障。
// 当成错误上报会让用户以为抓取失败，而实际上是这一楼的数据本身就取不全。
func TestFetchRepliesOffsetLimitIsNotAnError(t *testing.T) {
	f := &fakeReplyPages{
		pages: []*ReplyPage{mkReplyPage(100000, 1, 2)},
		err:   NewAPIError(-400, "max offset exceeded", "x"),
		errAt: 2,
	}
	res, got := collectReplies(t, f, 0)

	if len(got) != 2 {
		t.Fatalf("已抓到的部分应当保留，实际 %v", got)
	}
	if res.Stopped != StopOffsetLimit {
		t.Errorf("停止原因 = %q，期望 %q", res.Stopped, StopOffsetLimit)
	}
	if !res.Truncated {
		t.Error("只抓到 2/100000 条，应当标记 truncated")
	}
}

// 除 offset 超限外的错误必须原样冒出，不能被当成正常停止吞掉。
func TestFetchRepliesPropagatesRealErrors(t *testing.T) {
	boom := errors.New("网络断了")
	f := &fakeReplyPages{
		pages: []*ReplyPage{mkReplyPage(100, 1)},
		err:   boom,
		errAt: 2,
	}
	var got []*model.Comment
	res, err := fetchReplyPages(context.Background(), f.fetch, 0, func(p []*model.Comment) error {
		got = append(got, p...)
		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("应当返回原始错误，实际 %v", err)
	}
	if len(got) != 1 || res.Fetched != 1 {
		t.Errorf("部分结果应保留，实际 got=%d res=%+v", len(got), res)
	}
}

// 服务端如果一直返回满页且 count 说还有更多，循环必须有个上界，
// 否则就是一个不停发请求的无限循环。
func TestFetchRepliesBoundedByOffsetLimit(t *testing.T) {
	// 永远返回满页，count 永远说还有更多。
	f := &endlessReplies{}
	var n int
	res, err := fetchReplyPages(context.Background(), f.fetch, 0, func(p []*model.Comment) error {
		n += len(p)
		return nil
	})
	if err != nil {
		t.Fatalf("失败：%v", err)
	}
	if res.Stopped != StopOffsetLimit {
		t.Errorf("停止原因 = %q，期望 %q", res.Stopped, StopOffsetLimit)
	}
	if f.calls > MaxReplyPages {
		t.Errorf("请求了 %d 次，超过上界 %d", f.calls, MaxReplyPages)
	}
}

type endlessReplies struct{ calls int }

func (e *endlessReplies) fetch(_ context.Context, pn int) (*ReplyPage, error) {
	e.calls++
	// count 说还有海量回复，永远够不着，所以只有上界能让循环停下。
	page := &ReplyPage{Total: 1 << 30, Page: pn, Size: ReplyPageSize}
	for i := 0; i < ReplyPageSize; i++ {
		page.Replies = append(page.Replies, mkComment(uint64(pn*1000+i), 1))
	}
	return page, nil
}

func TestFetchRepliesMarksUp(t *testing.T) {
	up := mkComment(1, 100)
	up.User.Mid = 42
	other := mkComment(2, 100)
	other.User.Mid = 7

	f := &fakeReplyPages{pages: []*ReplyPage{
		{Total: 2, Size: ReplyPageSize, Replies: []*model.Comment{up, other}},
	}}
	var seen []*model.Comment
	if _, err := fetchReplyPages(context.Background(), f.fetch, 42, func(p []*model.Comment) error {
		seen = append(seen, p...)
		return nil
	}); err != nil {
		t.Fatalf("失败：%v", err)
	}

	if !seen[0].IsUp {
		t.Error("UP 主自己的回复没有被标记")
	}
	if seen[1].IsUp {
		t.Error("普通用户的回复被误标为 UP 主")
	}
}

// 回调报错要立刻停手，不再继续发请求。
func TestFetchRepliesStopsOnCallbackError(t *testing.T) {
	f := &fakeReplyPages{pages: []*ReplyPage{
		mkReplyPage(100, 1, 2),
		mkReplyPage(100, 3, 4),
	}}
	boom := errors.New("写盘失败")
	_, err := fetchReplyPages(context.Background(), f.fetch, 0, func([]*model.Comment) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("应当原样返回回调错误，实际 %v", err)
	}
	if len(f.pns) != 1 {
		t.Errorf("回调报错后仍请求了 %v", f.pns)
	}
}

func TestFetchRepliesRejectsBadArgs(t *testing.T) {
	c := NewClient(Options{Delay: 0})
	if _, err := c.FetchReplies(context.Background(), 0, 1, 0, nil); err == nil {
		t.Error("aid 为 0 应当报错")
	}
	if _, err := c.FetchReplies(context.Background(), 1, 0, 0, nil); err == nil {
		t.Error("root 为 0 应当报错")
	}
}
