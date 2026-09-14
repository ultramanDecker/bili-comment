# bili-comment

把 B 站视频的评论区提取成结构化文件，供程序或 LLM 消费。

单个静态链接的二进制，无运行时依赖。

## 状态

> **开发中。** 单视频抓取与四种输出格式已可用；批量抓取尚未实现。

| 命令 | 状态 |
|---|---|
| `bili video <视频>` | ✅ 视频元数据 |
| `bili login` / `logout` / `whoami` | ✅ 扫码登录与登录态缓存 |
| `bili comments <视频>` | ✅ 一级评论，游标翻页 + 提前停止 |
| `bili comments --replies-*` | ✅ 楼中楼，决策点 + 并发抓取 + 断点续传 |
| `bili comments --format` / `-o <目录>` | ✅ JSONL / TSV / Markdown / JSON，附 manifest、stats、schema |
| 批量抓取（多视频、双排序合并） | 🚧 开发中 |

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

bili comments BV1GJ411x7h7 -o out/              # 写入目录，得到一份数据集
bili comments BV1GJ411x7h7 -o out/ --format tsv # 数据集里再加一份 TSV
bili comments BV1GJ411x7h7 -o c.tsv --format md # 主格式 TSV，另出一份 Markdown

bili comments BV1GJ411x7h7 --replies-top 20             # 只展开最热的 20 栋楼中楼
bili comments BV1GJ411x7h7 --no-replies                 # 完全不要楼中楼
bili comments BV1GJ411x7h7 --replies-min-likes 100      # 楼中楼回复点赞过百才展开
bili comments BV1GJ411x7h7 --limit 2000 -o c.jsonl \
    --resume                                            # 中断后接着抓
```

| 参数 | 说明 |
|---|---|
| `-o, --out` | 写入文件或目录，默认打印到 stdout。见[输出](#输出) |
| `--format` | 额外输出这些格式。**只做加法**，不改变主格式 |
| `--limit` | 最多抓多少条一级评论，0（默认）表示不限 |
| `--mode` | `hot`（热度，默认）或 `time`（时间） |
| `--since` | 只抓该日期之后的评论，需配合 `--mode time` |
| `--full` | 保留头像与表情图片地址 |
| `--concurrency` | 并发抓取的楼数，默认 1。**不改变请求速率**，限速器才是刹车 |
| `--resume` | 从 `-o` 对应的进度文件续传，要求主格式是 JSONL |

楼中楼默认**全展开**。工具不替你做数据质量取舍，要做就显式做：

| 参数 | 说明 |
|---|---|
| `--no-replies` | 完全不抓楼中楼 |
| `--replies-top N` | 只展开回复数最多的 N 栋 |
| `--replies-min-likes N` | 楼中楼回复点赞数门槛 |
| `--replies-min-count N` | 楼的回复数门槛 |
| `--replies-include-up` | 无条件展开 UP 主参与过的楼 |
| `--replies-include-top` | 无条件展开置顶评论所在的楼 |

## 输出

四种格式，主数据永远是同一批记录，只是写法不同：

| 格式 | 扩展名 | 长什么样 | 什么时候用 |
|---|---|---|---|
| `jsonl` | `.jsonl` | 每行一条 JSON | 默认。程序消费、流式读取 |
| `tsv` | `.tsv` | 一张表，带 `#` 注释头 | token 最省，整份读进 LLM 上下文 |
| `md` | `.md` | 按楼分组的可读文本 | 人读，或让 LLM 读讨论脉络 |
| `json` | `.json` | 一个完整嵌套的 JSON 对象 | 对接只吃标准 JSON 的工具 |

`json` 要在内存里攒齐才能写出，**不能当主格式**（`-o x.json` 会被拒绝并提示改用
`--format json`），也不能写到 stdout——那会先卡住再一次性吐出，看上去像程序挂了。

### 主格式与 `--format`

主格式决定进度文件挂在哪，所以要有一个明确的：

- `-o comments.tsv` → 扩展名就是主格式，写单个文件
- `-o out/` 或 `-o` 指向已存在的目录 → **目录模式**，主格式固定是 `comments.jsonl`
- 没有扩展名 → 按 `jsonl` 处理

`--format` **只做加法**，不会顶掉主格式：

```bash
bili comments BV1xx -o c.tsv --format md,jsonl
# → c.tsv（主）、c.md、c.jsonl
```

目录模式**不写 `--format`** 时默认给 `jsonl` + `md`。一旦写了 `--format`，它就决定
除主格式以外要哪些——`--format tsv` 得到的是 `jsonl` + `tsv`，不会再多一份 `md`。
`jsonl` 始终会被补上：目录模式的产出是一份数据集，而 `jsonl` 是这份数据集的主数据，
也是续传的锚点。

### 数据集目录

`-o <目录>` 除了数据本身，还会写三份随附文件。它们存在的理由是**让 LLM 少读一点**：
一万条评论约五十万 token，塞不进任何上下文，而很多时候看完统计就能回答问题。

| 文件 | 内容 |
|---|---|
| `comments.jsonl` | 主数据 |
| `comments.md` | 可读版本（`--format` 可换成别的） |
| `stats.json` | 本地预聚合的统计：高频词、IP 属地、时间分布、点赞分段、复读榜 |
| `manifest.json` | 这个目录里有什么、各文件多少条、完整度结论 |
| `schema.md` | 字段与标记的自描述，以及「完整」到底指什么 |

`manifest.json` 里的 `read_first` 字段直接告诉你该先读哪两个文件。

### JSONL

由五类行组成，靠 `type` 字段区分：

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

### TSV

开头一段 `#` 注释，然后一行表头，然后**只有评论**，最后一段 `#` 完整度说明：

```tsv
# bili-comment 数据集，格式版本 1
# video	BV1xx411c7mD	字幕君交流场所
# up	2	碧诗
# stat_reply	89243
# mode	hot
# columns	rpid	root	parent	mid	name	like	ctime	rel	reply_count	location	flags	message
rpid	root	parent	mid	name	like	ctime	rel	reply_count	location	flags	message
495059			2	碧诗	54335	1291918239	1年	1708		short	wwwww
164517433	495059	495059	3476504	MaskQwQ麦斯科	647	1479570959	7年				拉了半天总算是见了底
#
# completeness	root	5	89243	truncated
# completeness	sub_replies	2911	2911	complete
# not_fetched	3 栋楼共 2202 条回复未抓取
```

`grep -v '^#' comments.tsv` 就得到一张干净的表：所有行字段数一致，不含元数据。
`rel` 是相对视频发布时间的说法（「3小时」「2年」），`flags` 见下。

正文里的 `\\`、制表符、换行分别转义成 `\\`、`\t`、`\n`，所以字段里不会出现真的
制表符——否则表就散架了。

### Markdown

```markdown
# 《字幕君交流场所》评论

> `BV1xx411c7mD` · UP **碧诗** · 发布 2009-09-09 · 服务端报告 89243 条评论
> 抓取于 2026-09-14 18:55，hot 排序

## 一级评论

### 1. 碧诗 · 👍54335 · 2010-12-10（1年） · 1708 条回复 · `short`
  wwwww

## 楼中楼

### ↳ 碧诗 的楼 · 1524 条回复（服务端报告 1708）

- **MaskQwQ麦斯科** · 👍647 · 2016-11-19（7年）
  拉了半天总算是见了底
- **klxhx160** · 👍65 · 2017-01-01（7年） · `short|repeat`
  www
  - **因为t善** · 👍0 · 2026-09-13（17年）
    回复 @未來星織 :我来负责考古，考古学家
```

收尾同样有一段「完整性」，把「哪些数据不在这个文件里」写清楚。

一级评论先全部列出，再单独一节放楼中楼，每栋楼聚在一起。层级靠缩进表示，
缩进深度从 `parent` 链还原，最多 8 层——再深的链条通常是异常的，继续缩下去
只会把正文挤出屏幕。

### `flags`

一个「这条有什么特别的」的列，用 `|` 分隔，可能同时有多个。**标记是提示不是过滤**：
带标记的评论仍然在数据里，怎么用由下游决定。

| 标记 | 含义 |
|---|---|
| `up` | UP 主本人发的评论 |
| `top` | 置顶评论 |
| `emoji_only` | 正文只有表情与标点，没有可分析的文本 |
| `short` | 正文 ≤ 5 字，通常只有情绪没有观点 |
| `lottery` | 抽奖评论，模板化文本 |
| `repeat` | 与之前某条评论近似相同（SimHash 汉明距离 ≤ 3） |

做统计时先按 `flags` 排掉 `emoji_only`、`short`、`repeat`，结论会干净很多——
热门视频的评论区里这三类常常占掉一半。

### 若干设计取舍

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

**续传。** `-o` 写入时会同时维护一个 `<主输出文件>.resume.json` 进度文件。中断后加
`--resume` 即可接着抓，**不重复、不遗漏**：已确认完整的部分原样保留，上次写了一半的
那栋楼被丢掉重抓。全部完成后进度文件会自动删除。进度文件会校验楼中楼参数、输出格式
集合是否与上次一致——参数变了计划就变了，接着写会错位，此时会明确报错要求换参数或换
输出文件。

进度按**每种格式分别记录**写入位置：一条评论在 JSONL 里约 300 字节，在 TSV 里约 120，
共用一个偏移会让至少一个文件停在半条记录中间，所以每种格式各截各的。

**`--resume` 要求主格式是 JSONL。** 续传时要先把已抓到的一级评论读回来重建楼中楼
计划，而 TSV 和 Markdown 都是单向渲染，读不回原始字段——用 `-o x.tsv --resume` 会被
拒绝。目录模式的主格式固定是 `comments.jsonl`，所以不受影响。

**`--concurrency` 通常不必调。** 它只填响应延迟留下的空档，不提高请求速率：所有请求
仍要一个个过限速器，总耗时约等于 `总页数 × 请求间隔`。只有服务端比请求间隔还慢时
（响应时间 > 间隔）并发才有意义。默认 1 是诚实的选择，不是保守。

`--mode time --since` 可以增量抓取：按时间排序时评论严格从新到旧，遇到第一条早于截止时间的就可以停，不必翻完全部。这个组合之外 `--since` 会被拒绝——按热度排序时评论不按时间递减，看到旧的就停会漏掉后面的新评论。

## 退出码

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
