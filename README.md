# bili-comment

把 B 站视频的评论区提取成结构化文件，供程序或 LLM 消费。

单个静态链接的二进制，无运行时依赖。

## 状态

> **开发中。** 核心的评论抓取还没实现，当前完成的是元数据与登录。

| 命令 | 状态 |
|---|---|
| `bili video <视频>` | ✅ 视频元数据 |
| `bili login` / `logout` / `whoami` | ✅ 扫码登录与登录态缓存 |
| `bili comments <视频>` | 🚧 开发中 |

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
