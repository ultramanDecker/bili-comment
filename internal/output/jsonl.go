package output

import (
	"bufio"
	"encoding/json"
	"io"
)

// JSONL 写出「每行一条 JSON」。
//
// 这是程序消费与存档的格式，也是唯一能作为续传锚点的格式：每行独立，
// 追加语义天然成立，截断到任意一条记录的边界都是一份合法文件。
//
// 它同时是所有派生格式的来源——TSV / Markdown / JSON 都是从完整的
// JSONL 重新编码出来的，所以 JSONL 必须是信息最全的那一份，不能有
// 「只给 TSV 看的字段」。
type JSONL struct {
	bw  *bufio.Writer
	enc *json.Encoder
}

func (f *JSONL) Name() string { return "jsonl" }
func (f *JSONL) Ext() string  { return "jsonl" }
func (f *JSONL) Flush() error { return f.bw.Flush() }

// Schema 对 JSONL 的意义与其他格式不同：它没有列，但有稳定的键。
// 返回同一张表是为了让 schema.md 能统一描述所有格式，而不是因为
// JSONL 真的按列写。
func (f *JSONL) Schema() []Field { return CommentSchema() }

func (f *JSONL) Begin(w io.Writer, m *Meta) error {
	f.bw = bufio.NewWriter(w)
	f.enc = json.NewEncoder(f.bw)
	// 关掉 HTML 转义。Go 默认把尖括号和 & 写成 < 这样的六字节转义，
	// 而它防的是「把 JSON 内联进 <script> 标签」的注入——我们没有这个场景。
	// 评论区里这些字符不少见（「笑死 >_<」「A&B」），多出来的字节在几万条
	// 评论上是实打实的体积，人用 less 翻看时也难读。JSON 解析结果不受影响。
	f.enc.SetEscapeHTML(false)
	if m.Resuming {
		return nil
	}
	return f.enc.Encode(m.Video)
}

func (f *JSONL) Write(r Row) error {
	switch r.Kind {
	case KindVideo:
		return f.enc.Encode(r.Video)
	case KindComment:
		return f.enc.Encode(r.Comment)
	case KindStub:
		return f.enc.Encode(r.Stub)
	default:
		return nil
	}
}

func (f *JSONL) End(s *Summary) error {
	if s == nil {
		return nil
	}
	return f.enc.Encode(s)
}
