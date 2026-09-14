package bilibili

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ultramanDecker/bili-comment/internal/model"
)

func mkComment(rpid uint64, ctime int64) *model.Comment {
	return &model.Comment{
		Rpid:  rpid,
		Ctime: model.Time(time.Unix(ctime, 0)),
		User:  model.User{Mid: 1, Name: "u"},
	}
}

// fakePages 按顺序返回预先准备好的页，模拟服务端翻页。
// 记录每次收到的游标，用来断言请求序列。
type fakePages struct {
	pages   []*CommentPage
	offsets []string
	err     error
	calls   int
}

func (f *fakePages) fetch(_ context.Context, offset string) (*CommentPage, error) {
	f.offsets = append(f.offsets, offset)
	if f.err != nil {
		return nil, f.err
	}
	if f.calls >= len(f.pages) {
		// 模拟服务端一直有数据：这能暴露分页循环缺少终止条件的问题。
		return &CommentPage{Cursor: Cursor{NextOffset: offset}}, nil
	}
	p := f.pages[f.calls]
	f.calls++
	return p, nil
}

func run(t *testing.T, pages []*CommentPage, opt FetchOptions) (*FetchResult, []uint64) {
	t.Helper()
	f := &fakePages{pages: pages}
	var got []uint64
	res, err := fetchPages(context.Background(), opt, "", f.fetch, func(page []*model.Comment, _ Cursor) error {
		for _, cm := range page {
			got = append(got, cm.Rpid)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fetchPages 失败：%v", err)
	}
	return res, got
}

func TestFetchPagesWalksCursor(t *testing.T) {
	pages := []*CommentPage{
		{Comments: []*model.Comment{mkComment(1, 100), mkComment(2, 90)},
			Cursor: Cursor{AllCount: 4, NextOffset: "A"}},
		{Comments: []*model.Comment{mkComment(3, 80), mkComment(4, 70)},
			Cursor: Cursor{AllCount: 4, IsEnd: true}},
	}
	res, got := run(t, pages, FetchOptions{AID: 1, Mode: ModeHot})

	want := []uint64{1, 2, 3, 4}
	if len(got) != len(want) {
		t.Fatalf("抓到 %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("抓到 %v，期望 %v", got, want)
		}
	}
	if res.Stopped != StopEnd {
		t.Errorf("停止原因 = %q，期望 %q", res.Stopped, StopEnd)
	}
	if res.Fetched != 4 || res.Expected != 4 || res.Pages != 2 {
		t.Errorf("结果 = %+v，期望 Fetched=4 Expected=4 Pages=2", res)
	}
	if res.Truncated {
		t.Error("全部抓到时报了 truncated")
	}
}

// 第二页的请求必须带上第一页返回的游标，否则会一直重复拉第一页。
func TestFetchPagesPassesCursor(t *testing.T) {
	f := &fakePages{pages: []*CommentPage{
		{Comments: []*model.Comment{mkComment(1, 100)}, Cursor: Cursor{NextOffset: "CAEiAggC"}},
		{Comments: []*model.Comment{mkComment(2, 90)}, Cursor: Cursor{IsEnd: true}},
	}}
	var got []uint64
	_, err := fetchPages(context.Background(), FetchOptions{AID: 1}, "", f.fetch,
		func(page []*model.Comment, _ Cursor) error {
			for _, cm := range page {
				got = append(got, cm.Rpid)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("失败：%v", err)
	}

	if len(f.offsets) != 2 {
		t.Fatalf("请求了 %d 次，期望 2 次：%v", len(f.offsets), f.offsets)
	}
	if f.offsets[0] != "" {
		t.Errorf("首页游标 = %q，期望空串", f.offsets[0])
	}
	if f.offsets[1] != "CAEiAggC" {
		t.Errorf("第二页游标 = %q，期望 CAEiAggC", f.offsets[1])
	}
}

// limit 正好落在页中间时要精确截断，既不能多也不能少。
func TestFetchPagesLimitMidPage(t *testing.T) {
	pages := []*CommentPage{
		{Comments: []*model.Comment{mkComment(1, 100), mkComment(2, 90), mkComment(3, 80)},
			Cursor: Cursor{AllCount: 100, NextOffset: "A"}},
		{Comments: []*model.Comment{mkComment(4, 70), mkComment(5, 60), mkComment(6, 50)},
			Cursor: Cursor{AllCount: 100, NextOffset: "B"}},
	}
	res, got := run(t, pages, FetchOptions{AID: 1, Limit: 4})

	if len(got) != 4 {
		t.Fatalf("抓到 %d 条，期望 4 条：%v", len(got), got)
	}
	// 必须是前 4 条，而不是「第二页整页都要」或「只要第一页」。
	for i, want := range []uint64{1, 2, 3, 4} {
		if got[i] != want {
			t.Fatalf("抓到 %v，期望前 4 条为 [1 2 3 4]", got)
		}
	}
	if res.Stopped != StopLimit {
		t.Errorf("停止原因 = %q，期望 %q", res.Stopped, StopLimit)
	}
	if !res.Truncated {
		t.Error("只抓了 4/100 条，应当标记 truncated")
	}
}

// limit 是整页边界时，不该为了凑满而多请求一页。
func TestFetchPagesLimitOnPageBoundary(t *testing.T) {
	f := &fakePages{pages: []*CommentPage{
		{Comments: []*model.Comment{mkComment(1, 100), mkComment(2, 90)},
			Cursor: Cursor{AllCount: 100, NextOffset: "A"}},
		{Comments: []*model.Comment{mkComment(3, 80), mkComment(4, 70)},
			Cursor: Cursor{AllCount: 100, NextOffset: "B"}},
	}}
	var got []uint64
	res, err := fetchPages(context.Background(), FetchOptions{AID: 1, Limit: 2}, "", f.fetch,
		func(page []*model.Comment, _ Cursor) error {
			for _, cm := range page {
				got = append(got, cm.Rpid)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("失败：%v", err)
	}
	if len(got) != 2 {
		t.Fatalf("抓到 %v，期望 2 条", got)
	}
	if len(f.offsets) != 1 {
		t.Errorf("请求了 %d 次，期望 1 次（不该多抓一页再丢掉）", len(f.offsets))
	}
	if res.Stopped != StopLimit {
		t.Errorf("停止原因 = %q，期望 %q", res.Stopped, StopLimit)
	}
}

// --since 只在按时间排序时生效：按热度排序的评论不按时间递减，
// 看到一条旧的就停会漏掉后面更新的评论。
func TestFetchPagesSinceOnlyInTimeMode(t *testing.T) {
	cutoff := time.Unix(75, 0)
	pages := func() []*CommentPage {
		return []*CommentPage{
			{Comments: []*model.Comment{mkComment(1, 100), mkComment(2, 80), mkComment(3, 50)},
				Cursor: Cursor{AllCount: 3, NextOffset: "A"}},
			{Comments: []*model.Comment{mkComment(4, 40)},
				Cursor: Cursor{AllCount: 3, NextOffset: "B"}},
		}
	}

	res, got := run(t, pages(), FetchOptions{AID: 1, Mode: ModeTime, Since: cutoff})
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("按时间排序时应截到第 2 条为止，实际 %v", got)
	}
	if res.Stopped != StopSince {
		t.Errorf("停止原因 = %q，期望 %q", res.Stopped, StopSince)
	}

	// 同样的数据在热度模式下必须原样全收。
	_, gotHot := run(t, pages(), FetchOptions{AID: 1, Mode: ModeHot, Since: cutoff})
	if len(gotHot) != 4 {
		t.Errorf("按热度排序时 --since 不应生效，实际抓到 %v", gotHot)
	}
}

// 服务端把下一页游标指回当前游标时必须停下，否则就是无限循环发请求。
func TestFetchPagesStopsOnStuckCursor(t *testing.T) {
	f := &fakePages{pages: []*CommentPage{
		{Comments: []*model.Comment{mkComment(1, 100)}, Cursor: Cursor{AllCount: 10, NextOffset: "A"}},
		{Comments: []*model.Comment{mkComment(2, 90)}, Cursor: Cursor{AllCount: 10, NextOffset: "A"}},
	}}

	res, err := fetchPages(context.Background(), FetchOptions{AID: 1}, "", f.fetch, func([]*model.Comment, Cursor) error { return nil })
	if err != nil {
		t.Fatalf("失败：%v", err)
	}
	if res.Stopped != StopCursorStuck {
		t.Errorf("停止原因 = %q，期望 %q", res.Stopped, StopCursorStuck)
	}
	if len(f.offsets) > 3 {
		t.Errorf("游标卡死时请求了 %d 次，应当很快停下", len(f.offsets))
	}
}

// 请求出错时，已抓到的部分要如实汇报，不能假装什么都没抓到。
func TestFetchPagesReportsPartialOnError(t *testing.T) {
	f := &fakePages{
		pages: []*CommentPage{
			{Comments: []*model.Comment{mkComment(1, 100)}, Cursor: Cursor{AllCount: 10, NextOffset: "A"}},
		},
		err: errors.New("网络断了"),
	}
	var got []uint64
	res, err := fetchPages(context.Background(), FetchOptions{AID: 1}, "", func(ctx context.Context, offset string) (*CommentPage, error) {
		// 第一页正常返回，之后报错。
		if len(got) == 0 {
			return f.pages[0], nil
		}
		return nil, f.err
	}, func(page []*model.Comment, _ Cursor) error {
		for _, cm := range page {
			got = append(got, cm.Rpid)
		}
		return nil
	})

	if err == nil {
		t.Fatal("应当返回错误")
	}
	// Pages 只数成功的那些：失败的那次请求没有贡献任何数据，
	// 计进去会让「抓了多少页」和「抓了多少条」互相矛盾。
	if res.Fetched != 1 || res.Expected != 10 || res.Pages != 1 {
		t.Errorf("部分结果 = %+v，期望 Fetched=1 Expected=10 Pages=1", res)
	}
}

// 回调返回错误时立即停止，不再继续发请求。
func TestFetchPagesStopsOnCallbackError(t *testing.T) {
	f := &fakePages{pages: []*CommentPage{
		{Comments: []*model.Comment{mkComment(1, 100)}, Cursor: Cursor{AllCount: 10, NextOffset: "A"}},
		{Comments: []*model.Comment{mkComment(2, 90)}, Cursor: Cursor{AllCount: 10, NextOffset: "B"}},
	}}
	boom := errors.New("写盘失败")
	_, err := fetchPages(context.Background(), FetchOptions{AID: 1}, "", f.fetch,
		func([]*model.Comment, Cursor) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("应当把回调的错误原样返回，实际 %v", err)
	}
	if len(f.offsets) != 1 {
		t.Errorf("回调报错后仍请求了 %d 次", len(f.offsets))
	}
}

// UP 主自己的评论要被标出来——分析时「UP 怎么回应的」是个常见问题。
func TestFetchPagesMarksUpComments(t *testing.T) {
	up := mkComment(1, 100)
	up.User.Mid = 42
	other := mkComment(2, 90)
	other.User.Mid = 7

	pages := []*CommentPage{
		{Comments: []*model.Comment{up, other}, Cursor: Cursor{AllCount: 2, IsEnd: true}},
	}
	f := &fakePages{pages: pages}
	var seen []*model.Comment
	if _, err := fetchPages(context.Background(), FetchOptions{AID: 1, UpMid: 42}, "", f.fetch,
		func(page []*model.Comment, _ Cursor) error {
			seen = append(seen, page...)
			return nil
		}); err != nil {
		t.Fatalf("失败：%v", err)
	}

	if !seen[0].IsUp {
		t.Error("UP 主自己的评论没有被标记")
	}
	if seen[1].IsUp {
		t.Error("普通用户的评论被误标为 UP 主")
	}
}

func TestParseCommentMode(t *testing.T) {
	ok := map[string]CommentMode{
		"hot": ModeHot, "HOT": ModeHot, "  hot  ": ModeHot, "3": ModeHot, "": ModeHot,
		"time": ModeTime, "Time": ModeTime, "2": ModeTime,
	}
	for in, want := range ok {
		got, err := ParseCommentMode(in)
		if err != nil {
			t.Errorf("ParseCommentMode(%q) 报错：%v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseCommentMode(%q) = %v，期望 %v", in, got, want)
		}
	}
	// 拼错排序名时必须报错。默默退回默认值会让用户以为筛选生效了，
	// 实际上抓的是另一种排序。
	if _, err := ParseCommentMode("newest"); err == nil {
		t.Error("未知排序名应当报错")
	}
}

// member.mid 在响应里是字符串，外层 mid 是数字，两种都要能解析。
func TestFlexIDAcceptsStringAndNumber(t *testing.T) {
	var v struct {
		A flexID `json:"a"`
		B flexID `json:"b"`
	}
	if err := json.Unmarshal([]byte(`{"a":"320773657","b":320773657}`), &v); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if v.A != 320773657 || v.B != 320773657 {
		t.Errorf("A=%d B=%d，期望都是 320773657", v.A, v.B)
	}

	var empty struct {
		A flexID `json:"a"`
	}
	if err := json.Unmarshal([]byte(`{"a":null}`), &empty); err != nil {
		t.Errorf("null 不应报错：%v", err)
	}
}

// 真实响应里出现过的字段组合，逐个核对映射。
func TestReplyToModel(t *testing.T) {
	const raw = `{
		"rpid": 3760801399,
		"root": 0, "parent": 0,
		"count": 2538, "rcount": 2128,
		"like": 751988,
		"ctime": 1606652805,
		"mid": 320773657,
		"content": {
			"message": "坏消息：被骗了\n好消息：真好听",
			"emote": {"[微笑]": {"text": "[微笑]", "url": "https://i0.hdslb.com/bfs/emote/x.png"}},
			"pictures": [{"img_src": "https://i0.hdslb.com/bfs/new_dyn/y.jpg"}]
		},
		"member": {
			"mid": "320773657", "uname": "老咸鱼A",
			"avatar": "https://i1.hdslb.com/bfs/face/z.jpg",
			"level_info": {"current_level": 6},
			"vip": {"vipStatus": 0, "vipType": 1}
		},
		"reply_control": {"location": "IP属地：上海", "is_up_top": true}
	}`

	var r apiReply
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	cm := r.toModel()

	if cm.Rpid != 3760801399 {
		t.Errorf("Rpid = %d", cm.Rpid)
	}
	if cm.Message != "坏消息：被骗了\n好消息：真好听" {
		t.Errorf("Message = %q，换行必须原样保留", cm.Message)
	}
	if cm.Like != 751988 {
		t.Errorf("Like = %d", cm.Like)
	}
	if cm.Ctime.Time().Unix() != 1606652805 {
		t.Errorf("Ctime = %d", cm.Ctime.Time().Unix())
	}
	// count 与 rcount 都给时取较大的，避免把「漏了多少条」报小了。
	if cm.ReplyCount != 2538 {
		t.Errorf("ReplyCount = %d，期望 2538（取 count 与 rcount 中较大的）", cm.ReplyCount)
	}
	if cm.User.Mid != 320773657 || cm.User.Name != "老咸鱼A" {
		t.Errorf("User = %+v", cm.User)
	}
	if cm.User.Level != 6 {
		t.Errorf("User.Level = %d", cm.User.Level)
	}
	// vipType 为 1 但 vipStatus 为 0 是会员已过期，网页端不显示大会员标记，
	// 所以这里必须以 vipStatus 为准。
	if cm.User.IsVip {
		t.Error("vipStatus 为 0 时不应标记为大会员")
	}
	if cm.Location != "IP属地：上海" {
		t.Errorf("Location = %q", cm.Location)
	}
	if !cm.IsTop {
		t.Error("is_up_top 为真时应当标记置顶")
	}
	if cm.Emotes["[微笑]"] != "https://i0.hdslb.com/bfs/emote/x.png" {
		t.Errorf("Emotes = %v", cm.Emotes)
	}
	if len(cm.Pictures) != 1 || cm.Pictures[0] != "https://i0.hdslb.com/bfs/new_dyn/y.jpg" {
		t.Errorf("Pictures = %v", cm.Pictures)
	}
}

// 楼中楼与一级评论共用结构，靠 root 区分。root 非零的必须原样带出来，
// M4 要靠它把回复挂回所属的楼。
func TestReplyToModelKeepsRootForNestedReply(t *testing.T) {
	var r apiReply
	if err := json.Unmarshal([]byte(`{"rpid":9,"root":123,"parent":456,"ctime":1}`), &r); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	cm := r.toModel()
	if cm.Root != 123 || cm.Parent != 456 {
		t.Errorf("Root=%d Parent=%d，期望 123/456", cm.Root, cm.Parent)
	}
}

// root 为 0 的一级评论不应在输出里带出 root/parent 字段——
// JSONL 会被整个塞进 LLM 上下文，每行省几个字符乘以一万条很可观。
func TestCommentOmitsZeroRootInJSON(t *testing.T) {
	b, err := json.Marshal(mkComment(1, 100))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"root", "parent", "location", "emotes", "pictures", "is_top"} {
		if _, ok := m[k]; ok {
			t.Errorf("空值字段 %q 不应出现在输出里：%s", k, b)
		}
	}
	if _, ok := m["rpid"]; !ok {
		t.Errorf("rpid 必须出现：%s", b)
	}
}

// B 站的正文用两种写法表示换行：老评论里是真正的 \n，新评论里是字面量
// `<br />`。不统一的话同一个字段的含义会随评论的年龄而变——老评论里换行
// 是换行，新评论里换行是六个字符的标签，而下游（尤其是 LLM）会把它当成
// 正文的一部分读出来。
func TestCleanMessageNormalizesBrTags(t *testing.T) {
	cases := map[string]string{
		"111111<br />22222": "111111\n22222",
		"a<br>b":            "a\nb",
		"a<br/>b":           "a\nb",
		"a<br  />b":         "a\nb",
		"a<BR />b":          "a\nb",
		"a<br\n>b":          "a<br\n>b", // 标签里不能有换行，不是标签
		"第一行\n第二行":          "第一行\n第二行", // 已经是真换行的原样保留
		"笑死 >_<":            "笑死 >_<",
		"x < y 且 y > z":     "x < y 且 y > z",
		"<b>粗体</b>":         "<b>粗体</b>", // 只认换行标记，不做通用清洗
		"a<brb":             "a<brb",
		"结尾的 <br":           "结尾的 <br",
		"":                  "",
		"<br />":            "\n",
		"<br /><br />":      "\n\n",
		"上<br />中<br />下":   "上\n中\n下",
		"[doge]<br />[大哭]":  "[doge]\n[大哭]",
	}
	for in, want := range cases {
		if got := cleanMessage(in); got != want {
			t.Errorf("cleanMessage(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// 换行统一之后，同一条评论在模型里只有一种写法，下游不必分别处理。
func TestToModelCleansMessage(t *testing.T) {
	var r apiReply
	const raw = `{"rpid":1,"content":{"message":"第一首:abc<br />第二首:def<br />第三首:ghi"}}`
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	got := r.toModel().Message
	want := "第一首:abc\n第二首:def\n第三首:ghi"
	if got != want {
		t.Errorf("Message = %q，期望 %q", got, want)
	}
}
