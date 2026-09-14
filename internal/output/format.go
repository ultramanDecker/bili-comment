package output

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
)

// Field 是一个字段的元信息。
//
// DESIGN.md §4.1 说格式矩阵「约束了 Formatter 接口：需要能拿到字段元信息，
// 而不只是写记录」。原因有三个，都是实的：
//
//   - TSV 的表头要从字段名生成，不能手写第二份，否则改字段时必然漏改一处；
//   - schema.md 要自描述，它的内容就是这张表；
//   - JSONL 与 TSV 的字段必须一一对应，元信息是校验这一点的地方。
//
// Type 用粗略的分类（int/str/time/flags）而不是 Go 类型：它的读者是
// 下游的人和 LLM，不是编译器。「这是个时间戳」比「这是个 int64」有用得多。
type Field struct {
	Name string
	Type string
	Desc string
}

// Formatter 是一种输出格式。
//
// 接口按「推」设计而不是「写一个整体」：抓取是流式的，边抓边写才能让中断时
// 的文件依然可用。需要整体结构的格式（JSON）在实现内部缓冲，
// 接口上看不出区别——这是刻意的，调用方不该为某个格式的特殊性买单。
type Formatter interface {
	// Name 是 --format 认识的名字。
	Name() string
	// Ext 是这种格式的文件扩展名（不含点）。
	Ext() string
	// Schema 返回主体记录的字段元信息。没有列概念的格式返回 nil。
	Schema() []Field
	// Begin 在写任何行之前调用一次。
	Begin(w io.Writer, m *Meta) error
	// Write 写一行。
	Write(r Row) error
	// End 在全部行写完之后调用一次，带上收尾统计。
	End(s *Summary) error
	// Flush 把缓冲里的内容推给 Begin 拿到的 writer。
	//
	// 缓冲归各格式自己管，因为「攒多少再吐」是格式自己的事——
	// JSON 要攒到 End，JSONL 逐行吐，TSV 攒 4KB。但调用方必须在推进
	// 续传进度之前把它调掉：字节数只有真正交到 writer 手里才是准的，
	// 留在格式自己的缓冲里就只是内存里的数字。
	Flush() error
}

// 支持的名字。md 与 markdown 都收，因为两种写法都有人用，
// 而为此报错纯属为难用户。
var registry = map[string]func() Formatter{
	"jsonl":    func() Formatter { return &JSONL{} },
	"ndjson":   func() Formatter { return &JSONL{} },
	"json":     func() Formatter { return &JSON{} },
	"tsv":      func() Formatter { return &TSV{} },
	"md":       func() Formatter { return &Markdown{} },
	"markdown": func() Formatter { return &Markdown{} },
}

// canonical 把别名归一，避免同一种格式因为写法不同被当成两种。
func canonical(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "ndjson":
		return "jsonl"
	case "markdown":
		return "md"
	default:
		return strings.ToLower(strings.TrimSpace(name))
	}
}

// New 按名字建一个 Formatter。
func New(name string) (Formatter, error) {
	c := canonical(name)
	factory, ok := registry[c]
	if !ok {
		return nil, fmt.Errorf("不认识的输出格式 %q，可用：%s", name, strings.Join(Names(), "、"))
	}
	return factory(), nil
}

// Names 返回所有格式名，去重且有序。
func Names() []string {
	seen := map[string]bool{}
	var out []string
	for k := range registry {
		c := canonical(k)
		if seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// DefaultDatasetFormats 是目录模式下的默认格式组合。
//
// jsonl 是主数据与续传锚点，md 是给人和 LLM 读的版本。tsv 不默认开——
// 它省 token 但丢结构，属于「分析场景手动切」的那一档，见 DESIGN.md §4.1。
func DefaultDatasetFormats() []string { return []string{"jsonl", "md"} }

// ByExt 按文件扩展名猜格式，猜不出来返回空。
func ByExt(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jsonl", ".ndjson":
		return "jsonl"
	case ".json":
		return "json"
	case ".tsv", ".tab":
		return "tsv"
	case ".md", ".markdown":
		return "md"
	default:
		return ""
	}
}

// Streaming 表示这种格式能不能边抓边写。
//
// 只有 JSON 不能：它的结构要求一个完整的对象包住所有记录，
// 所以必须在内存里攒齐再吐。这个区别影响续传——不能流式写的格式
// 没有「写到第几字节」这回事，做不了续传锚点。
func Streaming(name string) bool { return canonical(name) != "json" }
