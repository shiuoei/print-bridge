# PrintBridge

本地静默打印代理。浏览器业务系统通过 WebSocket 把 PDF、图片、Office 文档、HTML 或原始打印指令交给它，由它提交到本机打印机，无需弹出打印对话框。

桌面端使用 Go + [MyGo](https://github.com/egoist/mygo) 原生控件（无 WebView），另有 Linux headless 服务与 CLI。

> **开发中，尚未达到生产发布条件。** Windows 核心链路已在真实打印机上验证，但跨设备、跨平台、Office 安全边界与代码签名尚未完成。详细状态见 [开发说明](docs/DEVELOPMENT.md)。

## 项目来源

本项目参照 [vergil-lai/print-bridge](https://github.com/vergil-lai/print-bridge) 的**协议与功能范围**重新实现。

- 参照的是行为契约：WebSocket 消息格式、任务状态语义、错误码集合；
- 代码为独立的 Go 实现，**不包含原项目的任何源代码**，也不是其衍生作品；
- 安装标识、数据目录、配置、历史与发布包完全独立，不读取也不迁移原版数据。

本项目以 [Apache License 2.0](LICENSE) 发布。随包分发的第三方组件见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。

## 快速开始

要求 Go 1.27+，Windows 构建另需 Node.js（用于前端资源）。

```powershell
$env:GOCACHE='D:\path\to\print-bridge\.cache\go-build'
$env:CGO_ENABLED='0'

go run ./cmd/desktop                  # 桌面程序
go run ./cmd/server serve             # Linux headless 服务
go run ./cmd/print-bridge status      # CLI（需 Agent 在运行）
go test ./...                         # 全部测试
```

打包 Windows 安装包：

```powershell
go run github.com/egoist/mygo/cmd/mygo build -platform windows/amd64
```

产物在 `build/windows-amd64/`。

发布版支持签名自动更新：启动后自动检查 GitHub Releases，也可从托盘菜单或"关于"页手动检查；更新包经内置公钥验签、支持增量下载，确认后重启生效。

发布新版本不需要本地安装 GitHub CLI：提交版本号与 [docs/RELEASES.md](docs/RELEASES.md) 的发布说明后，推送 `v<version>` tag 即可，[GitHub Actions](.github/workflows/release.yml) 会构建、用仓库 Secret 里的私钥签名、创建并发布 Release。私钥用 `mygo keygen` 生成且**不入库**；完整步骤见[开发说明的"发布与自动更新"](docs/DEVELOPMENT.md#发布与自动更新)。

## 业务系统对接

```js
const ws = new WebSocket('ws://127.0.0.1:17890/ws');

ws.onopen = () => ws.send(JSON.stringify({
  type: 'print',
  job_id: 'job-1',
  format: 'pdf',
  data_base64: '<base64 内容>',
  copies: 1,
  paper: { width_mm: 100, height_mm: 150 },
}));
```

完整协议、字段说明与可直接使用的客户端实现见 [开发说明 · 对接](docs/DEVELOPMENT.md#对接协议)。

**两件必须先知道的事：**

1. **业务站点的 Origin 要加白名单**，否则握手返回 403。在桌面「访问控制」页添加，必须精确匹配（如 `https://www.example.com`）。
2. **HTTPS 页面可以直接连 `ws://127.0.0.1`**，不需要 `wss://`。浏览器把 loopback 地址视为可信来源，不按混合内容拦截。

## 状态语义

`submitted` 只表示**任务已提交到系统打印队列**，不代表物理出纸。无法确认物理结果时，本程序不会宣称 `completed`。

## 文档

| 文档 | 内容 |
| --- | --- |
| [AGENTS.md](AGENTS.md) | 开发约定：不可改变的产品边界、修改指引、交接格式 |
| [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) | 架构、模块职责、协议细节、构建与验证方式 |
| [docs/CHANGELOG.md](docs/CHANGELOG.md) | 变更记录与验证证据 |
| [docs/TODO.md](docs/TODO.md) | 待办事项 |
| [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) | 第三方组件与许可证 |
