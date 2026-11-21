# PrintBridge 开发说明

本文描述当前代码与目标设计。待办见 [TODO.md](TODO.md)，已验证的证据见 [CHANGELOG.md](CHANGELOG.md)，AI 开发约定见 [AGENTS.md](../AGENTS.md)。后续开发应先读这些文件，避免把"可编译"误认为"可发布"。

## 1. 产品边界

- 独立程序，模块路径 `github.com/shiuoei/print-bridge`，安装标识 `print-bridge`，使用自己的数据目录。不扫描、不复制、不迁移其他程序的数据。
- 应用代码使用 Go；桌面端使用 `github.com/egoist/mygo/ui` 原生控件，不使用 WebView、Vue 或前端构建。
- 保持 `GET /ws`、默认端口 `17890`、snake_case 消息字段和任务状态语义。端口被占用时展示冲突，不结束占用者。
- 桌面与 headless 共享 domain、Agent、CommandService、存储和打印后端；headless 不导入 MyGo。
- 代理负责接收、校验、下载、转换和**提交系统打印队列**；`submitted` 不代表物理出纸完成。

## 2. 代码结构与职责

| 位置 | 职责 |
| --- | --- |
| `cmd/desktop` | MyGo 原生窗口、托盘、单实例、自启动开关；启动进程内 Agent 与本地 IPC |
| `cmd/desktop/assets/icon.png` | 应用图标（1024×1024 方形 PNG）：exe 资源、安装器、窗口与托盘共用，由 `mygo.json` 的 `icon` 与 `go:embed` 引用 |
| `cmd/server` | headless `serve`、信号退出、systemd notify |
| `cmd/print-bridge` | CLI；在线命令走 IPC，离线仅在 Agent 确认不存在时执行；稳定退出码 |
| `internal/domain` | 配置、任务与状态模型、统一 `ProtocolError`、打印机能力结构、白名单与任务字段校验 |
| `internal/agent` | `/ws`、串行队列、任务状态、取消、连接生命周期、远程轮询和上报 |
| `internal/command` | UI/CLI 共用的强类型命令执行入口；只读 `doctor` |
| `internal/ipc` | 4 字节大端长度前缀 + JSON envelope；Windows 命名管道、Unix socket |
| `internal/logging` | slog handler：内存环形缓冲 + 控制台输出 + 敏感信息脱敏 |
| `internal/store` | 独立配置 JSON、SQLite 历史和远程上报 outbox、只读打开、历史清理 |
| `internal/security` | 公网 HTTP(S) 下载、DNS/IP 检查、大小限制 |
| `internal/convert` | PDF 校验、图片转 PDF、Office 转 PDF、Chromium HTML 转 PDF |
| `internal/printer` | 打印机列表与能力查询；Windows Spooler raw 与 SumatraPDF；Unix CUPS `lp` |
| `internal/transfer` | Argon2id + AES-256-GCM 加密配置导入导出 |
| `resources/windows-amd64` | 随 Windows 包发布的 SumatraPDF 3.6.1 |
| `docs/RELEASES.md` | 自动更新窗口里的发布说明，每个 `## <version>` 小节对应一次发布 |
| `packaging/linux` | headless systemd unit；deb/rpm 打包脚本尚未实现 |

依赖方向：入口 → CommandService / Agent → domain、store、security、convert、printer。桌面 UI 不直接处理 WebSocket 消息；浏览器客户端不能通过 `/ws` 修改本机配置。`internal/printer`、`internal/store`、`internal/convert`、`internal/security` 不得反向导入 `internal/agent` 或 `internal/command`。

## 3. 运行流程

1. `store.Open` 创建独立数据目录，加载或创建默认配置，打开 `history.sqlite3`，启用 WAL、迁移 schema（当前版本 3），并将上次异常退出时残留的非终态任务标记为 `failed`。这些任务不会自动重印，因为崩溃前可能已提交系统队列。**恢复只在这个独占启动路径执行**；`store.OpenReadOnly` 用于运行中读取，不迁移、不恢复、拒绝写，因此 CLI 在线查询不会破坏运行中的任务状态。
2. `Agent.Start` 监听 `<service.host>:<service.port>`，仅注册 `GET /ws`，启动串行 worker 和远程 worker。端口冲突返回 `SERVICE_PORT_IN_USE`。
3. WebSocket 握手检查客户端 IP 和 `Origin`。默认仅允许本机 IP，网站 Origin 列表默认空，必须显式添加业务网站。基于 Node `ws` 的客户端不发送 `Origin`，仅回环地址可在无 `Origin` 时连接。
4. `print`/`print_batch` 经模型校验后入队并返回 `job_status: queued`。worker 依次下载或解码、转换、提交系统队列，持久化每次状态并向原连接推送。首次状态推送前会等待 `queued` 确认写出，保证顺序。
5. 远程 worker 对配置的 endpoint 发 GET，按 `job_id` 查询本地历史去重，将任务放入同一个队列；`queued/submitted/failed` 映射为 `accepted/success/failed`，先写入 SQLite outbox，再用 POST 重试。401/403/404 时暂停直到远程配置变化；重试耗尽的事件保留在数据库中并从待重试队列移除。
6. CLI 通过本地 IPC 与运行中的 Agent 交互。桌面 UI 在进程内调用同一个 `CommandService`。命令分为 onlineOnly（`status`、`task cancel`、`test-print`、`test-remote`、`logs`）和 onlinePreferred；只有确认 Agent 不存在才会走离线执行，权限/协议错误不会被误判为"Agent 未运行"。

### 当前配置与数据

- 默认端口 `17890`；默认白名单 IP 包含 `127.0.0.1`、`::1`，Origin 为空。
- `app.language` 接受 `en` 与 `zh-CN`，**默认 `zh-CN`**（简体中文）。桌面"设置"页切换后通过 `command.Service` 立即保存。配置里已显式保存的语言选择不会被默认值覆盖；只有缺少该键的旧配置才会继承新默认值。中英文 JSON 语言包位于 `cmd/desktop/locales/`，编译时嵌入程序。诊断项按稳定 ID 和级别显示本地化说明，未知底层错误保留原文作为技术详情。
- 首次启动且缺少默认打印机、纸张或 Origin 时，桌面显示原生配置引导；通过 `command.Service` 保存打印机、纸张、Origin、端口与 `app.setup_completed`。可跳过，也可从设置页再次打开。已有完整配置不会强制重走引导。
- 默认文件上限 20 MiB、单批 20 个任务、最多 100 份、下载超时 30 秒。
- 独立数据目录默认 `os.UserConfigDir()/print-bridge`；环境变量 `PRINTBRIDGE_DATA_DIR` 可覆盖。headless systemd unit 指定 `/var/lib/print-bridge`。

## 4. 对接协议

线上消息一律 snake_case（`job_id`、`file_url`、`width_mm`）。客户端 SDK 常见的 camelCase 是它们在客户端内部做的转换；直接发原始帧时必须用 snake_case。

### 4.1 请求类型

| `type` | 作用 | 必带 |
| --- | --- | --- |
| `ping` | 心跳 | — |
| `print` | 单任务 | `job_id`、`format` |
| `print_batch` | 批量 | `batch_id`、`jobs[]` |
| `get_printers_list` | 打印机列表 | — |
| `printers` | 打印机列表（兼容写法，见 4.6） | — |
| `get_printer_info` | 单台打印机详情 | `printer_name` |
| `get_print_queue` | 未开始执行的任务 | — |
| `query` | 查某任务最新状态 | `job_id` |

响应类型：`pong`、`printers_list`、`printer_info`、`print_queue`、`job_status`、`error`（`printers` 见 4.6）。

### 4.2 心跳

```json
{"type":"ping","time":1700000000000}
```
```json
{"type":"pong","time":1700000000000,"agent_status":"ready"}
```

`agent_status` 的字面量必须是 **`ready`**（不是 `running`）。**`pong` 没有 `request_id`**，客户端须按回显的 `time` 配对。

### 4.3 任务字段

`request_id`、`job_id`、`format`、`printer_name`、`file_url`、`data_base64`、`html`、`wait_ms`、`copies`、`paper{width_mm,height_mm}`、`scale_mode`、`direction`、`center`。

| `format` | 载荷 | 说明 |
| --- | --- | --- |
| `pdf` | `file_url` **或** `data_base64` | 也接受 `data:application/pdf;base64,...` 形式的 URL |
| `image` | `file_url` **或** `data_base64` | PNG/JPEG，按内容识别 |
| `docx` / `xlsx` / `pptx` | `file_url` | 只接受 HTTP(S) URL，转换后打印 |
| `html` | `file_url` | 下载并渲染 |
| `raw-html` | `html` | 直接渲染字符串 |
| `raw` | `data_base64` | 已生成的设备指令，原样提交 |

`file_url` 与 `data_base64` **二选一**：`pdf`/`image` 同时给出会被拒绝（不能替调用方猜）。`raw` 不接受 `paper`；`copies` 接受但忽略（部分业务系统无条件携带）。

`scale_mode`/`direction`/`center` 只影响 PDF 渲染链路（`pdf`、`image`、Office、HTML 都会先转 PDF）；`raw` 原样提交，这些字段不起作用。`center` 是 **JSON 布尔值**，省略即居中（默认 `true`）。注意 Windows 后端（SumatraPDF）**没有独立的居中设置**，居中只能靠缩放令牌表达：

| `scale_mode` | `center: true`（默认） | `center: false` |
| --- | --- | --- |
| `actual` | `shrink`：按需缩小到纸张内并居中，绝不放大 | `noscale`：1:1 打印，从物理纸张左上角开始 |
| `fill` / `fit_width` / `fit_height` / 未指定 | `fit`：整页缩放装入纸张并居中 | 仍是 `fit`，SumatraPDF 无法关闭居中 |

居中/缩放只移动"页面"，不会移动 PDF 页面内部的内容：页面尺寸等于纸张时，`actual` 两种取值的落点差异仅来自打印区域边距；如果 PDF 内容本身在页面里偏左上，任何 `-print-settings` 参数都无法把它挪到中间。需要内容级居中时，应由业务方生成页面尺寸等于标签、内容居中的 PDF（或改发 `image`，图片路径会按纸张 contain + 居中）。Linux/CUPS 后端目前忽略这三个字段。

### 4.4 状态

`queued`、`downloading`、`printing`、`submitted`、`completed`、`failed`、`unknown`、`cancelled`。

当前实现主要产生 `queued/downloading/printing/submitted/failed/cancelled`；系统队列完成追踪尚未实现，因此终态通常停在 `submitted`。单任务和批量子任务各自持有可取消 context，活动下载/转换会收到取消信号；已开始提交打印/已提交的任务返回 `JOB_NOT_CANCELLABLE`。

错误帧含 `error_code`、`request_id`、`message`；**失败任务的 `job_status` 帧同样含 `error_code`** 并入库，客户端无需解析消息文本。

### 4.5 打印机能力

由 `printer.Backend.Info` 返回系统真实能力：Windows 使用 `EnumPrintersW`(level 2) + `DeviceCapabilitiesW`，Unix 使用 `lpstat -v` 与 `lpoptions -p <name> -l`。`papers` 永远返回数组（可为空），`trays`/`media_types` 在驱动不提供时为空。未知字段留空而不是猜测，避免 UI 谎报设备能力。

`paper` 的毫米值**不会原样传给 SumatraPDF**，而是先匹配驱动声明的纸张名，匹配不到返回 `PAPER_NOT_FOUND`。原因见 5.2。

### 4.6 兼容写法

部分业务前端使用另一套写法。两套并存，都能用：

| 差异 | 兼容写法 | 标准写法 |
| --- | --- | --- |
| 打印机列表 | `{"type":"printers"}` → `{type:"printers", list:[{name,isDefault}]}` | `get_printers_list` → `printers_list` + `is_default` |
| PDF 内容 | `data_base64` | `file_url` |
| `raw` 的 `copies` | 无条件带 `copies:1` | 会报错 |
| 缩放 | `scale_mode`/`direction`/`center` | 无 |

`scale_mode` 映射到 SumatraPDF 的 `-print-settings`（只有三种缩放令牌）：`actual` → `center` 为真时 `shrink`、为假时 `noscale`；`fill` → `fit`；`fit_width`/`fit_height` → `fit`；未指定 → `fit`。

> `fill` **不能**映射为 `stretch`——SumatraPDF 3.6.1 根本不接受 `stretch`（未知令牌被静默忽略），而且按宽高独立缩放会把面单拉变形。`fit_width`/`fit_height` 也不是单方向拟合，SumatraPDF 做不到，实际退化为"完整缩放到纸张内"。`direction` 覆盖按纸张比例推断的方向（`landscape` 是内容旋转 90°，纸张方向不变）。`center` 默认 `true`，但它不是 SumatraPDF 的参数，而是决定 `actual` 用 `shrink`（居中、不放大）还是 `noscale`（左上角 1:1）；其余模式本身就居中。

## 5. 打印、转换与安全

| 格式 | 路径 | 状态 |
| --- | --- | --- |
| raw | Windows `OpenPrinterW`/`WritePrinter`；Unix `lp -o raw` | 已实测 HPRT N41 提交 TSPL 指令（234 ms）；字节一致性由驱动决定 |
| PDF | 公网下载或 data URL，验证 `%PDF-`；Windows SumatraPDF、Unix CUPS | 已实测出纸；纸张与份数在 spooler 层验证 |
| image | Go 解码 PNG/JPEG，生成单页 PDF | JPEG 已实测出纸；PNG、透明背景、大图内存限制待验 |
| Office | OOXML ZIP 校验；Windows 尝试 Office/WPS COM，未安装时回退 LibreOffice；Unix LibreOffice | DOCX/XLSX/PPTX 已实测转换；COM 激活卡死、宏/外链待验 |
| HTML | Chromium CDP 导航 + `PrintToPDF`；本地代理限制目标 IP | 真实 Chrome 完成 7 类子资源攻击面验证并配负对照；DNS 重绑定仍属残余风险 |

### 5.1 打印参数要点

Windows PDF 走 SumatraPDF 的 `-print-to` + `-print-settings`。该工具**没有** `-silent` 参数（`-print-to` 本身即静默），传入会以 `ParseFlags` 中止。3.6.1 的缩放令牌只有 `noscale`、`shrink`、`fit`，**没有** `stretch`，也**没有** `center`；未知令牌会被静默忽略（源码 `ApplyPrintSettings` 没有错误分支），所以把"居中"当成令牌传给它是无效的。`fitwidth`/`fitheight` 是查看器的 `-zoom` 参数，不是打印参数。三者对落点的作用：`noscale` 从物理纸张左上角 1:1 打印（SumatraPDF 有意如此，便于表单/标签对位）；`shrink`/`fit` 把页面缩放到打印区域并居中，且 `shrink` 不会放大。

### 5.2 纸张必须匹配驱动声明

SumatraPDF 的 `paper=` 只接受**打印机驱动自己的纸张名**，不接受尺寸表达式。实测：`paper=100x180mm`、`paper=Custom.100x180mm`、`paper=A4` 全部回落到驱动默认纸张，且**不报错**——业务指定面单尺寸时会打错尺寸而无任何提示。

因此 `internal/printer/paper.go` 把请求尺寸在驱动 `Info().Papers` 中按 1 mm 容差、方向无关地解析为驱动纸张名；解析不到返回 `PAPER_NOT_FOUND`。这是有意选择：宁可明确失败，也不接受"打出来是错的不自知"。

### 5.3 下载与 HTML 安全

下载客户端禁用系统代理，对每次连接解析 DNS 并拒绝本机、私网、链路本地、组播等地址；重定向再校验。**当前 dial-time 检查是本机唯一的地址兜底**——`ValidateURL` 无法识别十进制/十六进制/短写等 IP 形式（`net.ParseIP` 不解析），因此不要移除或绕过 `security.Client` 的 `DialContext` 检查。

HTML 的 HTTP(S) 资源走本地代理，与下载器共用地址校验，并对每个页面实施 512 个子资源 / 64 MiB 总量 / 单资源 20 MiB / 单次 20 s 上限；超限时渲染明确失败而不是静默产出残缺 PDF。

Loopback 请求由 CDP `Fetch` 域逐条拦截，只有命中授权 origin 才放行。授权 origin 也计入页面资源预算。

### 5.4 日志装配

只能用 `logging.Install(buffer)`。手写 `slog.SetDefault(slog.New(logging.NewHandler(slog.Default().Handler(), buf)))` 会形成 `slog → 自己的 handler → slog defaultHandler → log → 自己的 handler` 的闭环；`log` 的输出锁不可重入，第二次进入即死锁并永久持锁，进程内之后任何日志调用都会卡死。详见 `AGENTS.md`。

### 5.5 第三方资源

Windows 随包 SumatraPDF 的 SHA-256 应为 `719f689b34f47be8ca105ce8484948474dafde0e106bab599e4a89326070c3d0`，本地下载后及构建产物中均已核对。位置与许可证见 `THIRD_PARTY_NOTICES.md`。

## 6. 构建、运行和测试

```powershell
# Windows 开发环境
$env:GOCACHE='D:\go\github.com\shiuoei\print-bridge\.cache\go-build'
$env:CGO_ENABLED='0'

go build ./...
go test ./...
go vet ./...
gofmt -l internal cmd

# 桌面 / headless / CLI
go run ./cmd/desktop
go run ./cmd/server serve
go run ./cmd/print-bridge status
```

### 打包

```powershell
# 仅编译，不产安装包
go run github.com/egoist/mygo/cmd/mygo build -skip-installer -platform windows/amd64

# 含 NSIS 安装包（首次会下载 NSIS）
go run github.com/egoist/mygo/cmd/mygo build -platform windows/amd64
```

产物在 `build/windows-amd64/`：`PrintBridge.exe`、`SumatraPDF.exe`、`print-bridge-setup-<version>-amd64.exe`。

图标只有一个源文件：`cmd/desktop/assets/icon.png`，由 `mygo.json` 的 `icon` 指给构建工具。`mygo build` 把它写成 exe 的 `RT_ICON`/`RT_GROUP_ICON` 资源（16/24/32/48/64/128/256）与 NSIS 安装器的 `MUI_ICON`/`MUI_UNICON`；窗口与托盘在同一份 PNG 上运行时缩放（`cmd/desktop/appicon.go`），所以 `go run ./cmd/desktop` 这类没有 exe 资源的构建也有图标。换图标只需替换该 PNG（正方形、建议 1024×1024），重新构建即可。

### 发布与自动更新

桌面版使用 MyGo 的签名自动更新：`mygo.json` 的 `updates` 声明更新源（本仓库为 GitHub Releases）与公钥；`mygo build` 用私钥生成签名归档、清单和增量，应用内由 `plugins/updater/native` 提供原生更新窗口（无 WebView），启动 10 秒后自动检查、之后每天一次，托盘与"关于"页可手动检查。更新窗口的语言取应用**启动时**的界面语言，切换语言后重启生效。更新包与清单必须签名，客户端用内置公钥验签后才安装。

**更新签名密钥**（Ed25519，一次生成，长期复用）：

```powershell
go run github.com/egoist/mygo/cmd/mygo keygen   # 默认写到 %AppData%\mygo\update-keys
```

- 公钥填进 `mygo.json` 的 `updates.publicKey`（可提交）。
- 私钥 `mygo-update.key` **必须放在仓库外**（密码管理器 + CI secret）。构建发布包时通过 `MYGO_UPDATER_PRIVATE_KEY` 传入。私钥丢失后，已安装版本将无法再自动更新，只能手动重装；私钥泄露等于任何人都能向用户下发代码。

**发布一次新版本**（全在 GitHub 上完成，本地不需要安装 `gh`）：

1. 提升 `mygo.json` 的 `version`（语义化版本，例如 `0.2.1`），在 `docs/RELEASES.md` 增加 `## <version>` 小节（更新窗口里的发布说明；缺这一节带私钥构建会直接报错；历史证据仍写 `docs/CHANGELOG.md`），提交并推送 `main`。
2. 打 tag 并推送，触发 [`.github/workflows/release.yml`](../.github/workflows/release.yml)：

```powershell
git tag v0.2.1
git push origin v0.2.1
```

3. 工作流在 GitHub 的 `ubuntu-latest` runner 上：校验 tag 与 `mygo.json` 的 `version` 一致 → 安装 NSIS → `mygo build -upload -platform windows/amd64`（用仓库 Secret `MYGO_UPDATER_PRIVATE_KEY` 里的私钥签名）→ 创建草稿 release 并上传归档、清单、`install.ps1`、`SHA256SUMS`、安装包 → 上传成功后自动 `gh release edit --draft=false` 发布为 latest。构建失败时 release 停在草稿，客户端看不到。
4. 到 Actions 页面确认本次 run 通过；也可以在 Releases 页面看资产清单。

首次使用前需要在 GitHub 仓库做一次性配置：**Settings → Secrets and variables → Actions → New repository secret**，名称 `MYGO_UPDATER_PRIVATE_KEY`，值为仓库外 `%AppData%\mygo\update-keys\mygo-update.key` 的完整内容（一行 base64）。只有这一个 Secret；`GITHUB_TOKEN` 由工作流自动提供（工作流已声明 `permissions: contents: write`）。若 Actions 因权限报 403，在 Settings → Actions → General → Workflow permissions 选择 "Read and write permissions"。

本地构建仍可用于校验产物（需要私钥环境变量）：

```powershell
$env:MYGO_UPDATER_PRIVATE_KEY = (Get-Content "$env:APPDATA\mygo\update-keys\mygo-update.key" -Raw)
go run github.com/egoist/mygo/cmd/mygo build -platform windows/amd64
```

产物在 `build/windows-amd64/`。注意本机网络访问 `github.com` 时通时断，本地构建取不到旧 release 时不会生成增量（会提示 "no version is published yet"），GitHub runner 上没有这个问题。

失败处理：若工作流在**发布前**失败，可删掉远端 tag 修好后再打（`git push origin :refs/tags/v0.2.1`）；**已发布**的版本不要改 tag 或替换资产（客户端清单 URL 与签名绑定在该 tag 上），发新版本覆盖。想保留人工把关，可把工作流最后的 "Publish the release" 步骤去掉，改为在 Releases 页面手动 Publish。

**边界与限制**：

- 自动更新要求安装目录对当前用户可写：默认 per-user 安装（`%LOCALAPPDATA%\Programs\PrintBridge`）可以；装到 `Program Files` 时 `mygo.Updater.Enabled()` 为 false，更新窗口会说明。便携式手动目录（如 `E:\PrintBridge`）只要用户可写也可用。
- 0.1.0 没有更新能力：自动更新从 0.2.0 起步，`0.1.0 → 0.2.0` 必须手动安装一次。
- exe 尚未做 Authenticode 代码签名，SmartScreen 仍会提示未知发布者；更新包的 Ed25519 签名只保证完整性与来源，不改变 SmartScreen 行为。
- 更新会在用户确认后下载，重启才生效；正在打印时不要确认重启，避免打断队列任务。
- "关于"页与 doctor 的版本号来自 `mygo.App.Version()`（由 `mygo build` 写入），CLI 仍用 `PRINTBRIDGE_VERSION` 环境变量。

打包**默认不含** automation 插件（`-debug` 才带），因此不能用 UI 脚本驱动打包产物；验证请走 IPC。

### 测试注意事项

- **跑全量测试前先退出 PrintBridge。** `cmd/print-bridge` 的部分测试依赖 IPC，而 Windows 命名管道名是全局的、与数据目录无关——本机有 Agent 在运行时测试会连到真实运行的 Agent 并失败。
- `go test ./internal/agent` 中的 `TestDocumentedJSClient` 会用 `docs/DEVELOPMENT.md` 第 7 节的客户端对真实 Agent 执行，需要 Node.js。**改动协议字段时它会失败**，这是设计如此。
- 真实设备测试按需启用，见下表。

### 环境变量

| 变量 | 用途 |
| --- | --- |
| `PRINTBRIDGE_DATA_DIR` | 覆盖数据目录 |
| `PRINTBRIDGE_DEBUG` | 桌面调试日志写入数据目录 |
| `PRINTBRIDGE_REAL_PRINTER` | 真实打印机能力与纸张解析测试 |
| `PRINTBRIDGE_REAL_CHROMIUM` | 真实 Chromium HTML 安全测试 |
| `PRINTBRIDGE_REAL_INLINE` | 真实打印机验证 inline base64 PDF |
| `PRINTBRIDGE_MATRIX_*_URL` | 扩展格式矩阵测试的素材 URL |
| `PRINTBRIDGE_VERSION` | 覆盖桌面/CLI 构建的版本号（`go build` 没有 `mygo build` 的版本戳时使用） |
| `MYGO_UPDATER_PRIVATE_KEY` | 更新签名私钥内容：本地发布构建时临时设置，正式发布由 GitHub Actions Secret 提供，不写入仓库 |

### 跨平台

```powershell
GOOS=linux   GOARCH=amd64 go build ./...
GOOS=darwin  GOARCH=arm64 go build ./...
```

## 7. 业务方对接参考实现

约 200 行的原生 WebSocket 客户端，无任何依赖。`internal/agent/js_documented_client_test.go` 会**真实执行这段代码**对 Agent 做断言，文档与实现分叉时测试失败。`print`/`printBatch` 会透传页面适配字段 `scale_mode`/`direction`/`center`（见 4.3；`center` 省略即居中）。

```js
class PrintBridge {
  constructor({ port = 17890, host = '127.0.0.1', requestTimeoutMs = 30000 } = {}) {
    Object.assign(this, { port, host, requestTimeoutMs });
    this.pending = new Map();   // request_id -> {resolve, reject, timer}
    this.pings = new Map();     // time -> {resolve, reject, timer}
    this.listeners = new Map(); // job_id -> Set<fn>
    this.ws = null;
  }

  connect() {
    return new Promise((resolve, reject) => {
      const ws = new WebSocket(`ws://${this.host}:${this.port}/ws`);
      const timer = setTimeout(() => { ws.close(); reject(new Error('CONNECTION_TIMEOUT')); }, 5000);
      ws.onopen = () => { clearTimeout(timer); this.ws = ws; resolve(); };
      ws.onerror = () => { clearTimeout(timer); reject(new Error('CONNECTION_FAILED')); };
      ws.onclose = () => {
        this.ws = null;
        for (const [, p] of this.pending) { clearTimeout(p.timer); p.reject(new Error('NOT_CONNECTED')); }
        for (const [, p] of this.pings)   { clearTimeout(p.timer); p.reject(new Error('NOT_CONNECTED')); }
        this.pending.clear();
        this.pings.clear();
      };
      ws.onmessage = (ev) => this._onMessage(JSON.parse(ev.data));
      this.ws = ws;
    });
  }

  _onMessage(msg) {
    if (msg.type === 'pong') {
      const p = this.pings.get(msg.time);
      if (p) { clearTimeout(p.timer); this.pings.delete(msg.time); p.resolve(msg); }
      return;
    }
    if (msg.type === 'job_status') {
      const sub = this.listeners.get(msg.job_id);
      if (sub) for (const fn of sub) fn(msg);
    }
    const p = this.pending.get(msg.request_id);
    if (!p) return;
    clearTimeout(p.timer);
    this.pending.delete(msg.request_id);
    if (msg.type === 'error') {
      const err = new Error(msg.message);
      err.code = msg.error_code;
      p.reject(err);
    } else {
      p.resolve(msg);
    }
  }

  _request(payload) {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) {
      return Promise.reject(Object.assign(new Error('NOT_CONNECTED'), { code: 'NOT_CONNECTED' }));
    }
    const requestId = (crypto.randomUUID?.() ?? String(Date.now()) + Math.random());
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(requestId);
        reject(Object.assign(new Error('REQUEST_TIMEOUT'), { code: 'REQUEST_TIMEOUT' }));
      }, this.requestTimeoutMs);
      this.pending.set(requestId, { resolve, reject, timer });
      this.ws.send(JSON.stringify({ ...payload, request_id: requestId }));
    });
  }

  /** 心跳：pong 不带 request_id，必须按 time 配对。 */
  ping(timeoutMs = 5000) {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) {
      return Promise.reject(Object.assign(new Error('NOT_CONNECTED'), { code: 'NOT_CONNECTED' }));
    }
    const time = Date.now();
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pings.delete(time);
        reject(Object.assign(new Error('PING_TIMEOUT'), { code: 'PING_TIMEOUT' }));
      }, timeoutMs);
      this.pings.set(time, { resolve, reject, timer });
      this.ws.send(JSON.stringify({ type: 'ping', time }));
    });
  }
  async printers()       { return (await this._request({ type: 'get_printers_list' })).printers; }
  async printerInfo(name){ return (await this._request({ type: 'get_printer_info', printer_name: name })).printer; }
  async queue()          { return (await this._request({ type: 'get_print_queue' })).jobs; }
  async query(jobId)     { return this._request({ type: 'query', job_id: jobId }); }

  async print(job) {
    const { jobId, type, printerName, fileUrl, dataBase64, html, waitMs, copies, paper,
            scaleMode, direction, center } = job;
    const payload = { type: 'print', job_id: jobId, format: type };
    if (printerName !== undefined) payload.printer_name = printerName;
    if (fileUrl !== undefined) payload.file_url = fileUrl;
    if (dataBase64 !== undefined) payload.data_base64 = dataBase64;
    if (html !== undefined) payload.html = html;
    if (waitMs !== undefined) payload.wait_ms = waitMs;
    if (copies !== undefined) payload.copies = copies;
    if (paper !== undefined) payload.paper = { width_mm: paper.widthMm, height_mm: paper.heightMm };
    // 页面适配：center 省略即居中（默认 true，Windows 下用 shrink 实现），传 false 才关闭。
    if (scaleMode !== undefined) payload.scale_mode = scaleMode;
    if (direction !== undefined) payload.direction = direction;
    if (center !== undefined) payload.center = center;
    return this._request(payload);
  }

  async printBatch(batchId, jobs) {
    return this._request({
      type: 'print_batch',
      batch_id: batchId,
      jobs: jobs.map(j => {
        const item = { job_id: j.jobId, format: j.type };
        if (j.printerName !== undefined) item.printer_name = j.printerName;
        if (j.fileUrl !== undefined) item.file_url = j.fileUrl;
        if (j.dataBase64 !== undefined) item.data_base64 = j.dataBase64;
        if (j.html !== undefined) item.html = j.html;
        if (j.copies !== undefined) item.copies = j.copies;
        if (j.paper !== undefined) item.paper = { width_mm: j.paper.widthMm, height_mm: j.paper.heightMm };
        if (j.scaleMode !== undefined) item.scale_mode = j.scaleMode;
        if (j.direction !== undefined) item.direction = j.direction;
        if (j.center !== undefined) item.center = j.center;
        return item;
      }),
    });
  }

  /** 订阅某任务的状态推送，返回取消函数。 */
  onStatus(jobId, fn) {
    if (!this.listeners.has(jobId)) this.listeners.set(jobId, new Set());
    this.listeners.get(jobId).add(fn);
    return () => this.listeners.get(jobId)?.delete(fn);
  }

  /** 等待任务到达终态。
   *
   *  状态推送是一次性的：任务早于你注册订阅就结束时，事件已经发过，
   *  之后再也收不到。因此这里先注册订阅、再用 query 补查一次当前状态，
   *  两者竞速——打印很快时（几百毫秒）全靠这个补查，否则会白等到超时。 */
  async waitTerminal(jobId, timeoutMs = 90000) {
    const terminal = new Set(['completed', 'unknown', 'failed', 'cancelled', 'submitted']);
    let off = () => {};
    const viaEvent = new Promise((resolve) => {
      off = this.onStatus(jobId, (msg) => {
        if (terminal.has(msg.status)) resolve(msg);
      });
    });
    try {
      const current = await this.query(jobId).catch(() => null);
      if (current && terminal.has(current.status)) return current;
      return await Promise.race([
        viaEvent,
        new Promise((_, reject) =>
          setTimeout(() => reject(Object.assign(new Error('TERMINAL_TIMEOUT'), { code: 'TERMINAL_TIMEOUT' })), timeoutMs)),
      ]);
    } finally {
      off();
    }
  }

  close() { this.ws?.close(); }
}
```

用法：

```js
const pb = new PrintBridge();
await pb.connect();
const accepted = await pb.print({ jobId: crypto.randomUUID(), type: 'pdf', fileUrl: 'https://example.com/a.pdf' });
const final = await pb.waitTerminal(accepted.job_id);   // "submitted" — 已进队列，不代表已出纸
```

### 7.1 两个常见错误

**状态推送是一次性的。** `job_status` 只在状态变化那一刻推一次。任务若在注册监听前就结束（打印快时仅几百毫秒），那条帧已发过。上面的 `waitTerminal` 用"先订阅、再 `query` 补查"解决。**只订阅不补查是自研客户端最常见的 bug。**

**`submitted` 通常就是终点。** Windows 后端拿不到系统队列回执，流程停在 `submitted`。若后端支持队列追踪，可把 `submitted` 从 `terminal` 集合移除，继续等 `completed`——但必须后端真能查，否则只会等到超时。

### 7.2 浏览器注意事项

- **HTTPS 页面连 `ws://127.0.0.1` 是允许的，不需要 `wss://`。** 依据 [W3C Secure Contexts §3.1](https://w3c.github.io/webappsec-secure-contexts/#is-origin-trustworthy)：`127.0.0.0/8`、`::1/128`、`localhost`/`*.localhost` 属于 potentially trustworthy origin，不构成混合内容。Chrome 与 Firefox 均已实现。
- **白名单加的是页面自身的 Origin**（如 `https://www.example.com`），不是 `127.0.0.1`。浏览器握手时发的是页面来源，与连接目标无关。
- 页面里的 JS 只能连本机，无法跨机器打印；跨机器请用远程轮询或把 Agent 装在目标机器。
- 不要把 `17890` 暴露到公网。

## 8. 后续实现顺序与发布门槛

1. **协议与远程收尾：** 系统队列 `completed`/`unknown` 追踪、远程批量整体幂等、`test-remote` 在真实业务 endpoint 验收、死信人工处理、持久日志轮转和诊断包。
2. **真实环境验收：** 办公室转换的 COM 激活卡死与宏安全、CUPS 逐格式、各驱动纸张矩阵、浏览器真实业务页面。
3. **产品化：** 代码签名（当前未签名，SmartScreen 会告警）、更新器与签名校验、Linux deb/rpm、CI。

发布门槛：未通过真实 Chromium 子资源绕过、Office/WPS 进程归属、真实打印机三类测试的能力，不得标为生产可用。
