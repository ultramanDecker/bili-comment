package bilibili

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ultramanDecker/bili-comment/internal/model"
)

const apiReplyMain = "https://api.bilibili.com/x/v2/reply/wbi/main"

// CommentMode 是评论区的排序方式。两种排序各自最多只能翻到约 5000 条，
// 所以想拿全量必须两种都跑一遍再合并去重（M6 的 --complete）。
type CommentMode int

const (
	ModeTime CommentMode = 2 // 按时间
	ModeHot  CommentMode = 3 // 按热度
)

func (m CommentMode) String() string {
	if m == ModeTime {
		return "time"
	}
	return "hot"
}

// ParseCommentMode 解析命令行传入的排序名。
func ParseCommentMode(s string) (CommentMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "time", "2":
		return ModeTime, nil
	case "hot", "3", "":
		return ModeHot, nil
	}
	return ModeHot, fmt.Errorf("未知的排序方式 %q，可选 hot 或 time", s)
}

// StopReason 说明分页为什么结束。它会写进输出的完整性元数据里：
// LLM 拿到一份残缺数据却不知道残缺，比拿不到数据更危险。
type StopReason string

const (
	StopEnd         StopReason = "end"          // 服务端说没有更多了
	StopLimit       StopReason = "limit"        // 达到 --limit
	StopSince       StopReason = "since"        // 早于 --since 截止时间
	StopCursorStuck StopReason = "cursor_stuck" // 游标不再前进，继续翻会死循环
	StopOffsetLimit StopReason = "offset_limit" // 翻页超出服务端的 offset 上限（实测 5000）
)

// CommentPage 是一页一级评论。
type CommentPage struct {
	Comments []*model.Comment
	Cursor   Cursor
}

// Cursor 是翻页状态。
type Cursor struct {
	IsEnd      bool
	AllCount   int    // 服务端报告的评论总数（一级 + 楼中楼）
	NextOffset string // 下一页的游标，IsEnd 为真时无意义
	Page       int    // 已翻页数，从 1 开始
}

// FetchOptions 控制一次评论抓取。
type FetchOptions struct {
	AID   int64
	Mode  CommentMode
	Limit int       // 最多抓多少条一级评论，<=0 表示不限
	Since time.Time // 只抓这个时间之后的，零值表示不限。仅在 ModeTime 下有效
	UpMid int64     // UP 主的 mid，用于标记 UP 自己的评论
}

// FetchResult 汇总一次抓取的实际结果。
type FetchResult struct {
	Fetched   int        // 实际抓到的一级评论条数
	Expected  int        // 服务端报告的评论总数
	Pages     int        // 成功抓取的页数；失败的请求不计入，与 Fetched 的口径一致
	Stopped   StopReason // 为什么停下
	Truncated bool       // Fetched 明显少于 Expected 时为真
}

// pageFetcher 取一页评论。抽成函数类型是为了让分页逻辑能脱离 HTTP 测试——
// 翻页的边界条件（limit 截断、since 截止、游标不前进）是最容易出错的地方。
type pageFetcher func(ctx context.Context, offset string) (*CommentPage, error)

// FetchComments 分页抓取一级评论，每页交给 onPage 处理。
//
// 之所以用回调而不是先把全部评论收进内存再返回：热门视频动辄几十万条评论，
// 而且抓取要跑几十分钟，中途断掉是常态。逐页交给调用方，调用方才能立刻落盘，
// 断掉时已抓到的部分依然可用。
func (c *Client) FetchComments(ctx context.Context, opt FetchOptions, onPage func([]*model.Comment) error) (*FetchResult, error) {
	return c.FetchCommentsFrom(ctx, opt, "", func(page []*model.Comment, _ Cursor) error {
		return onPage(page)
	})
}

// FetchCommentsFrom 与 FetchComments 相同，但从指定游标开始，并把每页之后的
// 游标一并交给回调。
//
// 续传需要它：一级评论阶段中断后，进度文件里存着中断时的游标，
// 下次接着翻即可，不必把已经抓到的几千条重抓一遍。
func (c *Client) FetchCommentsFrom(ctx context.Context, opt FetchOptions, startOffset string, onPage func([]*model.Comment, Cursor) error) (*FetchResult, error) {
	if opt.AID <= 0 {
		return nil, fmt.Errorf("无效的 aid：%d", opt.AID)
	}
	mode := opt.Mode
	if mode != ModeTime {
		mode = ModeHot
	}
	return fetchPages(ctx, opt, startOffset, func(ctx context.Context, offset string) (*CommentPage, error) {
		return c.commentPage(ctx, opt.AID, mode, offset)
	}, onPage)
}

func fetchPages(ctx context.Context, opt FetchOptions, startOffset string, fetch pageFetcher, onPage func([]*model.Comment, Cursor) error) (*FetchResult, error) {
	res := &FetchResult{Stopped: StopEnd}
	offset := startOffset

	for page := 1; ; page++ {
		// limit 在请求之前判断，避免为了凑数多抓一整页。
		if opt.Limit > 0 && res.Fetched >= opt.Limit {
			res.Stopped = StopLimit
			break
		}

		cp, err := fetch(ctx, offset)
		if err != nil {
			return res, err
		}
		res.Pages = page
		if cp.Cursor.AllCount > 0 {
			res.Expected = cp.Cursor.AllCount
		}

		comments := cp.Comments
		if opt.UpMid != 0 {
			for _, cm := range comments {
				if cm.User.Mid == opt.UpMid {
					cm.IsUp = true
				}
			}
		}

		// 按时间排序时服务端是严格从新到旧的，遇到第一条早于截止时间的
		// 就可以停——再往后只会更旧。按热度排序时没有这个单调性，
		// 所以只在 ModeTime 下启用（CLI 层也会拦住这个组合）。
		if !opt.Since.IsZero() && opt.Mode == ModeTime {
			for i, cm := range comments {
				if cm.Ctime.Time().Before(opt.Since) {
					comments = comments[:i]
					res.Stopped = StopSince
					break
				}
			}
		}

		if opt.Limit > 0 && res.Fetched+len(comments) > opt.Limit {
			comments = comments[:opt.Limit-res.Fetched]
			res.Stopped = StopLimit
		}

		if len(comments) > 0 {
			if err := onPage(comments, cp.Cursor); err != nil {
				return res, err
			}
			res.Fetched += len(comments)
		}

		if res.Stopped == StopSince || res.Stopped == StopLimit {
			break
		}
		if cp.Cursor.IsEnd || cp.Cursor.NextOffset == "" {
			res.Stopped = StopEnd
			break
		}
		// 服务端把「下一页的游标」又指回了我们刚用过的那一个，说明它在重复给同一页。
		// 继续翻就是无限循环，会一直发请求直到撞上风控——宁可停在这里并如实报告。
		// 首页的游标是空串，而上面已经排除了 NextOffset 为空的情况，所以不会误判。
		if cp.Cursor.NextOffset == offset {
			res.Stopped = StopCursorStuck
			break
		}
		offset = cp.Cursor.NextOffset
	}

	res.Truncated = res.Expected > 0 && res.Fetched < res.Expected
	return res, nil
}

// commentPage 拉取一页一级评论。
func (c *Client) commentPage(ctx context.Context, aid int64, mode CommentMode, offset string) (*CommentPage, error) {
	// pagination_str 是一个嵌在查询参数里的 JSON 串。WBI 签名会对整个
	// 编码后的查询串取摘要，所以这里只要保证编码一致即可。
	pagination, err := json.Marshal(struct {
		Offset string `json:"offset"`
	}{Offset: offset})
	if err != nil {
		return nil, err
	}

	params := url.Values{}
	params.Set("oid", strconv.FormatInt(aid, 10))
	params.Set("type", "1") // 1 = 视频
	params.Set("mode", strconv.Itoa(int(mode)))
	params.Set("pagination_str", string(pagination))
	params.Set("plat", "1")
	params.Set("seek_rpid", "")
	params.Set("web_location", "1315875")

	data, err := c.call(ctx, apiReplyMain, params, true)
	if err != nil {
		return nil, err
	}

	var raw apiReplyMainResp
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("解析评论列表失败：%w", err)
	}

	page := &CommentPage{
		Cursor: Cursor{
			IsEnd:      raw.Cursor.IsEnd,
			AllCount:   raw.Cursor.AllCount,
			NextOffset: raw.Cursor.PaginationReply.NextOffset,
		},
	}
	for i := range raw.Replies {
		page.Comments = append(page.Comments, raw.Replies[i].toModel())
	}
	// 置顶评论有时只出现在 top_replies 里，不在 replies 中。
	// 按 rpid 去重，避免它在输出里出现两次。
	seen := make(map[uint64]bool, len(page.Comments))
	for _, cm := range page.Comments {
		seen[cm.Rpid] = true
	}
	for i := range raw.TopReplies {
		if cm := raw.TopReplies[i].toModel(); !seen[cm.Rpid] {
			cm.IsTop = true
			seen[cm.Rpid] = true
			page.Comments = append([]*model.Comment{cm}, page.Comments...)
		}
	}
	return page, nil
}

// apiReplyMainResp 是 /x/v2/reply/wbi/main 的响应结构。
type apiReplyMainResp struct {
	Cursor struct {
		IsEnd           bool `json:"is_end"`
		AllCount        int  `json:"all_count"`
		PaginationReply struct {
			NextOffset string `json:"next_offset"`
		} `json:"pagination_reply"`
	} `json:"cursor"`
	Replies    []apiReply `json:"replies"`
	TopReplies []apiReply `json:"top_replies"`
}

type apiReply struct {
	Rpid   uint64 `json:"rpid"`
	Root   uint64 `json:"root"`
	Parent uint64 `json:"parent"`
	Count  int    `json:"count"`
	Rcount int    `json:"rcount"`
	Like   int    `json:"like"`
	Ctime  int64  `json:"ctime"`
	Mid    flexID `json:"mid"`

	Content struct {
		Message string `json:"message"`
		Emote   map[string]struct {
			Text string `json:"text"`
			URL  string `json:"url"`
		} `json:"emote"`
		Pictures []struct {
			ImgSrc string `json:"img_src"`
		} `json:"pictures"`
	} `json:"content"`

	Member struct {
		// member.mid 在响应里是字符串，而外层 mid 是数字。
		// 同一个接口两种写法，写死任一种都会在另一种出现时解析失败。
		Mid       flexID `json:"mid"`
		Uname     string `json:"uname"`
		Avatar    string `json:"avatar"`
		LevelInfo struct {
			CurrentLevel int `json:"current_level"`
		} `json:"level_info"`
		Vip struct {
			VipStatus int `json:"vipStatus"`
		} `json:"vip"`
	} `json:"member"`

	ReplyControl struct {
		Location string `json:"location"`
		IsUpTop  bool   `json:"is_up_top"`
	} `json:"reply_control"`
}

func (r *apiReply) toModel() *model.Comment {
	// 回复数在 count 与 rcount 两个字段里都给，实测两者相等。
	// 取较大的那个，免得其中一个缺失时把「有多少条没抓」报小了。
	replyCount := r.Count
	if r.Rcount > replyCount {
		replyCount = r.Rcount
	}

	cm := &model.Comment{
		Rpid:       r.Rpid,
		Root:       r.Root,
		Parent:     r.Parent,
		Message:    cleanMessage(r.Content.Message),
		Like:       r.Like,
		Ctime:      model.Time(time.Unix(r.Ctime, 0)),
		ReplyCount: replyCount,
		Location:   r.ReplyControl.Location,
		IsTop:      r.ReplyControl.IsUpTop,
		User: model.User{
			Mid:    int64(r.Member.Mid),
			Name:   r.Member.Uname,
			Avatar: r.Member.Avatar,
			Level:  r.Member.LevelInfo.CurrentLevel,
			IsVip:  r.Member.Vip.VipStatus > 0,
		},
	}

	if len(r.Content.Emote) > 0 {
		cm.Emotes = make(map[string]string, len(r.Content.Emote))
		for name, e := range r.Content.Emote {
			// 表情名在正文里就是 [微笑] 这样的字面量，
			// 这个映射让下游能把名字还原成图片。
			key := e.Text
			if key == "" {
				key = name
			}
			cm.Emotes[key] = e.URL
		}
	}
	for _, p := range r.Content.Pictures {
		if p.ImgSrc != "" {
			cm.Pictures = append(cm.Pictures, p.ImgSrc)
		}
	}
	return cm
}

// cleanMessage 把正文里的排版标记还原成真正的换行。
//
// B 站的正文用两种写法表示换行：老评论里是真正的 \n，新评论里是字面量
// `<br />`。不统一的话，同一个字段的含义会随评论的年龄而变——老评论里
// 换行是换行，新评论里换行是六个字符的标签，而下游（尤其是 LLM）会把
// 它当成正文的一部分读出来。实测一个只有十条评论的样本里就有三条带这个
// 标签，比例不低。
//
// 只处理换行标记，不做通用的 HTML 清洗：正文里的 `<` `>` 大多是「大于号」
// 「箭头」这类正常内容（「笑死 >_<」），把它们当标签删掉是改数据。
// 换行标记是唯一一个「平台用它表达排版、而非用户写下的内容」的构造。
func cleanMessage(s string) string {
	// 绝大多数评论里没有尖括号，先挡掉，免得每条都分配一个 Builder。
	if strings.IndexByte(s, '<') < 0 {
		return s
	}

	// 逐段扫描而不是一次性 ReplaceAll：标签的写法有 <br>、<br/>、<br />
	// 三种，大小写也不保证一致，而 ReplaceAll 要求先枚举出全部写法。
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '<' {
			if n := brTagLen(s[i:]); n > 0 {
				b.WriteByte('\n')
				i += n
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// brTagLen 判断 s 是否以 <br> 形式的标签开头，是则返回它的字节长度。
//
// 只认这一种标记，不做通用 HTML 解析：这里要的是「认识 B 站用来表达换行的
// 那一个构造」，而不是一个解析器。写成通用解析就会开始吞掉用户真正写下的
// 尖括号——「笑死 >_<」和「x < y」都是正常的正文。
func brTagLen(s string) int {
	if len(s) < 4 || !strings.EqualFold(s[:3], "<br") {
		return 0
	}
	i := 3
	skipSpace := func() {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
	}
	skipSpace()
	if i < len(s) && s[i] == '/' {
		i++
		skipSpace()
	}
	if i < len(s) && s[i] == '>' {
		return i + 1
	}
	return 0
}

// flexID 兼容同一个接口里 ID 时而字符串时而数字的写法。
type flexID int64

func (f *flexID) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("无法解析 ID %q：%w", s, err)
	}
	*f = flexID(n)
	return nil
}
