package bilibili

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	"github.com/ultramanDecker/bili-comment/internal/model"
)

const apiReplyReply = "https://api.bilibili.com/x/v2/reply/reply"

// ReplyPageSize 是楼中楼每页的条数。
//
// 实测这是服务端的硬上限：传 ps=30/49/100 都会被静默压回 20，
// 返回的 page.size 也变成 20。所以抓一楼需要的请求数是 ceil(回复数/20)，
// 没有任何通过调大页容量来省请求的余地。
const ReplyPageSize = 20

// ReplyOffsetLimit 是楼中楼翻页的 offset 上限，实测为 5000。
//
// 与一级评论那个约 5000 条的排序上限是同一个数量级，但成因不同：
// 这里限制的是 offset 而非条数，超过就返回 -400 max offset exceeded。
// 单楼回复数超过 5000 时无论如何都抓不全，必须如实上报。
const ReplyOffsetLimit = 5000

// MaxReplyPages 是单楼最多能翻的页数，由 offset 上限推出。
const MaxReplyPages = ReplyOffsetLimit / ReplyPageSize

// ReplyPage 是一页楼中楼。
type ReplyPage struct {
	Replies []*model.Comment
	Total   int // 该楼回复总数，来自 page.count
	Page    int // 当前页码，来自 page.num
	Size    int // 服务端实际采用的页大小，来自 page.size
}

// ReplyResult 汇总一楼楼中楼的抓取结果。
type ReplyResult struct {
	Fetched   int        // 实际抓到的回复条数
	Expected  int        // 服务端报告的该楼回复总数
	Pages     int        // 成功抓取的页数
	Stopped   StopReason // 为什么停下
	Truncated bool       // Fetched 少于 Expected 时为真
}

// replyFetcher 取一页楼中楼。与 pageFetcher 同样是为了让分页逻辑脱离 HTTP 测试。
type replyFetcher func(ctx context.Context, pn int) (*ReplyPage, error)

// FetchReplies 抓取某一级评论下的全部楼中楼。
//
// 楼中楼是**平铺**返回的：root 恒等于传入的一级评论 rpid，
// 而 parent 指向被直接回复的那条评论（可能是一级评论，也可能是楼里的另一条回复）。
// 层级关系靠这两个字段还原，返回的列表本身不带嵌套结构。
func (c *Client) FetchReplies(ctx context.Context, aid int64, root uint64, upMid int64, onPage func([]*model.Comment) error) (*ReplyResult, error) {
	if aid <= 0 {
		return nil, fmt.Errorf("无效的 aid：%d", aid)
	}
	if root == 0 {
		return nil, fmt.Errorf("无效的 root rpid：0")
	}
	return fetchReplyPages(ctx, func(ctx context.Context, pn int) (*ReplyPage, error) {
		return c.replyPage(ctx, aid, root, pn)
	}, upMid, onPage)
}

func fetchReplyPages(ctx context.Context, fetch replyFetcher, upMid int64, onPage func([]*model.Comment) error) (*ReplyResult, error) {
	res := &ReplyResult{Stopped: StopEnd}

	for pn := 1; ; pn++ {
		// 这个上界是兜底而非主要终止条件：正常情况下 page.count 或空页会先让循环停下。
		// 但没有它，一个「永远返回同样内容」的异常响应就会变成无限循环发请求。
		if pn > MaxReplyPages {
			res.Stopped = StopOffsetLimit
			break
		}

		page, err := fetch(ctx, pn)
		if err != nil {
			// offset 超限不是故障，而是服务端明确告知「就只给到这里」。
			// 当作正常停止上报，否则用户会以为抓取失败，而实际上是数据本身拿不全。
			if IsRangeLimit(err) {
				res.Stopped = StopOffsetLimit
				break
			}
			return res, err
		}
		res.Pages = pn
		if page.Total > 0 {
			res.Expected = page.Total
		}

		comments := page.Replies
		if upMid != 0 {
			for _, cm := range comments {
				if cm.User.Mid == upMid {
					cm.IsUp = true
				}
			}
		}

		if len(comments) > 0 {
			if err := onPage(comments); err != nil {
				return res, err
			}
			res.Fetched += len(comments)
		}

		// 两种「到底了」的信号。
		//
		// 注意这里**没有**「返回条数少于 page.size 就是最后一页」这条规则。
		// 直觉上它成立，实测却不成立：某楼 2128 条回复，第 7 页只返回 19 条，
		// 第 8 页又回到 20 条。服务端在页内部过滤掉了若干条（大概是已删除或
		// 被折叠的），但页码仍按过滤前的偏移切分，于是中间页会凭空短一条。
		// 靠短页判断的话，那一楼在 139 条处就被判定「到底了」——不到实际的 7%。
		// 代价还特别隐蔽：不报错，只是安静地少抓，而 summary 里的 expected
		// 明明写着 2128，看上去像是提前触发了什么上限。
		//
		// 所以只认两个信号：空页，以及抓够 count 说的条数。count 缺失时
		// 就靠空页兜底——比请求数多一次，但不会漏。
		if len(comments) == 0 {
			res.Stopped = StopEnd
			break
		}
		// 抓够 count 说的条数就停——但只在刚才那页**没装满**时才信它。
		//
		// 因为 count 会少报，实测一楼 page.count=3286 却翻出了 3287 条。
		// 少报时若是「第 164 页刚好装满 20 条、累计正好等于 3286」，照 count
		// 停手就会漏掉后面真实的回复。而装满页恰恰是唯一需要多确认一次的
		// 情形：末页没装满时我们本来就已经超过了 count，没有任何损失。
		//
		// 多问一页的代价只发生在「回复数正好是 20 的整数倍」的楼上，
		// 换来的是不再依赖一个已知会说谎的字段。
		lastPageFull := page.Size > 0 && len(comments) >= page.Size
		if res.Expected > 0 && res.Fetched >= res.Expected && !lastPageFull {
			res.Stopped = StopEnd
			break
		}
	}

	res.Truncated = res.Expected > 0 && res.Fetched < res.Expected
	return res, nil
}

// replyPage 拉取一楼楼中楼的一页。
func (c *Client) replyPage(ctx context.Context, aid int64, root uint64, pn int) (*ReplyPage, error) {
	params := url.Values{}
	params.Set("oid", strconv.FormatInt(aid, 10))
	params.Set("type", "1") // 1 = 视频
	params.Set("root", strconv.FormatUint(root, 10))
	params.Set("pn", strconv.Itoa(pn))
	params.Set("ps", strconv.Itoa(ReplyPageSize))

	data, err := c.call(ctx, apiReplyReply, params, true)
	if err != nil {
		return nil, err
	}

	// 复用一级评论的响应结构：两个接口返回的回复对象字段完全一致。
	var raw struct {
		Page struct {
			Count int `json:"count"`
			Num   int `json:"num"`
			Size  int `json:"size"`
		} `json:"page"`
		Replies []apiReply `json:"replies"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("解析楼中楼失败：%w", err)
	}

	page := &ReplyPage{
		Total: raw.Page.Count,
		Page:  raw.Page.Num,
		Size:  raw.Page.Size,
	}
	for i := range raw.Replies {
		page.Replies = append(page.Replies, raw.Replies[i].toModel())
	}
	return page, nil
}
