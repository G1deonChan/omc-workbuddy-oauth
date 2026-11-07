<p align="center">
  <img src="assets/workbuddy.svg" width="96" height="96" alt="OMC WorkBuddy OAuth logo">
</p>

<h1 align="center">OMC WorkBuddy OAuth</h1>

<p align="center">
  <a href="https://github.com/WizisCool/oh-my-cpa">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/WizisCool/oh-my-cpa/main/web/src/assets/brand/omc-wordmark-dark.svg">
      <img src="https://raw.githubusercontent.com/WizisCool/oh-my-cpa/main/web/src/assets/brand/omc-wordmark-light.svg" alt="Oh-My-CPA" height="22">
    </picture>
  </a>
</p>

<p align="center">
  为 <a href="https://github.com/router-for-me/CLIProxyAPI">CLIProxyAPI</a>
  提供原生的 WorkBuddy / WorkBuddy AI OAuth 登录
</p>

<p align="center">
  <a href="https://github.com/omc-community/omc-workbuddy-oauth/releases"><img src="https://img.shields.io/github/v/release/omc-community/omc-workbuddy-oauth?label=release" alt="Release"></a>
  <a href="https://github.com/omc-community/omc-workbuddy-oauth/actions/workflows/build.yml"><img src="https://github.com/omc-community/omc-workbuddy-oauth/actions/workflows/build.yml/badge.svg" alt="CI"></a>
  <img src="https://img.shields.io/badge/CLIProxyAPI-v8+-4f46e5" alt="CLIProxyAPI v8+">
  <img src="https://img.shields.io/badge/license-MIT-blue" alt="MIT license">
</p>

<p align="center">
  <a href="README.md">English</a> · <a href="README.zh-CN.md">简体中文</a>
</p>

OMC WorkBuddy OAuth 为 CLIProxyAPI 增加 `workbuddy` provider。在面板的 OAuth 流程里用
WorkBuddy 账号登录后，CPA 即获得该账号自身的模型列表、OpenAI 兼容对话执行和标准化配额，
无需独立进程，也无需本地 WorkBuddy 桌面客户端。

本项目是 [Oh-My-CPA](https://github.com/WizisCool/oh-my-cpa) 生态的一部分：
Oh-My-CPA 是 CLIProxyAPI 的自托管控制面板与用量观测台。

## 两个版本，一个 provider

WorkBuddy 分为两个版本，它们是不同主机上的不同账号。两者产生同一种凭证
（`"type": "workbuddy"`）并共用同一个 executor，因此插件注册两个独立 OAuth 登录入口，
而不是一个入口加一个版本下拉框。

| 版本 | 登录入口 | API 主机 |
| --- | --- | --- |
| WorkBuddy（中国版） | `workbuddy` | `https://copilot.tencent.com` |
| WorkBuddy AI（国际版） | `workbuddy-ai` | `https://www.workbuddy.ai` |

在渲染逐入口元数据的面板（CPA v8.0.51+）上，每个入口各自成为一张卡片，带自己的名称
和标识。在更旧的宿主上两个标识符仍然可用，面板会为它们显示插件自身的名称和 logo。

- 选入口即选版本。入口优先于任何 `site` 提示。
- 每份凭证都会记录签发它的版本，字段为 `site`，同时写入凭证的 `oauth_provider`。
- `site`（默认 `workbuddy`）只是未指定入口时的兜底，例如宿主仅以插件标识符发起登录。
- 显式但无效的 `site` 值会被拒绝，而不是静默解析成默认值。

## 面板无关

所有面向用户的部分都由宿主渲染：OAuth 卡片、配置字段、静态资源和配额视图都通过 CPA
的插件 ABI 与管理 API 呈现。插件在
[Oh-My-CPA](https://github.com/WizisCool/oh-my-cpa) 中是一等公民，在 CPA 自带的
Management Center 或任何对接 CPA 管理 API 的面板中表现一致。

OAuth 发起和轮询使用 CPA 原生受认证管理 API。公共插件资源仅包含两个静态标识与可选的
描述声明，即 `workbuddy.svg`、`codebuddy-color.svg` 与 `omc.json`。对这些路由的请求无法
发起登录流程，查询参数也不会触发任何动作。

登录使用 WorkBuddy 自带桌面端所用的浏览器流程。插件向 WorkBuddy 申请登录页，打开腾讯为
该版本提供的页面，在同一个流程上轮询令牌，并读取令牌所属的账号。无需本地回调监听器，
也无需手动粘贴回调地址。桌面端登录不可用：插件运行在服务器上的 CPA 进程内，不读取任何
客户端文件。

## 功能

- WorkBuddy 与 WorkBuddy AI OAuth，每个版本一个登录入口。
- 使用 WorkBuddy 轮换式 refresh 令牌续期，同一凭证同一时刻只花一次；企业（团队）账号
  在每次请求上携带 `X-Enterprise-Id` 与 `X-Tenant-Id`。
- 基于该版本 `/v3/config` 的模型发现，按 CPA 路由要求转小写；配置读取失败时使用内置
  兜底列表。
- OpenAI 对话补全进出，支持流式与非流式，包括 tool calls、reasoning、图片输入和 usage。
  WorkBuddy 只接受流式对话，因此非流式请求由插件把流读成一条完整回答。
- 端点要求的请求形态：`max_tokens` 代替 `max_completion_tokens`，`system` 代替
  `developer`，补上 WorkBuddy 默认 system 消息，`reasoning` 代替 `reasoning_content`，
  客户端未指定推理档位时补上该模型的默认档位。
- WorkBuddy 的请求标识（会话、回合、消息）由对话内容推导，使一次用户发送在账号用量明细
  里仍是一条请求。
- 来自账单单据的标准化配额，按 WorkBuddy 自身面板的口径拆分：套餐积分、额外积分、账号
  总计，以及预付费余额。
- 按凭证的并发控制。
- 导入已有的 `"type": "workbuddy"` 凭证文件。
- 错误映射保留各自状态：登录过期、无权限、频率限制、渠道拒绝不会被折叠成笼统的上游失败。

## 安装

把本仓库加入插件商店来源：

```yaml
plugins:
  enabled: true
  dir: /path/to/plugins
  store-sources:
    - https://raw.githubusercontent.com/omc-community/omc-plugins/main/official.json
  configs:
    omc-workbuddy-oauth:
      enabled: true
```

然后在插件商店安装 `omc-workbuddy-oauth`，并在面板中打开它的 OAuth 卡片。商店与动态库
名称为 `omc-workbuddy-oauth`；CPA 凭证 provider 标识仍为 `workbuddy`。登录、配置、
配额与排障见 [docs/USAGE.md](docs/USAGE.md)。

推送 `v*` 标签时由 GitHub Actions 生成发布。

## 配置

| 字段 | 类型 | 默认值 | 取值 |
| --- | --- | --- | --- |
| `site` | enum | `workbuddy` | 未指定入口时的兜底版本：`workbuddy`、`workbuddy-ai` |
| `max_concurrency` | integer | `3` | 每凭证 1–100 个并发请求 |
| `app_version` | string | `5.9.3` | 客户端 `User-Agent` 声称的 WorkBuddy 桌面版本 |
| `cli_version` | string | `2.165.0` | 客户端 `User-Agent` 声称的 WorkBuddy CLI 版本 |

WorkBuddy 会拒绝 `User-Agent` 不以 `WorkBuddy/<version>` 开头的对话（错误 `10085`），
因此两个版本号属于配置而不是常量：厂商升级构建不需要新版本发布。

## 开发

插件实现位于 [`go/`](go/)。下载依赖后测试无需网络：

```sh
cd go
go mod download
GOPROXY=off GOSUMDB=off go test ./...
GOPROXY=off GOSUMDB=off go vet ./...
```

为当前机器本地构建动态库：

```sh
cd go
CGO_ENABLED=1 go build -buildmode=c-shared -o ../build/omc-workbuddy-oauth.so .
```

macOS 使用 `.dylib`，Windows 使用 `.dll`。`build/` 已被 git 忽略。

发布工作流在打标签后构建以下产物：

```text
omc-workbuddy-oauth_<version>_linux_amd64.zip
omc-workbuddy-oauth_<version>_linux_arm64.zip
omc-workbuddy-oauth_<version>_darwin_amd64.zip
omc-workbuddy-oauth_<version>_darwin_arm64.zip
omc-workbuddy-oauth_<version>_windows_amd64.zip
omc-workbuddy-oauth_<version>_windows_arm64.zip
checksums.txt
omc-workbuddy-oauth.omc.json
```

每个压缩包根目录只有名称正确的 `omc-workbuddy-oauth.so`、`.dylib` 或 `.dll`。
`checksums.txt` 覆盖全部压缩包，插件商店下载后按它校验。`omc-workbuddy-oauth.omc.json`
是 [docs/USAGE.md](docs/USAGE.md) 中描述的可选声明；它发布的字节与插件内嵌并对外提供的
字节完全一致。

提交规范、测试要求与生态开发标准见 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 项目结构

```text
assets/workbuddy.svg        WorkBuddy 标识：插件 logo 与中国版登录入口
assets/codebuddy-color.svg  CodeBuddy 标识：WorkBuddy AI（国际版）登录入口
go/                      Go/cgo 插件实现与测试
go/assets/               内嵌资源与 omc.json 声明
docs/USAGE.md            运维安装与使用指南
```

Go 实现是独立代码。协议来源，包括登录流程、请求头和对话改写，记录在
[NOTICE.md](NOTICE.md)。

## 许可证

[MIT](LICENSE)
