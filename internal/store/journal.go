// Package store 负责抓取进度的持久化，让中断的抓取可以续传。
//
// 这里解决的问题只有一句话：**中断后重跑，不重复、不遗漏。**
//
// 难点在于输出是流式追加写的。一楼楼中楼可能写了 3 页就断在第 4 页，
// 此时文件里那半栋楼既不能算「已完成」（会漏），也不能直接重抓（会重）。
// 解法是记录「已确认完整的字节数」：每次一楼彻底写完并落盘后才推进这个位置，
// 续传时先把文件截断回该位置，那半栋楼就被干净地丢掉了，重抓不会产生重复。
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// journalVersion 用于识别旧格式。格式变更时递增，读到不认识的版本就拒绝续传，
// 而不是用错误的语义去解释旧数据。
const journalVersion = 1

// Phase 表示抓取进行到哪一阶段。两个阶段各有各的续传信息。
type Phase string

const (
	PhaseRoots   Phase = "roots"   // 正在抓一级评论
	PhaseReplies Phase = "replies" // 一级评论已抓完，正在抓楼中楼
)

// Journal 记录一次抓取的进度。
//
// 它必须与输出文件严格对应：Offset 之前的内容是完整的，之后就不可信。
// 因此 Save 依赖调用方先 Flush 输出文件——顺序反了就会记录一个
// 比实际落盘内容更靠后的位置，续传时截断出空洞。
type Journal struct {
	Version int    `json:"version"`
	Output  string `json:"output"` // 输出文件路径，防止把 A 的进度用到 B 上
	AID     int64  `json:"aid"`
	BVID    string `json:"bvid,omitempty"`
	Phase   Phase  `json:"phase"`

	// Offset 是输出文件中已确认完整的字节数。续传时先截断到这里。
	Offset int64 `json:"offset"`

	// RootCursor 仅 PhaseRoots 有意义：一级评论下一页的游标。
	// 有了它，一级评论阶段中断后不必从头重抓。
	RootCursor string `json:"root_cursor,omitempty"`

	// PlanIndex 仅 PhaseReplies 有意义：计划中已彻底处理完的楼数（前缀长度）。
	// 存前缀长度而不是 rpid 集合，是因为输出严格按计划顺序写，进度天然是前缀。
	PlanIndex int `json:"plan_index,omitempty"`

	// PolicyHash 是楼中楼策略的指纹。续传时若策略变了，计划就变了，
	// PlanIndex 会指向另一栋楼——必须拒绝续传而不是接着写。
	PolicyHash string `json:"policy_hash,omitempty"`

	// 下面两组累计量描述的是**输出文件里已有的内容**，而不是本次运行。
	//
	// 为什么要存：summary 行是给下游判断「这份数据全不全」用的。续传时如果
	// 只是从头开始数本次新抓的部分，写出来的 summary 会声称只展开了 1 栋楼、
	// 而文件里明明有 3 栋——比不写更有害，因为它看起来像个正常的完整结论。
	// 最糟的一次是续传后 summary 写成 `expected:0, fetched:0`，而文件里有
	// 20 条一级评论：这不是少报，是**和文件内容直接矛盾**。
	//
	// 累计量必须与 Offset 同一次 Save 落盘，两者才对得上同一份文件。

	// Reply* 只用于 PhaseReplies。
	ReplyFetched       int  `json:"reply_fetched,omitempty"`
	ReplyExpected      int  `json:"reply_expected,omitempty"`
	ReplyOffsetLimited int  `json:"reply_offset_limited,omitempty"`
	ReplyTruncated     bool `json:"reply_truncated,omitempty"`

	// Root* 描述一级评论阶段的结果。一级评论抓完的那一刻定稿，
	// 此后即便进入 PhaseReplies 也要留着——续传时 summary 写的就是它们。
	RootExpected  int    `json:"root_expected,omitempty"`
	RootFetched   int    `json:"root_fetched,omitempty"`
	RootPages     int    `json:"root_pages,omitempty"`
	RootStopped   string `json:"root_stopped,omitempty"`
	RootTruncated bool   `json:"root_truncated,omitempty"`

	// StubsWritten 表示墓碑行已经写进输出并被算进 Offset。
	//
	// 不能靠 PlanIndex 推断：墓碑行是 phaseReplies 的第一件事，此时 PlanIndex
	// 还是 0，而「在第一楼抓到一半时中断」恰恰是最常见的断点——那种情况下
	// 墓碑行已在文件里，但 PlanIndex 仍是 0，按 PlanIndex 判断就会把它们
	// 再写一遍。这个字段由写墓碑行的同一次 Save 置位，与 Offset 同源。
	StubsWritten bool `json:"stubs_written,omitempty"`

	UpdatedAt string `json:"updated_at"`
}

// ErrNoJournal 表示没有找到进度文件，调用方应开始一次全新的抓取。
var ErrNoJournal = errors.New("没有找到续传进度")

// JournalPath 返回与输出文件配套的进度文件路径。
func JournalPath(output string) string { return output + ".resume.json" }

// LoadJournal 读取进度文件。文件不存在时返回 ErrNoJournal。
func LoadJournal(output string) (*Journal, error) {
	path := JournalPath(output)
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNoJournal
		}
		return nil, fmt.Errorf("读取进度文件失败：%w", err)
	}
	var j Journal
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("进度文件已损坏（%s）：%w", path, err)
	}
	if j.Version != journalVersion {
		return nil, fmt.Errorf("进度文件版本为 %d，当前程序只认 %d，无法续传", j.Version, journalVersion)
	}
	// 输出路径不同说明进度文件被挪用了。接着写会把两批数据混在一起。
	if j.Output != "" && j.Output != output {
		return nil, fmt.Errorf("进度文件属于 %s，不是 %s", j.Output, output)
	}
	return &j, nil
}

// Save 原子地写入进度。
//
// 先写临时文件再 rename：进度文件被写坏比没有进度文件严重得多——
// 前者会让续传截断到错误的位置，后者只是重抓一遍。
func (j *Journal) Save(output string) error {
	j.Version = journalVersion
	j.Output = output
	j.UpdatedAt = time.Now().Format(time.RFC3339)

	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	path := JournalPath(output)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("写入进度文件失败：%w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("替换进度文件失败：%w", err)
	}
	return nil
}

// Remove 删除进度文件，抓取正常结束后调用。
func Remove(output string) error {
	err := os.Remove(JournalPath(output))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// TruncateOutput 把输出文件截断到指定长度，并返回一个追加模式的句柄。
// 续传的第一步就是它：丢掉上次中断时写了一半的那栋楼。
func TruncateOutput(output string, offset int64) (*os.File, error) {
	f, err := os.OpenFile(output, os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开输出文件失败：%w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	// 文件比记录的短，说明它被外部改动过（换了文件、手工编辑过）。
	// 此时续传的假设不成立，截断只会把已有数据毁掉。
	if fi.Size() < offset {
		f.Close()
		return nil, fmt.Errorf("输出文件只有 %d 字节，小于进度的 %d 字节，无法安全续传", fi.Size(), offset)
	}
	if err := f.Truncate(offset); err != nil {
		f.Close()
		return nil, fmt.Errorf("截断输出文件失败：%w", err)
	}
	if _, err := f.Seek(offset, 0); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// EnsureDir 确保输出文件所在目录存在。
func EnsureDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}
