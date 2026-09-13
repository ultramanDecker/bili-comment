# bili-comment 设计文档

把 B 站视频的评论区提取成结构化文件，主要消费方是 LLM。

现成工具的主要问题是：字段与输出格式耦合、无断点续传、无请求量控制、错误信息晦涩。
本项目按「抓取能力」与「LLM 消费友好」两条主线重新设计。

---

## 1. 技术选型：Go

硬性约束是**跨平台静态单二进制**，且开发环境是 Windows。加权对比（权重按本项目痛点定，运行时性能权重为 0——瓶颈是平台限速，不是 CPU）：

| 维度 | 权重 | Go | Rust | C++ |
|---|---:|---:|---:|---:|
| 交叉编译 · 静态单文件 | 18 | **5** | 3 | 1 |
| 迭代速度 | 18 | **5** | 3 | 2 |
| HTTP / Cookie / 重定向 | 15 | **5** | 4 | 3 |
| 并发抓取 | 12 | **5** | 4 | 2 |
| JSON 脏数据处理 | 12 | 3 | **5** | 2 |
| 错误处理正确性 | 8 | 4 | **5** | 2 |
| 他人上手门槛 | 8 | **5** | 3 | 2 |
| Windows 开发体验 | 6 | **5** | 4 | 2 |
| 二进制体积 | 3 | 3 | **5** | **5** |
| **总分** | 100 | **462** | 367 | 206 |

**决定性因素**：在 Windows 上，Rust 和 C++ 无法合法产出 macOS 二进制（需要 Apple SDK，许可禁止非 Apple 硬件使用）。Go 的 darwin 链接器是纯 Go 实现，`GOOS=darwin go build` 直接可用。

**Rust 反超的条件**：若 macOS 交给 CI、且维护者本身熟悉 Rust，serde 处理 B 站脏 JSON 的优势（`like` 字段时而是数字时而是字符串、`vip` 时而是对象时而是 null）会拉近差距。届时架构不变，换实现语言即可。

**依赖克制**（保持二进制精简）：
- `spf13/cobra` — 子命令
- `skip2/go-qrcode` — 扫码登录
- `modernc.org/sqlite` — 缓存（**注意：`mattn/go-sqlite3` 需要 CGO，会破坏静态链接，不可用**）
- 终端样式、进度条自己写 ANSI，不引 lipgloss/pterm

---

## 2. 架构分层

```
bili-comment/
├── cmd/bili/main.go
└── internal/
    ├── cli/          # cobra 命令，只做参数解析与编排
    ├── bilibili/     # API 层
    │   ├── client.go # 签名/限速/重试/cookie jar
    │   ├── wbi.go    # WBI 签名（隔离在此，官方改算法时只动这个文件）
    │   ├── video.go  # 视频元数据
    │   ├── comment.go# 评论接口
    │   └── types.go  # API 原始响应结构
    ├── auth/         # 扫码登录 + cookie 持久化
    ├── model/        # 领域模型（归一化后的干净结构）
    ├── output/       # Formatter 接口 + 各格式实现
    └── config/       # 路径解析、配置读写
```

**核心原则：`bilibili` 层返回原始 API 结构，`model` 层是归一化结构，`output` 只认 `model`。**
平台改字段名时只动一个文件，输出格式永远稳定。现成工具难用，很大一部分就是 API 字段和输出格式耦死在一起。

领域模型预留 `Danmaku` 结构与 `SnapshotID` 字段，便于后续扩展弹幕与快照对比，避免届时重构。

---

## 3. 抓取策略

### 3.1 风控现实

社区实测经验值（各方口径一致）：

| 指标 | 值 |
|---|---|
| 单 IP 请求间隔 | 建议 ≥ 3 秒 |
| 触发临时封禁 | 连续快速请求 > 20 次/分钟 |
| QPS 上限 | 约 30 次/分钟 |
| 保守配置 | 并发 1、base_delay 3.0s、风控恢复期 600s |

**默认速率：1 请求 / 2.5 秒（约 24 次/分钟）。** 用户显式调高才加速。

风控升级路径：前期正常 → 第 3 页左右开始报错 → 连续失败、延迟递增 → 任务停止。严重时会**临时封禁账号**，不只是 IP。

### 3.2 单排序 5000 条上限

单一排序（时间或热度）大约只能翻到 5000 条评论。`--complete` 启用双排序合并（按时间抓一遍 + 按热度抓一遍，合并去重）突破该限制，代价是请求数 ×2。

### 3.3 请求量是第一约束

按 24 req/min 计算：

| 场景 | 请求数 | 耗时 |
|---|---|---|
| 万评视频（主评论 3k + 楼中楼） | ~3,200 | ~2.2 小时 |
| 100 个视频 | ~320,000 | ~9 天 |

单视频可忍，批量是灾难。**提高速率解决不了问题**——10 倍速率也只是 9 天变 22 小时，且必然被封。唯一有效的杠杆是降低请求数：

1. **缓存** — 同一视频永不重复抓（收益最大，纯工程问题）
2. **增量** — 时间序抓取，遇到已知 rpid 立即停止
3. **选择性展开楼中楼** — 见 3.4，省 60–70% 请求
4. **双排序合并** — 仅当确实触达 5000 上限时启用

### 3.4 两阶段抓取与决策点

```
Phase 0  解析输入 → BV/aid、视频元数据、评论总数
Phase 1  抓主评论（可按时间范围 / max-pages 提前停止）
         ↓
    ⭐ 决策点：选择哪些 root 展开楼中楼
         ↓
Phase 2  并发抓楼中楼
Phase 3  输出（标注 + 本地过滤 + 多格式）
```

**决策点是架构上唯一的新增结构。** 因为评论区的回复量分布极度不均——楼中楼请求数等于 root 总数，而回复量集中在前 20% 的热门评论下，不展开长尾直接砍掉 60–70% 请求。这一点**无法通过「全抓之后本地过滤」实现**，因为全抓正是要避免的成本。

**默认全展开**（诚实、完整），选择性展开是显式的性能逃生口。用户抓万评视频发现要跑 2 小时，自然会用 `--replies-top`。不要让工具替用户默默做数据质量取舍。

### 3.5 不做的两件事

- **代理池** — 对登录态是**负收益**：cookie 与 IP 绑定，换 IP 触发异地登录风控，轻则验证码重则冻结账号；且廉价代理多为机房 IP，平台对机房网段有额外风控。
- **账号池** — 技术上自相矛盾：新注册账号的评论可见范围与风控阈值都更差，每个号能拿到的数据更少；多账号会通过设备指纹（`buvid3`/`b_lsid`）关联。

真到万级视频规模，正确做法是拉长时间窗口 + 缓存 + 增量，而非堆小号。

---

## 4. 输出设计

### 4.1 格式矩阵

给 LLM 喂数据，JSONL 不是最优——**键名在每条记录里重复**，10k 条评论中 `"message":`、`"user":` 这类键名占掉的 token 比内容本身还多。

同一行数据两种编码（约 55 token vs 28 token，省约 50%）：

```
{"rpid":196234567890,"user":"阿伟","like":45,"ctime":"2026-01-01T10:00:00Z","msg":"这个转场做得真好"}
```
```
196234567890	阿伟	45	2026-01-01 10:00	这个转场做得真好
```

| 用途 | 格式 | 理由 |
|---|---|---|
| 程序消费 / 存档 | JSONL | 完整、可复现、字段稳定 |
| **LLM 消费** | **TSV** | 省 40–50% token，表头只写一次 |
| LLM 消费（带层级）/ 人类阅读 | Markdown | 缩进表达楼中楼，LLM 对 markdown 理解通常优于嵌套 JSON |
| 兼容性 | JSON | 嵌套完整，可直接喂给任意工具 |

默认输出 `jsonl + md`，分析场景手动切 `tsv`。`--format` 支持多值同时输出。

**该设计约束了 `Formatter` 接口**：需要能拿到字段元信息（列名、类型），而不只是写记录。

### 4.2 两级读取

10k 条评论约 50 万 token，塞不进上下文。输出一个目录让 LLM 先做粗判断：

```
dataset/
├── manifest.json      # 数据集描述：规模、时间跨度、字段、分片
├── stats.json         # 本地预聚合
├── schema.md          # 自描述字段说明
├── comments.tsv       # 主数据
└── shards/            # 按时间/热度分片
```

`stats.json` 放**纯本地可算、不花 LLM token 的东西**：高频词 Top100、IP 属地分布、时间桶分布、点赞直方图、独立用户数、重复评论 Top20。

很多时候 LLM 看完 stats 就能回答问题，无需读全量。**这一设计同时是 MCP 接口契约**（见第 8 节）。

### 4.3 完整度元数据

```json
{
  "completeness": {
    "root_comments": {"expected": 3421, "fetched": 3421},
    "sub_replies":   {"expected": 8912, "fetched": 5340, "reason": "rate_limited"},
    "truncated": true
  }
}
```

若因风控只抓到 60% 楼中楼，**LLM 必须知道**，否则会基于残缺样本给出「评论区主流观点是 X」这类过度自信的结论。这是诚实性问题，也是本项目比现成工具更可信的地方。

### 4.4 墓碑机制（标注优先于丢弃）

过滤是破坏性的且对 LLM 不可见。`--min-likes 100` 丢掉 3000 条评论后，LLM 不知道被丢了什么。

**不静默过滤，而是留墓碑**：

```jsonc
{"rpid": 196234, "root": 0, "type": "reply_stub",
 "reply_count": 37, "expanded": false, "reason": "below_threshold"}
```

LLM 看到这行就知道「这楼有 37 条我没看到」，结论自然更谨慎。成本是每楼一行。

### 4.5 其他 LLM 向细节

- **表情转文本** — `[doge]` 有实际情感语义，`Emotes` 不能只存图片 URL
- **相对时间** — `ctime` 保留绝对时间的同时给出相对视频发布时间的表述，对 LLM 更有意义
- **slim / full** — 默认剔除 `avatar`/`nameplate`/`pendant` 等纯 UI 字段（一条评论原始响应约 2KB，有语义的不超过 200 字节）
- **噪声标注** — 复读检测（SimHash，抓改一字的变体）、抽奖评论（固定格式特征）、纯表情/极短评论，一律打 `flags` 而非过滤

---

## 5. Filter 分层

> **一个过滤条件只有在「减少 HTTP 请求数」时才有资格进 fetch-time。否则它属于输出层，且对 LLM 场景应优先做成标注而非丢弃。**

### 5.1 前提：平台几乎不提供服务端过滤

接口只给三样能力：**排序选择**（`mode=2` 时间 / `mode=3` 热度）、**提前停止**（游标翻页随时可停）、**选择抓不抓子集**。没有 `keyword`、`min_likes`、`uid` 参数。

### 5.2 判定表

| 条件 | 减少请求? | 机制 | 归属 |
|---|---|---|---|
| 楼中楼展开选择 | ✅ 60–70% | 两阶段决策 | **fetch-time** ⭐ |
| 增量 `--since-last` | ✅ 巨大 | 时间序，遇已知 rpid 即停 | **fetch-time** ⭐ |
| 时间范围 `--since/--until` | ✅ 显著 | `mode=2` 有序 → 越界即停 | **fetch-time** |
| `--max-pages/--max-comments` | ✅ 线性 | 硬截断 | fetch-time |
| 双排序 `--complete` | ❌ 反向 | 请求 ×2，突破 5000 上限 | fetch-time（条件启用） |
| `--min-likes` 主评论 | ❌ | 热度序不单调，early-stop 不可靠 | output |
| `--keyword` | ❌ | 无服务端搜索 | **不做**（下游 grep 即可，工具内做纯属重复且带偏） |
| `--only-up` | ❌ | UP 主回复分散，无法定向查 | output |
| 去重 / 复读 | ❌ | 须全量才能算 | output（标注） |
| 抽奖 / 机器人 / 纯表情 | ❌ | 同上 | output（标注） |

### 5.3 阈值过滤的逃生口

**任何阈值过滤都必须提供强制保留**，否则会丢掉最有信号的部分：

- `--replies-include-up` — UP 主参与的对话往往最有分析价值，但点赞数可能很低，会被 `--min-likes` 误杀
- `--replies-include-top` — 置顶评论同上

---

## 6. 命令面

```bash
bili login                      # 扫码登录，二维码渲染在终端
bili logout
bili whoami                     # 当前登录态

bili comments <url|bvid|av号>   # 核心命令
    # 输入解析
    --all-parts                     多P视频全部抓
    # 请求量控制（fetch-time，默认保守）
    --since / --until               时间范围
    --max-pages N                   硬截断
    --incremental                   增量，遇已知 rpid 停
    --complete                      突破 5000 上限，双排序合并
    --replies-top N                 只展开最热的 N 个 root
    --replies-min-likes N           只展开点赞 ≥N 的主评论
    --replies-min-count N           至少 N 条回复才展开
    --replies-include-up            强制包含 UP 主参与的楼
    --replies-include-top           强制包含置顶
    # 速率
    --concurrency 1 --delay 2.5s
    # 输出
    --format jsonl,md               默认 jsonl,md
    --slim / --full                 默认 slim
    --out ./data/
    --annotate-dupes                复读检测打 flag（默认开）
    --annotate-noise                抽奖/机器人/纯表情打 flag（默认开）
    --only-up                       只输出 UP 主相关（本地筛，无请求收益）

bili video <url|bvid>           # 只要元数据
bili batch --from-file urls.txt # 批量
bili mcp                        # 以 MCP server 模式运行（stdio）
```

**输入解析要宽容**：BV 号、av 号、完整 URL、带分P的 URL、`b23.tv` 短链（跟一次 302）全部接受。直接粘贴浏览器地址栏就能跑——这是「好用」与「难用」的分界线。

---

## 7. 错误处理

### 7.1 错误码

| 码 | 含义 | 行为 |
|---|---|---|
| 0 | 成功 | — |
| -403 | 签名错误 | 刷新 WBI 密钥重试一次 |
| -352 | 频率限制 | 指数退避 + 自动降速 |
| -799 | 请求过于频繁 | 同上 |
| -412 / 其他负数码 | 按风控处理 | 连续 3 次报错并建议加大 `--delay` |
| -404 | 视频不存在 | 明确提示 |
| 12002 | 评论区已关闭 | 明确提示 |
| 12061 | 需登录 / 仅粉丝可见 | 提示 `bili login` |

> 注：社区文档明确记载的风控码是 **-352 / -799 / -503**。-412 未找到可靠出处，实现时以实测为准。

### 7.2 退出码

`0` 成功 / `1` 通用错误 / `2` 用法错误 / `3` 风控 / `4` 需登录 / `5` 无评论

### 7.3 进度可见

stderr 实时显示 `已抓 1,240 条主评论 / 楼中楼 3,891 条 / 第 12 页`，而不是黑盒卡住不动。

---

## 8. 扩展形态：MCP 与 Skill

### 8.1 判据：agent 有没有 shell

| 场景 | 需要 MCP 吗 |
|---|---|
| Claude Code / Cursor 等 coding agent | ❌ 不需要，直接跑 CLI 读文件 |
| Claude Desktop / ChatGPT / 网页版 | ✅ 必须，MCP 是唯一通道 |
| 自建 agent 框架 | ⚠️ 多半可包成普通 tool |

MCP 的价值在于触达「没有终端」的场景。

### 8.2 已有设计即是 MCP 接口

MCP tool 的结果**要进上下文窗口**——CLI 可以吐 20MB 文件，MCP tool 不行。这迫使我们做两级读取，而第 4.2 节的 `manifest` / `stats` / `completeness` / 墓碑行**正好就是 MCP 的接口契约**。

`completeness` 在 MCP 场景下比 CLI 更关键：模型可能只读 `stats.json` 就下结论，没有这个字段它永远不知道自己在残缺样本上说话。

### 8.3 工具集

```
fetch_comments(url, opts)          → 触发抓取（慢、有副作用），只返回元信息
get_comments(dataset_id, …)        → 读已抓数据，支持分页/范围/分片
search_comments(dataset_id, query) → 本地搜索，避免模型自己写 grep
list_datasets()                    → 已抓数据集清单
fetch_video_meta(url)              → 只要元数据，快
```

三个要点：
1. **描述就是 prompt**——`fetch_comments`（慢、有副作用、返回元信息）与 `get_comments`（快、只读）的区别必须写清楚，否则模型会反复误调用。这是 MCP server 最常见的失败模式，与代码质量无关。
2. **`get_comments` 必须自带返回量上限**并告知「还有 N 条」——模型不会自觉控制。
3. **Tools vs Resources**——规范上只读数据该用 Resources，但各家宿主对 Resources 支持远不如 Tools 成熟。务实选择：全做成 Tools。

### 8.4 Skill 的定位

Skill 编码**判断规则**，不编码函数：

- 分析前先读 `stats.json`，很多时候不用读全量
- 见到 `expanded: false` 的墓碑行，意味着那里有 N 条未抓取的回复
- `completeness.truncated` 为 true 时，不要使用「主流观点是 X」这类全称判断
- 引用评论时带上 `rpid` 便于溯源
- `ctime` 优先用相对视频发布时间的表述

写在 skill 里而非硬编码进工具，用户就能改。

### 8.5 实现陷阱

MCP stdio 传输要求 **stdout 是纯 JSON-RPC，一个杂字符都不能有**。而 CLI 模式要在 stderr 打进度条和 ANSI 颜色。

**`bili mcp` 模式必须彻底静默所有输出**，日志只走 stderr。这类 bug 极难排查——JSON 解析失败却看不到原因，因为污染 stdout 的是自己的进度条。

Go SDK 选型：`mark3labs/mcp-go` 或官方 `modelcontextprotocol/go-sdk`，实现前确认维护状态。

**单二进制在此再次显出优势**：同一个二进制既是 CLI 又是 MCP server，无需额外部署。

---

## 9. 里程碑

| 阶段 | 内容 | 验证标准 |
|---|---|---|
| M1 | 骨架 + WBI 签名 + 输入解析 + 视频元数据 | `bili video BV1xx` 输出正确 JSON |
| M2 | 扫码登录 + cookie 缓存 + buvid3 自动获取 | `bili whoami` 显示昵称 |
| M3 | 主评论 + 游标翻页 + early-stop + JSONL | 小视频评论数与页面一致 |
| M4 | 决策点 + 楼中楼并发 + 缓存 + 断点续传 | 中断后 `--resume` 不重复不遗漏 |
| M5 | 多格式 + manifest/stats + 标注 + schema | 多格式同时输出，TSV 可被 LLM 消费 |
| M6 | 批量 + `--complete` 双排序 + goreleaser | GitHub Release 六平台可下载 |
| M7 | MCP server + skill（可选） | Claude Desktop 能调用 |

登录放在 M2 而非最后：不登录只能看到前几页、且很多视频直接返回空，早接上后面每步都能用真实数据验证。

---

## 10. 已知风险与待实测确认项

| 项 | 说明 |
|---|---|
| WBI 算法变更 | 隔离在 `wbi.go` 单文件，改起来是分钟级。这是唯一需要长期与官方对抗的地方 |
| **游标字段路径** | `data.cursor` 下 `next_offset` 的确切路径需用真实视频实测确认，不靠记忆写死 |
| **`-412` 语义** | 社区文档无可靠出处，以实测为准 |
| 风控是概率问题 | 设计上接受「慢但稳」：宁可 24 req/min 跑十分钟，不要 200 req/min 被 ban 十分钟 |
| Cookie 存储 | 配置文件 0600 权限。Windows 上 POSIX 权限位不生效，会额外提示文件位置；暂不做 DPAPI 加密，跨平台复杂度不划算 |
| 弹幕 | 本期不做，但 `model` 层预留结构与 `WriteDanmaku` 接口 |

---

## 附：数据模型

```go
type Video struct {
    BVID, AID, Title, Desc string
    UpMid  uint64; UpName string
    PubTime time.Time
    Duration int
    Stat   Stats   // view/danmaku/reply/favorite/coin/share/like
    Pages  []Page
}

type Comment struct {
    Rpid, Root, Parent uint64  // Root=0 表示一级评论
    Level      int             // 0=一级, 1=楼中楼
    User       User            // mid/uname/level/vip/up身份
    Message    string
    Emotes     map[string]string
    Pictures   []string
    Like       int
    Ctime      time.Time
    Location   string          // IP 属地
    ReplyCount int             // 该楼回复总数
    IsUp, IsTop, IsFolded bool
    Flags      []string        // ["duplicate_of:196234", "possible_lottery", ...]
}
```

---

## 附录 B：实施记录（已实测确认的事实）

设计阶段靠社区资料推断的部分，在实现中用真实接口逐条核对，结论记录在此，
避免以后有人照着错误的前提改代码。

### M1

| 项 | 结论 |
|---|---|
| WBI 签名 | 实测通过。服务端接受本地算出的 `w_rid`（`/x/web-interface/wbi/view` 返回正常数据），置换表与 MD5 拼接方式正确 |
| BV↔AV 换算 | 双向与官方接口一致。**进制是 58 而非 36**——用 36 时 6 位数装不下 `(aid ^ xor) + add` 的数值域，会得到格式合法但指向错误视频的 BV 号 |
| `nav` 未登录行为 | 返回 `code=-101`，但 `data.wbi_img` 字段依然存在，取密钥不必先登录 |
| 获取 buvid3 | `/x/frontend/finger/spi` 无需登录即可拿到 `b_3`/`b_4` |

### M2

| 项 | 结论 |
|---|---|
| 扫码轮询的状态码 | 在 `data.code` 里而**不是**外层 `code`（外层恒为 0）。只看外层会永远显示「等待扫码」 |
| 二维码内容长度 | 实测约 131 字符（轮询凭据是 32 位十六进制），编码后 57×57 模块，含静区宽 57 列，80 列终端放得下 |
| 登录 cookie 来源 | 成功时 `data.url` 是跨域回调地址，cookie 以查询参数形式挂在上面；同时响应头也有 `Set-Cookie`。**两条路都要收**，只靠 `Set-Cookie` 偶尔缺 `DedeUserID` |
| `gourl` 参数 | 是登录后的跳转目标，不是 cookie，必须排除，否则会污染后续请求头 |
| 轮询节奏 | 2 秒一次，不走抓取用的限速器。扫码是短请求且总量固定（约 90 次），套用 2.5 秒的抓取间隔只会让扫码体验变差 |

登录态落盘在 `os.UserConfigDir()/bili-comment/config.json`，权限 0600。
它等同于账号凭据：不要提交到版本库，不要分享。
