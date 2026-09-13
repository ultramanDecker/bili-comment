# bili-comment

把 B 站视频的评论区提取成结构化文件，供程序或 LLM 消费。

单个静态链接的二进制，无运行时依赖。

## 状态

> **开发中。** 一级评论已可用；楼中楼、多格式输出、批量抓取尚未实现。

| 命令 | 状态 |
|---|---|
| `bili video <视频>` | ✅ 视频元数据 |
| `bili login` / `logout` / `whoami` | ✅ 扫码登录与登录态缓存 |
| `bili comments <视频>` | ✅ 一级评论，游标翻页 + 提前停止 |
| 楼中楼 / 多格式输出 / 批量 | 🚧 开发中 |

进度与设计取舍见 [DESIGN.md](DESIGN.md)。

## 安装

```bash
go install github.com/ultramanDecker/bili-comment/cmd/bili@latest
```

或者自行构建：

```bash
git clone https://github.com/ultramanDecker/bili-comment
cd bili-comment
go build -o bili ./cmd/bili
```

需要 Go 1.27 或更高版本。

## 使用

视频标识可以直接粘贴浏览器地址栏里的内容，BV 号、av 号、完整 URL、`b23.tv` 短链都能识别：

```bash
bili video BV1GJ411x7h7
bili video https://www.bilibili.com/video/BV1GJ411x7h7?p=2
bili video av80433022
```

输出是 JSON，默认写到 stdout，可以用 `-o` 存成文件：

```bash
bili video BV1GJ411x7h7 -o meta.json
```

登录（可选，但登录后能看到的评论更多）：

```bash
bili login      # 终端显示二维码，用 B 站手机客户端扫码
bili whoami     # 确认登录态
bili logout     # 清除本地登录态
```

**登录不是可选项，是完整抓取的前提。** 未登录时服务端会静默截断评论列表——实测对一条 23 万评论的视频只返回 3 条并声称「已到底」。命令会在未登录时给出警告。

### 抓取评论

```bash
bili comments BV1GJ411x7h7                      # 按热度，默认全部
bili comments BV1GJ411x7h7 --limit 500          # 只抓 500 条
bili comments BV1GJ411x7h7 --mode time --since 2024-01-01
bili comments BV1GJ411x7h7 -o comments.jsonl    # 写入文件
```

| 参数 | 说明 |
|---|---|
| `-o, --out` | 写入文件而非 stdout |
| `--limit` | 最多抓多少条一级评论，0（默认）表示不限 |
| `--mode` | `hot`（热度，默认）或 `time`（时间） |
| `--since` | 只抓该日期之后的评论，需配合 `--mode time` |
| `--full` | 保留头像与表情图片地址 |

输出是 **JSONL**（每行一条 JSON），由三类行组成，靠 `type` 字段区分：

```jsonl
{"type":"video","bvid":"BV1GJ411x7h7","title":"...","stat_reply":232702,"mode":"hot",...}
{"rpid":3760801399,"user":{"mid":320773657,"name":"老咸鱼A","level":6},"message":"...","like":751988,"ctime":1606652805,"reply_count":2538}
{"type":"summary","expected":232702,"fetched":40,"pages":2,"reason":"limit","truncated":true}
```

几点设计：

- **流式写入**：抓一页写一页，Ctrl-C 或断网时已抓到的部分依然完整可用，末行会记下中断原因。
- **时间戳是 Unix 秒**，不是 RFC3339。`"1606652805"` 是 4 个 token，`"2020-11-29T16:26:45+08:00"` 是 25 个——上万条评论下这个差距是决定性的。
- **默认裁掉头像和表情图片地址**（`--full` 可保留）。正文里已经写了 `[微笑]` 这样的表情名，图片地址对文本分析没有价值。
- **`summary` 行是输出的关键**。下游拿到残缺数据却不知道它残缺，比拿不到数据更危险——基于部分评论得出的「用户普遍认为」是纯粹的幻觉。

`--mode time --since` 可以增量抓取：按时间排序时评论严格从新到旧，遇到第一条早于截止时间的就可以停，不必翻完全部。这个组合之外 `--since` 会被拒绝——按热度排序时评论不按时间递减，看到旧的就停会漏掉后面的新评论。

### 退出码

便于脚本批量调用：

| 码 | 含义 |
|---|---|
| 0 | 成功 |
| 1 | 一般错误 |
| 2 | 参数用法错误 |
| 3 | 触发风控 |
| 4 | 需要登录 |
| 5 | 评论区不可用 |

## 关于抓取频率

默认请求间隔 2.5 秒，约合 24 次/分钟。这个值是社区实测的安全线——连续超过 20 次/分钟就可能触发临时封禁，**调低 `--delay` 大概率会让你自己被封，而不是把这个工具变得更好用**。

```bash
bili video BV1xx --delay 5s    # 更保守
```

这个项目不做代理池，也不做账号池：

- cookie 与 IP 绑定，换 IP 会触发异地登录风控，反而更容易被封；
- 机房 IP 会被更严格地过滤；
- 新账号能看到的评论比老账号更少，账号池在逻辑上就是自相矛盾的。

降低请求量的正确做法是缓存和增量抓取，不是换 IP。

## 登录态

扫码登录后，cookie 保存在系统配置目录：

| 平台 | 路径 |
|---|---|
| Linux | `~/.config/bili-comment/config.json` |
| macOS | `~/Library/Application Support/bili-comment/config.json` |
| Windows | `%AppData%\bili-comment\config.json` |

**这组 cookie 等同于账号凭据**，拿到它就能以你的身份操作。文件权限设为 0600（仅当前用户可读），不要提交到版本库，也不要分享。

## 文档

- [DESIGN.md](DESIGN.md) —— 完整设计推演：技术选型、风控现实、抓取策略、输出格式、里程碑，以及实现过程中实测确认的事实（附录 B）。

## 许可

[MIT](LICENSE)
