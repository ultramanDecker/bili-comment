# bili-comment

把 B 站视频的评论区提取成结构化文件，供程序或 LLM 消费。

单个静态链接的二进制，无运行时依赖。

## 状态

> **开发中。** 一级评论与楼中楼已可用；多格式输出、批量抓取尚未实现。

| 命令 | 状态 |
|---|---|
| `bili video <视频>` | ✅ 视频元数据 |
| `bili login` / `logout` / `whoami` | ✅ 扫码登录与登录态缓存 |
| `bili comments <视频>` | ✅ 一级评论，游标翻页 + 提前停止 |
| `bili comments --replies-*` | ✅ 楼中楼，决策点 + 并发抓取 + 断点续传 |
| 多格式输出 / 批量 | 🚧 开发中 |

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

bili comments BV1GJ411x7h7 --replies-top 20             # 只展开最热的 20 栋楼中楼
bili comments BV1GJ411x7h7 --no-replies                 # 完全不要楼中楼
bili comments BV1GJ411x7h7 --replies-min-likes 100      # 楼中楼回复点赞过百才展开
bili comments BV1GJ411x7h7 --limit 2000 -o c.jsonl \
    --resume                                            # 中断后接着抓
```

| 参数 | 说明 |
|---|---|
| `-o, --out` | 写入文件而非 stdout |
| `--limit` | 最多抓多少条一级评论，0（默认）表示不限 |
| `--mode` | `hot`（热度，默认）或 `time`（时间） |
| `--since` | 只抓该日期之后的评论，需配合 `--mode time` |
| `--full` | 保留头像与表情图片地址 |
| `--concurrency` | 并发抓取的楼数，默认 1。**不改变请求速率**，限速器才是刹车 |
| `--resume` | 从 `-o` 对应的进度文件续传 |

楼中楼默认**全展开**。工具不替你做数据质量取舍，要做就显式做：

| 参数 | 说明 |
|---|---|
| `--no-replies` | 完全不抓楼中楼 |
| `--replies-top N` | 只展开回复数最多的 N 栋 |
| `--replies-min-likes N` | 楼中楼回复点赞数门槛 |
| `--replies-min-count N` | 楼的回复数门槛 |
| `--replies-include-up` | 无条件展开 UP 主参与过的楼 |
| `--replies-include-top` | 无条件展开置顶评论所在的楼 |

输出是 **JSONL**（每行一条 JSON），由五类行组成，靠 `type` 字段区分：

```jsonl
{"type":"video","bvid":"BV1GJ411x7h7","title":"...","stat_reply":232702,"mode":"hot",...}
{"rpid":3760801399,"user":{"mid":320773657,"name":"老咸鱼A","level":6},"message":"...","like":751988,"ctime":1606652805,"reply_count":2538}
{"rpid":4012538137,"root":3760801399,"parent":3760801399,"user":{"mid":441145305,"name":"WtfWasThat","level":6},"message":"[支持]","like":1482,"ctime":1611752781}
{"type":"reply_stub","rpid":3463001426,"reply_count":802,"expanded":false,"reason":"not_in_top_n"}
{"type":"summary","expected":232705,"fetched":20,"pages":1,"reason":"limit","truncated":true,"replies":{"expected":6644,"fetched":1358,"expanded":3,"skipped":17,"truncated":true,"skip_reasons":{"not_in_top_n":17}}}
```

楼中楼是**平铺**的：`root` 恒等于所在楼的 rpid，`parent` 指向被直接回复的那条
（可能是楼主，也可能是楼里另一条回复）。层级关系靠这两个字段还原，要不要重建树
由下游决定。

几点设计：

- **流式写入**：抓一页写一页，Ctrl-C 或断网时已抓到的部分依然完整可用，末行会记下中断原因。
- **时间戳是 Unix 秒**，不是 RFC3339。`"1606652805"` 是 4 个 token，`"2020-11-29T16:26:45+08:00"` 是 25 个——上万条评论下这个差距是决定性的。
- **默认裁掉头像和表情图片地址**（`--full` 可保留）。正文里已经写了 `[微笑]` 这样的表情名，图片地址对文本分析没有价值。
- **`summary` 行是输出的关键**。下游拿到残缺数据却不知道它残缺，比拿不到数据更危险——基于部分评论得出的「用户普遍认为」是纯粹的幻觉。
- **墓碑行不静默丢数据**。被 `--replies-top` 之类裁掉的楼会留下一条 `reply_stub`，
  写明它有多少条回复、为什么没展开。下游因此能分辨「这楼没有回复」和「这楼有 802
  条回复但我们没抓」——直接过滤掉的话，这两件事在输出里长得一模一样。
- **两种回复数不可比，别拿它们对账**。`reply_stub.reply_count` 是评论对象上的
  `reply_count`，对没抓的楼我们只有这个数，它是**上界**；而
  `summary.replies.expected` 是楼中楼接口的 `page.count` 之和，也就是**真正能抓到的**
  条数。两者实测差 13–19%（如 2538 vs 2128），差额是已删除/被过滤的回复，
  抓取并没有漏。`truncated` 按后者算。
- **`replies.fetched` 可以略大于 `replies.expected`**。前者是实际抓到的条数，
  后者是服务端声称的条数，而服务端的计数会滞后或少报（实测少 1–2 条）。
  以 `fetched` 为准，`truncated` 只在 `fetched < expected` 时为真。

**续传。** `-o` 写入时会同时维护一个 `<输出文件>.resume.json` 进度文件。中断后加
`--resume` 即可接着抓，**不重复、不遗漏**：已确认完整的部分原样保留，上次写了一半的
那栋楼被丢掉重抓。全部完成后进度文件会自动删除。进度文件会校验楼中楼参数是否与上次
一致——参数变了计划就变了，接着写会错位，此时会明确报错要求换参数或换输出文件。

**`--concurrency` 通常不必调。** 它只填响应延迟留下的空档，不提高请求速率：所有请求
仍要一个个过限速器，总耗时约等于 `总页数 × 请求间隔`。只有服务端比请求间隔还慢时
（响应时间 > 间隔）并发才有意义。默认 1 是诚实的选择，不是保守。

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
