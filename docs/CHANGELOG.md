# 变更记录

记录已完成的改动与其验证证据。**未验证的内容明确标注"待验证"，不得写成"通过"。**

## 2026-11-21

### 默认界面语言改为简体中文

`app.language` 的默认值由 `en` 改为 `zh-CN`：全新安装，或数据目录里还没有 `config.json` 时，桌面从配置引导开始就是中文界面。

- 代码：`internal/domain/config.go` 的 `DefaultConfig()` 设置 `zh-CN`。已显式保存语言选择的配置不受影响（`store.loadConfig` 把 JSON 覆盖到默认值之上，存储值优先）；只有缺少 `app.language` 的旧配置才会继承新默认值。设置页仍可切到 English，切换后立即写回配置。
- 测试：`internal/domain/config_language_test.go` 覆盖"默认中文 / 旧配置无该键继承中文 / 已存 `en` 时保留 `en` / 非法语言被拒"；新增 `cmd/desktop/language_test.go`，用全新临时数据目录断言 `d.language`、`tr()` 与设置页渲染结果都是中文。
- 文档：`docs/DEVELOPMENT.md` 的默认语言改为 `zh-CN`，并说明已保存的选择不会被覆盖。

验证（Windows 11 / Go 1.27.4）：

- `go test -count=1 -timeout 300s ./internal/domain ./internal/store ./cmd/desktop` 通过；`go vet`、`gofmt -l cmd internal` 通过；`go build ./...` 与 Linux/macOS 交叉编译通过。
- `go run github.com/egoist/mygo/cmd/mygo build -platform windows/amd64` 重新产出 `PrintBridge.exe` 与安装器。
- 用不带 exe 资源的开发构建 + 全新 `PRINTBRIDGE_DATA_DIR` 真实启动：首次启动的配置引导为中文（"完成首次打印设置 / 1. 选择打印机 / 2. 设置纸张尺寸 / 完成设置 · 暂时跳过"），标题栏图标同时正常；核对后已销毁该测试实例并删除临时数据目录。

待验证：新安装包在真实新用户环境下的首次启动语言（本次未做全新安装）。已有安装如果配置里已存 `"language": "en"`，升级后仍是英文，需在"设置 → 界面语言"切换一次。

### 应用图标缺失：托盘有图标，应用本身没有

现象：托盘区有图标，但资源管理器里的 exe、任务栏按钮、窗口标题栏都是 Windows 默认空白图标。

根因：`mygo.json` 没有 `icon`，`resources/icon.png` 也不存在，`mygo build` 只写入 `RT_VERSION` 与 `RT_MANIFEST`，exe 内没有 `RT_ICON`/`RT_GROUP_ICON`（REA `inspect_pe_resources` 核对旧 `PrintBridge.exe`：资源目录只有类型 16 与 24，`icon_groups` 为空）。窗口类图标的来源是 `LoadIconW(instance, 1)`，资源不存在时该值为 0，标题栏与任务栏于是回落到系统默认图标。托盘之所以正常，是因为 `cmd/desktop/main.go` 里用代码画了一个 32×32 PNG 传给 `mygo.NewTray`。

修复：

- 新增 `cmd/desktop/assets/icon.png`：1024×1024 方形 PNG，圆角青绿底 + 白色打印机（配色取应用强调色 `#087F78`），用 `go:embed` 嵌入桌面程序。
- `mygo.json` 增加 `"icon": "cmd/desktop/assets/icon.png"`。`mygo build` 由它写入 exe 的七档 `RT_ICON`（16/24/32/48/64/128/256）与 ID 1 的 `RT_GROUP_ICON`，并作为 NSIS 安装器的 `MUI_ICON`/`MUI_UNICON`。
- 新增 `cmd/desktop/appicon.go`：`iconAt(size)` 按面积平均缩放内嵌图标（预乘 alpha，避免边缘发暗）。窗口用 256（Windows 再按标题栏/任务栏 DPI 缩放），托盘用 32（mygo 以 PNG 自身尺寸作为通知区图标尺寸）。窗口创建后调用 `Window.SetIcon`，因此不带 exe 资源的开发构建也有图标。
- 托盘图标改用同一素材（原实现是代码画的蓝色方块），与窗口、exe、安装器一致；`mygo.NewTray` 失败现在会写日志，避免"没有托盘图标却仍然在关闭窗口时隐藏"的静默状态。

验证（Windows 11 / Go 1.27.4）：

- `go run github.com/egoist/mygo/cmd/mygo build -platform windows/amd64` 成功：`PrintBridge.exe`（19.0 MB）、`print-bridge-setup-0.1.0-amd64.exe`（14.1 MB）。
- REA `inspect_pe_resources`：`PrintBridge.exe` 含 `RT_ICON`(3) 七项与 `RT_GROUP_ICON`(14) ID 1，16/24/32/48/64/128/256 各尺寸 `size_matches=true`；安装器同样含完整图标组。
- `shell32.ExtractIconExW` 从 exe 与安装器提取第 0 个图标并渲染，均为新图形。
- 用不带 exe 资源的开发构建真实启动（独立 `PRINTBRIDGE_DATA_DIR`）：窗口标题栏与任务栏按钮均显示新图标（截图核对）；托盘 PNG 经 `CreateIconFromResourceEx` 渲染正确，`Shell_NotifyIconGetRect` 查询托盘图标成功。
- `go test -count=1 -timeout 300s ./cmd/desktop`、`go vet ./cmd/desktop`、`gofmt -l cmd internal`、`go build ./...` 通过；`GOOS=linux GOARCH=amd64` 与 `GOOS=darwin GOARCH=arm64` 交叉编译通过。

待验证：用新安装包覆盖安装后，资源管理器、开始菜单与「应用和功能」中的图标显示。本次只核对 exe/安装器资源与未安装的开发构建，**未重新安装**。已安装的旧副本（例如 `E:\PrintBridge`）需重装或替换 exe 才会换图标。

### 仓库命名空间迁移到 GitHub

模块路径与应用标识从 `gitee.com/wxioi` 改为 `github.com/shiuoei`，本地工作目录同步迁移。

- 代码：`go.mod` 模块路径改为 `github.com/shiuoei/print-bridge`；87 个 `.go` 文件共 172 行内部 import 前缀同步替换（含尚未纳入 Git 的新文件）；`gofmt` 修正了 27 个文件的 import 分组排序。
- 文档：`AGENTS.md`、`docs/DEVELOPMENT.md` 的模块路径与 `GOCACHE` 示例路径改为 `D:\go\github.com\shiuoei\print-bridge`。
- 应用标识：`mygo.json` 的 `identifier` 由 `dev.wxioi.print-bridge` 改为 `dev.shiuoei.print-bridge`。影响面：Windows 安装器卸载注册表键（`HKCU\Software\Microsoft\Windows\CurrentVersion\Uninstall\<identifier>`）、macOS `CFBundleIdentifier`、`OpenAtLogin`/开机自启的应用 ID；**数据目录不变**（`%AppData%\print-bridge`），未迁移任何用户数据。
- 目录：仓库迁至 `D:\go\github.com\shiuoei\print-bridge`。Go 模块模式不要求目录与模块路径一致，迁移前后构建行为不变。

验证（Windows 11 / Go 1.27.4）：

- `go build ./...`、`go vet ./...` 通过。
- `go test -count=1 -timeout 400s ./...` 全部通过（12 个包 `ok`，约 50 s）。
- `go build -o print-bridge.exe ./cmd/print-bridge`、`go build -o desktop.exe ./cmd/desktop` 通过；`go version -m` 显示 `path github.com/shiuoei/print-bridge/cmd/...`。
- `go run github.com/egoist/mygo/cmd/mygo build -platform windows/amd64` 成功：`build/windows-amd64/PrintBridge.exe`（19.0 MB）与 `print-bridge-setup-0.1.0-amd64.exe`（14.1 MB）；`PrintBridge.exe` 内可见 `dev.shiuoei.print-bridge`，旧标识不存在；`SumatraPDF.exe` SHA-256 仍为 `719f689b...`。

**注意**：标识变化后，已安装的旧版本不会被新安装包原地覆盖升级，设置→应用里会留下旧条目；旧版开机自启项按旧 appID 判断，可能残留。升级前应先卸载旧版本。

待办：GitHub 上创建 `shiuoei/print-bridge` 并推送；Gitee 旧地址（匿名已 404）按需归档。安装包覆盖升级与真实打印机回归**未在本次迁移后重跑**，待验证。

### 文档整理

- 仓库文档从 8 份精简为 5 份：`README.md`、`AGENTS.md`、`docs/DEVELOPMENT.md`、`docs/CHANGELOG.md`、`docs/TODO.md`。
- 删除 `testdata/sdk-reference/`（此前 vendored 的第三方 SDK 夹具）与 `TestUpstreamNodeSDK`。仓库不再包含任何第三方 SDK 代码；协议兼容改由 `TestDocumentedJSClient` 与 `protocol_compat_test.go` 保障。
- 代码注释与文档中的历史遗留措辞已全部清除，协议约束统一改用"协议规定"表述；业务系统专有名称亦已去除，改用"兼容写法"描述。

### 纠正错误结论：HTTPS 页面不需要 wss

此前文档记载"HTTPS 页面连 `ws://127.0.0.1` 会被混合内容拦截，上线必须做 `wss://`"。**该结论是错误的**，经用户实测与规范核查纠正。

依据 [W3C Secure Contexts §3.1](https://w3c.github.io/webappsec-secure-contexts/#is-origin-trustworthy)：host 落在 `127.0.0.0/8`、`::1/128`，或为 `localhost`/`*.localhost` 时属于 potentially trustworthy origin，从 HTTPS 页面加载**不构成混合内容**。Chrome 与 Firefox 均已实现。

错误成因：把"HTTPS 不能连 `ws://`"当成无条件规则，忽略了它对 loopback 的豁免。**`wss://` 明确不需要**，不是"未实现"。

顺带明确：白名单里加的是**页面自身的 Origin**，不是 `127.0.0.1`。

### 纸张尺寸维持严格策略

`paper` 的毫米值先匹配驱动声明的纸张名，匹配不到返回 `PAPER_NOT_FOUND`。不含直传毫米值的回退。

实测（HPRT N41）：100×150、100×180、100×100 均能匹配；90×120 不在列表，返回 `PAPER_NOT_FOUND`。

理由：SumatraPDF 收到驱动不认识的 `paper=` 不报错，而是静默回退设备默认纸张，作业状态仍是 `submitted`——标签机上等于"每张都打错却看不出"。宁可明确失败。**该取舍由用户确认保持。**

## 2026-11-20

### 支持兼容协议写法

部分业务前端使用另一套写法，现在两套并存：

| 请求 | 改前 | 改后 |
| --- | --- | --- |
| `{"type":"printers"}` | `INVALID_MESSAGE` | 返回 `{type:"printers", list:[{name,isDefault}]}` |
| `pdf` + `data_base64` | 只收 `file_url` | 接受（与 `file_url` 二选一） |
| `raw` + `copies:1` | 严格拒绝 | 接受并忽略（`paper` 仍拒绝） |
| `scale_mode`/`direction`/`center` | 静默忽略 | 生效 |

改动：`internal/domain/job.go`（新增字段与校验）、`internal/convert/convert.go`（`writeInlinePayload`，大小上限按解码后字节算）、`internal/agent/agent.go`（`printers` 分支、传递缩放选项）、`internal/printer/printer_windows.go`（`PrintPDFWithOptions`）。

**两处关键修正：**

1. 最初打算把 `fit_width`→`fitwidth`。查 SumatraPDF 官方文档确认 **`-print-settings` 只支持 `noscale`/`shrink`/`fit`/`stretch`，`fitwidth`/`fitheight` 是查看器的 `-zoom` 参数**，传进去会被忽略。
2. 第一版把 `fill` 映射成 `stretch`。参照既有实现的行为后改为 `fit`——**`stretch` 按宽高独立缩放会把面单拉变形**。新增 `TestLabelScalingNeverStretches` 防止回归。

验证：新增 18 项测试；真实打印机 HPRT N41 上 `fill` 与 `actual` 两种缩放分别出纸，均到 `submitted`（7.6 s 完成两页）。

**待验证**：`fit_width`/`fit_height` 的实际视觉效果（只验证了令牌正确传递与作业 `submitted`，未逐张核对缩放比例）。

### 修复"测试页已出纸但提示 the operation timed out"

现象：点测试打印，面单确实打印，但界面显示 `the operation timed out` 并停在「正在打印…」。

用 `mygo-automation` 插件脚本化驱动真实 GUI 复现，配 goroutine dump 定位到卡死在 `test_print.go:95` 的 `slog.Info`——**打印已成功、作业已进队列，卡在成功日志上**。

根因是日志死锁。手机写 `slog.SetDefault(slog.New(logging.NewHandler(slog.Default().Handler(), logBuffer)))` 形成闭环：`slog → 自己的 handler → slog defaultHandler → log.Printf → handlerWriter → 自己的 handler`。`log` 输出锁不可重入，第二次进入即死锁，且该 goroutine 永久持锁，之后进程内任何日志调用都卡死。

修复：`logging.NewHandler` 拒绝接受 slog 自己的 `defaultHandler`；新增 `logging.Install(buffer)` 作为唯一接线入口。验证：真实桌面连续 4 次点击，每次 1 秒内恢复并显示成功消息；新增 3 项死锁回归测试（反向验证：去掉防护后立即失败）。

### Windows 安装包

`mygo build -platform windows/amd64` 产出 `PrintBridge.exe`（19.0 MB）与 setup（14.0 MB）。静默安装 7.4 s，安装后经 IPC 连续 3 次 `test.print` 均在 1.1 s 内返回 `submitted`，卸载 5.1 s、目录与注册表无残留。

**安装包未签名**，SmartScreen 会对未知发布者告警。

### 修复 HTML 渲染静默丢图

`proxy-bypass-list=<-loopback>` 使 Chromium 完全丢弃 loopback 子资源，PDF 只有 `ProcSet` 声明而无 `/XObject`，任何从本机提供的 HTML 打印都缺图且报 `submitted` 成功。

修复：移除 `<-loopback>`，改为显式 `localhost,127.0.0.1,::1` 走直连；新增 `loopbackGuard` 用 CDP `Fetch` 域拦截每个请求，loopback 请求只有命中授权 origin 才放行。反向验证：改回旧配置后同源图片测试立即失败并准确报"图片从未被请求"。

## 2026-11-17 及更早

### 功能面补齐

- **历史导出**：`history export <file> [--format csv|json] [--days N] [--status S] [--query Q]`，CSV 带 UTF-8 BOM。导出内容由 command service 生成，CLI 与桌面共用同一路径。
- **远程死信**：`remote dead-letters/{retry,delete,purge}`；重试在远程轮询关闭时明确拒绝；"全部丢弃"是独立命令，不可能通过省略参数触发。
- **桌面 UI**：白名单逐项管理（校验错误显示在字段旁，环回地址不可删除）、分组诊断、纸张下拉、主题切换、关于页、历史表格与详情时间线、配置导入导出对话框、死信面板。
- **CLI**：`task clear`、`remote dead-letters *`、`config validate [<file>]`。

### 真实缺陷修复

- **配置导出丢数据**：`config export --only service-port` 再 `import` 会清空白名单并重置远程参数。根因是 `applyFieldSelection` 从 `DefaultConfig()` 起算后整体覆盖。改为 `transfer.PartialConfig`（全字段指针 + `omitempty`）表达"键存在才覆盖"。
- **白名单可自锁**：`ip delete 127.0.0.1` 后本机 UI/CLI 被自己拒绝，且可写入 `0.0.0.0/0` 对整个 LAN 开放。新增 `Config.Normalize()`：强制注入环回、trim/去重、丢弃任意地址项；`IPAllowed` 增加环回放行旁路。
- **图片转 PDF 拉伸变形**：原实现把图片铺满整页 MediaBox。改为等比 contain + 居中，无纸张时按 203 DPI 换算。
- **`pong.agent_status` 字面量错误**：曾返回 `running`，协议规定是 `ready`。
- **Windows PDF 打印必然失败**：传入了 SumatraPDF 不存在的 `-silent` 参数，`-print-to` 本身即静默打印。
- **纸张设置被静默忽略**：`paper=100x180mm` 等尺寸表达式全部回落到驱动默认纸张。修复见 `internal/printer/paper.go`。

### 协议对齐

`printers_list` 补 `dpi/port/is_local/is_network/is_virtual`；`get_print_queue` 只返回未开始任务、状态恒 `queued`、带 `batch_id`；`raw` 显式携带 `copies` 即拒绝（2026-11-20 为兼容放宽）；非 HTML 格式携带 `wait_ms` 拒绝；`raw-html` 按 trim 后判空。

### 首次发布与验证

建立独立数据目录、配置模型、JSON 原子写入、SQLite WAL、版本迁移、异常退出任务恢复、任务状态历史及远程上报 outbox。加入 Apache-2.0 LICENSE、SumatraPDF 3.6.1 与第三方声明。

真实设备验证：HPRT N41 的 raw（TSPL）、PDF、JPEG 标签均确认实际出纸；HP Laser MFP 的 DOCX、XLSX、PPTX、内嵌 HTML、URL HTML 均已确认出纸。纸张尺寸与份数在 Windows 队列 DEVMODE 层核对。

## 尚未验证

- 跨机器场景、长时间断线重连压力
- `fit_width`/`fit_height` 的实际打印视觉效果
- Office COM 激活卡死、宏文档、LibreOffice 超时
- CUPS 逐格式打印、macOS/Linux 真实设备
- 真实公网复杂页面版面正确性
- DNS 重绑定攻击、浏览器直连绕过代理
- 代码签名、更新器、Linux deb/rpm 打包
