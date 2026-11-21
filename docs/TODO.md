# 待办任务

已完成改动与验证证据见 [CHANGELOG.md](CHANGELOG.md)，架构与协议见 [DEVELOPMENT.md](DEVELOPMENT.md)。

更新：2026-11-21。功能面已全部实现，以下全部属于**产品化交付**与**真实环境验收**两类：前者可以继续写代码，后者必须有设备或凭据才能验收。

---

## 一、可继续实现（不依赖外部设备/凭据）

### 1.1 两个缺失的 CLI 子命令

| 项 | 现状 | 需要做的事 |
| --- | --- | --- |
| `autostart [enable\|disable]` | 缺失，`exit 2`；桌面托盘能做，CLI 不能 | 新增子命令（无参数时输出 JSON `{"enabled":bool}`）；写 `app.autostart` |
| `app language <zh-CN\|en>` | 缺失，`exit 2`；桌面设置页能做，CLI 不能 | 新增子命令，非法值报 `InvalidInput`；写 `app.language` |

两者都需要**产品能力适配层**：桌面实现真实能力，headless 返回 `Unsupported` 错误。Go 侧目前没有这层抽象，需先加一个小接口（`ProductCapabilities`），由 `cmd/print-bridge` 根据是否桌面构建决定注入哪个实现。

### 1.2 持久日志与诊断包（D04）

| 项 | 现状 | 需要做的事 |
| --- | --- | --- |
| 日志文件持久化与轮转 | **仅内存 500 条环形缓冲**，重启清空 | 落盘 + 按大小/日期轮转 + 保留份数；脱敏沿用 `logging.Redact` |
| 诊断包导出 | **未实现**；桌面"复制诊断信息"只复制一段文本 | 打包为 zip：配置（脱敏）、日志、doctor 报告、环境信息；UI 与 CLI 双入口 |

诊断包不得包含 bearer token、密码、完整 endpoint 路径。

### 1.3 发布工程（D01/D03/D05）

| 项 | 现状 | 阻塞原因 |
| --- | --- | --- |
| 更新器 | **完全没有**：`mygo.json` 无 `updates` 段 | 需要决定更新源与**签名私钥** |
| 代码签名 / 公证 | 无；安装包未签名，SmartScreen 会告警 | 需要**代码签名证书**；macOS 需开发者账号 |
| CI 门禁 | **无 `.github/` 目录** | 可直接写，无外部依赖 |
| GitHub 仓库与远端 | 本地仓库**无 remote**；`github.com/shiuoei/print-bridge` 尚未创建（匿名 404） | 在 GitHub 建仓并 push；决定是否保留 Gitee 镜像或归档旧地址 |
| Linux deb/rpm | `packaging/` 只有 1 个 `.service` 文件 | 可直接写；验收需 Linux 环境 |
| 依赖预检（`serve` 启动前） | 未实现；缺 `lp/lpstat/lpoptions/soffice/chrome` 时只在任务到达才报错 | 可直接写 |
| systemd unit 补全 | 现有 unit 已含 `Type=notify`、`StateDirectory`、`ProtectSystem=strict`、`ProtectHome=true`、`ReadWritePaths`。**缺**：`RuntimeDirectory`、`PrivateTmp`、`RestrictAddressFamilies`、`SystemCallFilter` 等加固项 | 可直接写；实际行为需 Linux + systemd 验证 |
| 测试隔离缺陷 | `cmd/print-bridge` 的 IPC 测试依赖全局命名管道，本机有 Agent 运行时必失败 | 让 `ipc_windows.go` 的管道名可由环境变量覆盖，测试用独立名字 |

### 1.4 其他小项

| 项 | 说明 |
| --- | --- |
| `QUEUE_FULL` 错误码 | 已确认**不在**协议错误码集合内。删除会改变现有客户端可见行为，需确认后再动 |
| 内联 HTML 载体 | 当前用 `data:text/html;base64,` URL，无真实 origin，相对路径子资源行为受限。改动面较大（需代理提供合成 origin + 路由） |
| `data:` URL 接受范围 | 仅 `format=="pdf"` 且字面前缀匹配。属真实差异，改动小 |

---

## 二、需要真实环境验收（无法只靠代码完成）

| 项 | 需要的条件 | 待验内容 |
| --- | --- | --- |
| Office 转换 | Microsoft Office / WPS / LibreOffice | COM 激活阶段卡死、宏不执行、不影响用户已开会话、120 s 超时清理。WPS 表格已完成 5 s 与 120 s 超时清理验证，其余转换器待验 |
| CUPS 打印 | Linux 环境 + 打印机 | 逐格式提交、纸张解析（`media=`）、系统队列追踪 |
| 各驱动纸张矩阵 | 多种打印机 | 自定义尺寸回退、方向、份数在真实设备上的效果 |
| 浏览器真实业务页面 | HTTPS 业务站点 | Origin 白名单、复杂页面版面、HTML 子资源 |
| 公示页面打印 | 公网访问 | 复杂公网页面版面正确性 |
| 安装包覆盖升级 | Windows 桌面 | 已完成一次验证；签名后需重验 |
| macOS 桌面 | macOS 设备 | MyGo 原生窗口、签名、公证 |

---

## 三、已知残余风险

| 项 | 说明 |
| --- | --- |
| DNS 重绑定 | 下载器在 dial-time 校验解析结果，但 `ValidateURL` 无法识别十进制/十六进制/短写 IP 形式。**不要移除 `security.Client` 的 `DialContext` 检查** |
| 浏览器直连绕过代理 | Chromium 刻意移除代理参数时，loopback 拦截仍有效（走 CDP `Fetch`），但非 loopback 直连无法拦截 |
| `fit_width`/`fit_height` | SumatraPDF 无单方向拟合能力，实际退化为完整包含。若业务要求严格按宽度铺满，需在 HTML 侧把内容做成目标宽高比 |
| 自定义纸张尺寸 | 不可用。尺寸必须在驱动声明列表中，否则 `PAPER_NOT_FOUND`。**该取舍已确认保持** |

---

## 四、发布门槛

以下能力未通过对应真实测试前，不得标为生产可用：

1. HTML 子资源绕过（真实 Chromium + 负对照）
2. Office/WPS 进程归属与超时清理
3. 真实打印机出纸（各格式、各驱动）

代码签名是**交付门槛**：未签名安装包会触发 SmartScreen 警告，不适合分发给终端用户。
