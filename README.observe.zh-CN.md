# Magpie 观测版

这是 [yetone/magpie](https://github.com/yetone/magpie) 的一个面向「应用把 Magpie 当模型代理，并保存可识别会话」的裁剪版，仓库在 [SureD/magpie](https://github.com/SureD/magpie)。上游版权与 MIT 许可保留在 [LICENSE](LICENSE)（Copyright (c) 2026 yetone）。本版改动同样按 MIT 提供。

观测版不做对话摘要。它保存的是客户端自己声明的会话里，经网关转发的用户消息、助手消息和工具调用/结果，以及用量账本里的 token、延迟和（仅当已有价格元数据时）费用。

## 安装（普通使用）

不需要安装 Go，也不需要打开终端。

1. 在本仓库的 Actions 里打开 **macOS app (unsigned)**，下载产物 `magpie-darwin-arm64-unsigned`。里面是 `.zip`、`.dmg` 和说明。这是 **Apple Silicon（arm64）** 的桌面应用。
2. 用访达打开里面的 zip，或打开 dmg，把 `magpie.app` 拖到「应用程序」。不要再用别的工具把 `.app` 重新压缩一遍。
3. 这个应用有**临时签名**（ad-hoc，`codesign --sign -`），只用来保证包是完整的。它**没有** Developer ID 证书，也**没有公证**。macOS 不会把它当成已公证软件。
4. 第一次打开会被拦住。这是未公证应用的正常结果，不是要求你关闭 Gatekeeper。按 Apple 的做法，只放行这一个应用（[打开来自身份不明开发者的 Mac App](https://support.apple.com/zh-cn/guide/mac-help/mh40616/mac)）：先打开一次，在提示上点「完成」，再到「系统设置 → 隐私与安全性」里对 magpie 点「仍要打开」。也可以按住 Control 再点 `magpie.app`，选「打开」。不要执行 `spctl --master-disable`。
5. 窗口会打开，应用也住在菜单栏里。

若系统说 **「magpie 已损坏，无法打开，请移到废纸篓」**，并且「隐私与安全性」里没有「仍要打开」，先确认这个副本的签名，不要先改系统策略：

```sh
codesign --verify --deep --strict --verbose=2 /Applications/magpie.app
codesign -dv --verbose=4 /Applications/magpie.app
spctl --assess --verbose=4 --type execute /Applications/magpie.app
```

`valid on disk`、`Signature=adhoc`、`TeamIdentifier=not set` 表示文件没有损坏。这时 `spctl` 拒绝（例如 `no usable signature`）是未公证的预期结果。签名完好但仍然没有「仍要打开」时，只去掉这一个应用的下载标记后再打开：

```sh
xattr -dr com.apple.quarantine /Applications/magpie.app
```

这不改变其他应用。`sealed resource is missing`、`invalid signature` 或 `linker-signed` 才表示这个副本的封条坏了：重新下载，并用访达解压，不要用会改写包内容的压缩工具。

窗口左侧仍是原来的导航：

- **Providers**：Base URL、API key、模型、账号，和上游一样。
- **Settings**：原来的设置。
- **Conversations（会话记录）**：第一次打开会停在这里。记录默认关闭，在这一页确认后才保存对话。会话列表、单次请求、趋势和导出都在这个窗口里，不需要另开浏览器，也不需要复制带密钥的地址。

网关由这个应用在本机启动（默认 `127.0.0.1:3425`）。把应用的 base URL 指到它即可。

这个构建不是 GitHub Release，也不会自动更新。产物在 Actions 里大约保留 14 天。Linux 上的编译不能代替这台 Mac 的安装包，也不能做临时签名或公证。

## 给开发者

编译桌面应用需要在对应系统上构建（macOS 应用要在 Mac 上编，因为要链接系统 WebKit）。Go 版本见 `go.mod`。

```sh
make app          # 当前 Mac 上的 magpie.app（临时签名，未公证）
make cli          # 只有终端的 magpie，没有窗口
```

`magpie observe` 是可选的无界面模式：网关加一个带密钥的本机页面，给没有桌面的机器。普通使用不要走这条命令。它不启动菜单栏和主窗口。

已退役、改由社区插件提供的订阅仍然依赖插件宿主，没有从模块里删掉。技能同步、共享记忆和库 hub 在 `magpie observe` 里不启动；桌面应用仍保留上游的 Providers、Settings 和网关。

## 让应用带上会话

没有会话标识的请求**不会**被记成对话，也不会被猜成同一段历史。客户端使用自己的 ID，每次请求都带上：

```sh
# OpenAI Chat
curl http://127.0.0.1:3425/v1/chat/completions \
  -H "Authorization: Bearer magpie" \
  -H "X-Magpie-Session: app-session-1" \
  -H "X-Magpie-Project: demo" \
  -H "Content-Type: application/json" \
  -d '{"model":"your-provider/your-model","messages":[{"role":"user","content":"ping"}]}'

# OpenAI Responses
# POST http://127.0.0.1:3425/v1/responses
# 同样带 X-Magpie-Session

# Anthropic Messages
# POST http://127.0.0.1:3425/v1/messages
# Authorization: Bearer magpie
# 或 x-api-key: magpie

# Gemini
# POST http://127.0.0.1:3425/v1beta/models/{model}:generateContent
# x-goog-api-key: magpie
```

`X-Magpie-Session` 优先于各客户端自带的会话头（例如 Codex 的 `session_id`、Claude Code 的会话头）。只带那些原生头、不带 `X-Magpie-Session` 时，网关沿用该客户端原来的规则。两种方式都是「客户端声明的 ID」，不是 Magpie 生成的。

`X-Magpie-Project` 可选。没有这个头就不会出现项目名，观测版不会从工作目录或提示词推断项目。

响应里的 `X-Magpie-Session` 会回显客户端声明的 ID，便于核对。

## 记录与保留

记录默认关闭。在观测页「记录」中填写确认语后才会把正文写入本机：

`我了解对话正文可能包含源代码与隐私`

文件在 Magpie 配置目录的 `gateway-conversations/`（Linux 上通常是 `~/.config/magpie/`）。权限是目录 `0700`、文件 `0600`。

`gateway-retention.json`（与 `settings.json` 同级，不随同步走）：

```json
{
  "days": 0,
  "store_mb": 0,
  "session_mb": 256
}
```

| 字段 | 含义 |
| --- | --- |
| `days` | 保留的天数。`0` 表示不按时间删除。缺省文件等于 **7**。 |
| `store_mb` | 正文总容量，超出后从最旧的文件删。`0` 表示不按容量删。缺省 **256**。 |
| `session_mb` | 单个会话在一个 UTC 日的 `turns.jsonl` 上限。`0` 表示不限制该文件。缺省 **256**。 |

单轮仍最多 1 MiB，单段文本最多 64 KiB。这是为了单次写入有界。`0` 只关闭按时间或按总容量的清理，不表示磁盘不会增长；长期保留要自己看磁盘。

收紧策略（更少天数或更小容量）必须在页面上再次确认，因为下一轮清理会删掉超出的正文。放宽不会删数据。已按旧的 7 天策略删掉的内容无法恢复。

用量账本 `usage.jsonl` 与正文是分开的。清除正文不会改账本，清除用量数字也不会删正文。

## 页面上有什么

- 会话列表：按 Agent、项目（仅当请求带了 `X-Magpie-Project`）、日期和标题/ID 筛选。
- 对话：按页往前翻，不截成 128 轮。工具参数和结果在气泡里。
- 单次请求：输入上下文 token、输出、缓存读/写、延迟、状态。费用和上下文窗口只在有元数据时显示，否则标为未知。
- 趋势：按日汇总的是**各次请求计费 token 之和**。另一张表是**每一次的输入上下文**。两者不是同一个数。窗口未知时不画估计占比。
- 搜索与 Markdown / JSONL 导出。导出文件写明这不是逐字节原文。

旧的 `magpie web` 会话接口 `/api/sessions/transcript` 仍只展开最近 128 轮，并带截断标记。要看全部保留中的轮次，用观测页或 `GET /api/turns`。

## 完整性与隐私

保存的是解析后的片段，不是原始 HTTP 报文。

- 图片只留 `[image]`，不留 base64。
- 流没有正常结束、调用被取消、或解析失败时，轮次标成未完成或已取消，已拿到的文本会留下。
- 客户端每次重发的完整历史不会再存一份；只存新增片段，并记下折叠了多少段。若对不上已保存的前缀（压缩、编辑、换了内容），这一轮整段留下，并标 `prefix_reset`，不会猜成一条历史。
- `Authorization`、`Cookie`、`x-api-key` 以及 JSON 里已知的密钥字段不会进入对话文件。提示词、代码和工具输出里的其他隐私**不会**自动擦除。若要自动擦任意文本，就无法同时保证对话完整。
- 观测页默认只监听本机，请求都要带页面密钥。

测试只用合成字符串（例如 `hello-synthetic`），不使用真实对话或密钥。

## 迁移

- 不改写、不删除已有的 `providers.json`、`usage.jsonl` 或旧的 `gateway-conversations/`，除非你在页面上保存 provider、打开清除，或确认收紧保留策略。
- 没有 `gateway-retention.json` 时行为与上游一致：7 天、256 MiB。
- 旧的按日 `turns.jsonl` 和更早的单轮 JSON 仍可读。第一次打开列表时会补一份索引，不改原文。
- 从本版退回上游二进制时，上游会忽略它不认识的 JSON 字段；若你把天数设成了 `0`，上游仍按自己的 7 天常量清理。退回前请先复制 `gateway-conversations/`。

## 已知限制

- 不记录没有会话 ID 的请求，也不记录未经过对话入口的嵌入、计数和模型列表。
- 不把多次独立调用并成一段对话。
- 上下文窗口来自你在 provider 上填的数字，或本机模型目录里的公开窗口，不是对某次请求的实测。两者都会标明来源。
- 费用只使用该 provider 的价格或客户端上报的 `cost`。没有价格就是未知，不是 0。
- 共享记忆、技能分发、社区市场没有从模块里物理删除。`magpie observe` 不启动它们，对应 MCP 路由在这个进程里返回 404。`magpie web` / `magpie serve` 仍是完整产品入口。
- 桌面壳不是本版的运行方式。

## 测试

与本次改动直接相关、在本环境跑过的命令写在 PR 说明里。完整 `go test ./...` 覆盖整个上游产品，体积很大；改动集中在 `internal/sessions`、`internal/gateway` 的记录路径、`internal/observe` 和 `internal/cli`。
