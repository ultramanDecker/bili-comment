package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func newVideoCmd(g *globals) *cobra.Command {
	var out string

	cmd := &cobra.Command{
		Use:   "video <url|bvid|av号>",
		Short: "获取视频元数据",
		Long: `获取视频的标题、UP 主、发布时间、互动计数与分P 列表。

输入可以是 BV 号、av 号、完整 URL 或 b23.tv 短链，直接粘贴浏览器地址栏即可。`,
		Example: `  bili video BV1xx411c7mD
  bili video https://www.bilibili.com/video/BV1xx411c7mD
  bili video av123456 -o meta.json`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			c, err := g.client()
			if err != nil {
				return err
			}

			ref, err := c.ResolveRef(ctx, args[0])
			if err != nil {
				return err
			}
			if !c.LoggedIn() {
				g.logf("提示：未登录。部分视频的评论需要登录才能查看，可执行 bili login")
			}

			video, err := c.Video(ctx, ref)
			if err != nil {
				return err
			}

			b, err := json.MarshalIndent(video, "", "  ")
			if err != nil {
				return err
			}
			b = append(b, '\n')

			if out != "" {
				if err := writeFile(out, b); err != nil {
					return err
				}
				g.logf("已写入 %s（%s，共 %d P）", out, video.Title, len(video.Pages))
				return nil
			}

			_, err = cmd.OutOrStdout().Write(b)
			return err
		},
	}

	cmd.Flags().StringVarP(&out, "out", "o", "", "写入文件而非打印到 stdout")

	return cmd
}

func writeFile(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建目录失败：%w", err)
		}
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("写入 %s 失败：%w", path, err)
	}
	return nil
}
