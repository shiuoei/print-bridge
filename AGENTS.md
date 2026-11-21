# PrintBridge：AI 开发交接说明

本文件适用于整个仓库。开始任何实现前，依次阅读 `docs/DEVELOPMENT.md`、`docs/TODO.md`；修改核心 Agent 时再读代码。`docs/CHANGELOG.md` 记录已验证的证据和当前缺口。代码是当前行为的最终依据；文档与代码不一致时，先核对代码与运行证据，再修正文档。

## 不可改变的产品约定

- 独立安装，标识 `print-bridge`，数据写入自己的目录；不读取、复制或迁移任何其他程序的配置与历史。
- 应用代码统一使用 Go。桌面 UI 必须使用 MyGo `ui` 原生控件；不引入 WebView、Vue 或前端打包。
- 保持 `GET /ws`、默认端口 `17890`、协议规定的 snake_case 请求/响应、状态与错误语义。端口冲突只报告，不终止占用端口的进程。
- `submitted` 只表示提交系统打印队列。不能在无法确认物理结果时宣称 `completed`。
- 桌面 UI 在进程内调用 `command.Service`；CLI 通过本地 IPC 管理运行中的 Agent。Linux headless 不导入 MyGo。
- 未通过真实 Chromium 的 HTML 子资源绕过测试、Office/WPS 进程归属测试和真实打印机测试前，不把相应路径标为生产可用。

## 从哪里修改

| 任务 | 首先查看 |
| --- | --- |
| 协议、队列、远程轮询 | `internal/agent/agent.go`、`internal/agent/remote.go`、`internal/domain/job.go` |
| 配置、历史、异常恢复 | `internal/domain/config.go`、`internal/store/store.go` |
| 下载与 HTML 安全 | `internal/security/download.go`、`internal/convert/html.go` |
| PDF、图片、Office 转换 | `internal/convert/` |
| Windows、CUPS 打印 | `internal/printer/` |
| 管理命令及本地传输 | `internal/command/`、`internal/ipc/`、`cmd/print-bridge/` |
| 日志装配 | `internal/logging/logging.go` |
| MyGo 原生页面 | `cmd/desktop/main.go`、`mygo.json` |
| Linux 服务与发布 | `cmd/server/`、`packaging/linux/` |

## 推荐的开发闭环

1. 从 `docs/TODO.md` 或 `docs/DEVELOPMENT.md` 中选一项，确认现状、完成条件及平台依赖。先检查相关源码，避免把初版代码当成完整能力。
2. 协议改动对照 `docs/DEVELOPMENT.md` 的「对接协议」一节；不能仅根据字段名猜测错误码或状态。
3. 跨层改动遵守依赖方向：入口 → `command.Service`/Agent → domain、store、security、convert、printer。不要在 UI 中直接实现协议或打印逻辑。
4. 涉及网络/文件/进程的改动明确大小、超时、取消和清理；涉及 Office 时不得关闭用户已有办公软件进程。涉及任务重启恢复时避免自动重复打印。
5. 需要验证时优先运行相关包的有意义检查；跨平台代码至少确认目标系统可编译。真实设备、浏览器、办公软件或安装器未运行时，明确写"待验证"，不得写"通过"。
6. 完成后同步更新 `docs/CHANGELOG.md` 与 `docs/TODO.md`。若改变架构、命令或操作步骤，也更新 `docs/DEVELOPMENT.md` 与 `README.md`。

## 日志接法只能用 `logging.Install`

`slog.SetDefault` 会把标准库 `log` 的输出接到新 logger 上，而 slog 自己的 `defaultHandler` 又是通过 `log` 输出的。把 `slog.Default().Handler()` 包进自己的 handler 再 `SetDefault` 会形成 `slog → 自己 → slog defaultHandler → log → 自己` 的闭环。`log` 的输出锁不可重入，第二次进入即死锁，且该 goroutine 永久持锁，进程内之后任何一处日志调用都会一起卡死。

这个缺陷曾让测试打印在作业已进队列、纸已出之后卡死在成功日志上。`internal/logging.NewHandler` 现已拒绝 slog 自己的 `defaultHandler` 作为下游，但新增入口时仍应使用 `logging.Install`，并在改动日志装配后跑 `internal/logging` 的死锁回归测试。

## 本机开发命令

```powershell
$env:GOCACHE='D:\go\github.com\shiuoei\print-bridge\.cache\go-build'
$env:CGO_ENABLED='0'
go test ./...
go run ./cmd/desktop
go run ./cmd/server serve
go run ./cmd/print-bridge status
go run github.com/egoist/mygo/cmd/mygo build -skip-installer -platform windows/amd64
```

Windows 构建资源中的 SumatraPDF 哈希和许可证见 `THIRD_PARTY_NOTICES.md`。

**测试注意**：`cmd/print-bridge` 的部分测试依赖 IPC，而 Windows 命名管道名是全局的、与数据目录无关。**本机有 Agent 正在运行时这些测试会失败**（它们连到了真实运行的 Agent）。跑全量测试前先退出 PrintBridge。

## 交接记录格式

在 `docs/CHANGELOG.md` 记录：改动文件、验证命令与结果、使用的操作系统/软件/打印机、仍未完成的场景、下一步。遇到外部依赖缺失时继续完成可独立完成的代码和文档，并把真实设备或环境验证明确列为待办。
