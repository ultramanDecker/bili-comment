package output

import (
	"bufio"
	"encoding/json"
	"io"

	"github.com/ultramanDecker/bili-comment/internal/model"
)

// JSON 写出一个完整嵌套的对象，给「不认 JSONL 的工具」用。
//
// 它和其他格式有一个本质区别：**必须在内存里攒齐才能吐**。JSON 的结构要求
// 一个对象包住所有记录，而右花括号的位置取决于记录总数。所以它
// 不能流式写、不能作为续传锚点（没有「写到第几字节」这回事），
// 而且抓 20 万条评论时会把它们全留在内存里。
//
// 这个代价是值得的，因为它的用途就是兼容性：想把数据丢进 jq、
// pandas、某个只吃标准 JSON 的脚本时，JSONL 那些「每行一个对象」
// 的文件在它们眼里根本不是合法 JSON。
type JSON struct {
	bw       *bufio.Writer
	video    *Video
	comments []*model.Comment
	stubs    []*Stub
}

func (f *JSON) Name() string    { return "json" }
func (f *JSON) Ext() string     { return "json" }
func (f *JSON) Schema() []Field { return CommentSchema() }
func (f *JSON) Flush() error    { return f.bw.Flush() }

func (f *JSON) Begin(w io.Writer, m *Meta) error {
	f.bw = bufio.NewWriter(w)
	// 视频行在 Write 里到达（与 JSONL 一致），这里只做兜底：
	// 万一调用方没推视频行，Meta 里还有一份。
	if m.Video != nil {
		f.video = m.Video
	}
	return nil
}

func (f *JSON) Write(r Row) error {
	switch r.Kind {
	case KindVideo:
		if r.Video != nil {
			f.video = r.Video
		}
	case KindComment:
		if r.Comment != nil {
			f.comments = append(f.comments, r.Comment)
		}
	case KindStub:
		if r.Stub != nil {
			f.stubs = append(f.stubs, r.Stub)
		}
	}
	return nil
}

// jsonDoc 是 JSON 格式的顶层结构。
//
// complete 单独拎出来而不是只放在 completeness 里：下游第一件想知道的
// 往往是「这份数据能不能用」，让它成为一个不用解析嵌套就能读到的布尔值。
type jsonDoc struct {
	Video        *Video            `json:"video"`
	Complete     bool              `json:"complete"`
	Completeness *jsonCompleteness `json:"completeness"`
	Stubs        []*Stub           `json:"reply_stubs,omitempty"`
	Comments     []*model.Comment  `json:"comments"`
}

// jsonCompleteness 是按 DESIGN.md §4.3 的形状写的完整度元数据。
type jsonCompleteness struct {
	RootComments *jsonSide `json:"root_comments"`
	SubReplies   *jsonSide `json:"sub_replies,omitempty"`
	Truncated    bool      `json:"truncated"`
	Error        string    `json:"error,omitempty"`
}

type jsonSide struct {
	Expected int `json:"expected"`
	Fetched  int `json:"fetched"`
}

func (f *JSON) End(s *Summary) error {
	doc := jsonDoc{
		Video:    f.video,
		Stubs:    f.stubs,
		Comments: f.comments,
	}
	if doc.Comments == nil {
		// nil 会序列化成 null，而下游期望的是「没有评论」这个空列表。
		doc.Comments = []*model.Comment{}
	}
	if s != nil {
		// 两处必须来自同一个判断。分开算过一次，结果是
		// complete=true 而 completeness.truncated=true：一级评论抓全了、
		// 楼中楼被 --replies-top 裁掉了，于是「这份数据能不能用」这个
		// 下游最先读的字段说了「能用」。它比嵌套字段更早被读到，也更容易
		// 被单独读走，所以自相矛盾时错的是它。
		truncated := s.Truncated
		if r := s.Replies; r != nil && r.Truncated {
			truncated = true
		}
		doc.Completeness = &jsonCompleteness{
			RootComments: &jsonSide{Expected: s.Expected, Fetched: s.Fetched},
			Truncated:    truncated,
			Error:        s.Error,
		}
		if r := s.Replies; r != nil {
			doc.Completeness.SubReplies = &jsonSide{Expected: r.Expected, Fetched: r.Fetched}
		}
		doc.Complete = !truncated && s.Error == ""
	}
	// 缩进输出：这个格式的定位是给人看和给工具解析，而它本来就已经
	// 把所有数据留在内存里了，省这点字节没有意义。
	enc := json.NewEncoder(f.bw)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(doc)
}
