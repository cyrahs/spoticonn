# Spoticonn

在家中的 Linux 设备上常驻运行，把多个 Spotify Premium 账号连接到一个 AirPlay 输出。目标是：在 Spotify 中选择设备，即可从与 HomePod 配对的 Apple TV 播放，管理网页可以关闭。

```text
账号 A 的 Connect 会话 ─┐
账号 B 的 Connect 会话 ─┼─ 唯一活动音频流 → AirPlay 2 → Apple TV / HomePod
账号 C 的 Connect 会话 ─┘
```

每个账号有独立、持久的 Spotify device ID，显示相同的自定义名称。新账号播放会暂停旧账号；旧账号的登录会话保持在线。Spotify 使用 go-librespot **v0.9.0**，AirPlay 使用 cliairplay **v0.5.3**，以独立子进程运行。

## 当前交付范围

- 中文管理网页：管理登录、账号添加／删除／重绑、AirPlay 发现与配对、输出选择、音量、播放控制、实时状态和诊断。
- 后端：凭据隔离、原子持久化、设备身份复用、临时 Zeroconf 绑定、账号进程保活、输出接管、PCM 节奏控制、异常重连。
- 单镜像构建及 k3s Kustomize 配置，支持 Linux amd64／arm64。
- 使用模拟音频引擎的单元和进程集成测试，不需要真实 Spotify 凭据。

**真实设备验证尚未完成。** 电视关闭播放、各账号外网常驻可见、长时间播放及待机恢复，需要 home 上的 agent 按 [部署与验收交接](docs/HOME_HANDOFF.md) 完成。原生 Apple 发送端能够播放，不等同于本项目的 Linux 发送端已经通过兼容性验证。

## 首次使用

1. 通过内网或 VPN 打开管理网页，使用部署时设置的管理密码登录。
2. 选择 AirPlay 输出。如果 HomePod 已设为 Apple TV 默认输出，选择二者显示的合体设备。
3. 如设备要求配对，点击配对，输入电视上显示的四位码。日常播放是否需要开电视，以实机验收为准。
4. 添加 Spotify 账号备注。用该 Premium 账号在同一局域网的 Spotify App 中选择 `Spoticonn · 配对 xxxx`，播放一次。
5. 后端保存凭据，将临时配对设备转为该账号的常驻会话；对其他账号重复操作。一次只绑定一个账号，临时绑定十分钟后超时。
6. 各账号在自己的 Spotify 中选择配置的同名设备。B 发起播放时，A 暂停，音频输出切到 B。

更改输出会暂时暂停并重新连接；失败时保留新选择并显示原因。改播放器名称会重新连接账号并暂停播放。删除账号只删除此服务保存的凭据，不修改 Spotify 账户本身。

## 开发与构建

需要 **Go 1.25+、Node.js 22.12+、npm**。应用不需要本地音频库；实际播放需要两个独立引擎。

```sh
make build
make test
```

`make build` 构建网页并嵌入 Go 二进制，产物为 `bin/spoticonn`。完整镜像构建会下载经过 SHA-256 校验的 Linux 引擎。

GitHub Actions 按模块验证代码，并用固定名称的 **CI gate** 作为 PR 汇总检查。`main` 和 `v*` 版本标签通过全部检查后，会将已测试的 amd64／arm64 镜像发布到 `ghcr.io/cyrahs/spoticonn`。标签、权限与分支保护设置见 [CI 与发布说明](docs/CI.md)。

启动本地管理界面（即使未安装音频引擎也可检查设置流程）：

```sh
export SPOTICONN_ADMIN_PASSWORD='替换为至少12字节的管理密码'
./bin/spoticonn
```

默认地址为 `http://127.0.0.1:8080`，数据目录为 `./data`。不要将示例密码用于正式部署。开发热更新可在另一个终端执行 `cd web && npm run dev`，Vite 会把 `/api` 转到 Go 服务。

正式部署使用密码哈希文件或 Kubernetes Secret。`spoticonn hash-password` 从标准输入读取一行密码，输出 bcrypt 哈希；避免把真实密码写入仓库、命令行参数或日志。更换密码并重启服务会使现有管理会话失效。

## 配置

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `SPOTICONN_LISTEN_ADDR` | `127.0.0.1:8080` | 管理 HTTP 监听地址；home 上明确绑定实际 LAN IP |
| `SPOTICONN_INTERFACE` | 空 | 家庭网络接口名，如 `enp1s0`；k3s 中必须指定真实接口 |
| `SPOTICONN_DATA_DIR` | `./data` | 持久数据目录，仅允许运行一个实例 |
| `SPOTICONN_RUNTIME_DIR` | 系统临时目录 | 私有 FIFO、临时运行文件的父目录 |
| `SPOTICONN_ADMIN_PASSWORD_HASH_FILE` | 空 | bcrypt 哈希文件，优先于其他密码设置 |
| `SPOTICONN_ADMIN_PASSWORD_HASH` | 空 | 直接提供 bcrypt 哈希 |
| `SPOTICONN_ADMIN_PASSWORD` | 空 | 仅用于方便本地启动，12–72 字节 |
| `SPOTICONN_SECURE_COOKIES` | `false` | 如果管理入口由 HTTPS 代理提供，设为 `true` |
| `SPOTICONN_SPOTIFY_BINARY` | `go-librespot` | Spotify 引擎路径 |
| `SPOTICONN_AIRPLAY_BINARY` | `cliairplay` | AirPlay 引擎路径 |

仅本地浏览器使用管理会话；音频引擎 API 绑定 loopback，不直接暴露给网页。管理 Cookie 为 HttpOnly、SameSite=Strict，默认有效十二小时，状态接口不缓存。没有公网 Ingress 或 Spotify 网页 OAuth 回调依赖。

凭据保存在数据卷中的私有目录／文件（0700／0600）。这不是应用层加密；拥有宿主机或卷读取权限的管理员可以读取它们。备份整个数据目录时应将备份按凭据文件对待。

## 播放与恢复规则

- 首版固定为 Spotify 320 kbps、立体声 s16le／44.1 kHz，不请求 Spotify FLAC，不做多房间同步。
- FIFO 按实时速率消费，限制预读，避免播放器在音频尚未出声前就跑完整首歌曲。
- 接管时断开旧音频路径，暂停旧账号。新账号先暂停，等待 AirPlay 连接和时钟就绪，再回到请求的播放位置继续。
- 跨账号接管和换目标使用新的发送进程；同账号切歌／seek 清空发送缓存，防止旧内容串入新播放。
- 重启旧进程后的迟到事件被 generation 标识丢弃；暂停／播放事件还需与实时状态核对，避免内部控制事件误夺输出。
- 音量由 AirPlay 端应用一次；Spotify 引擎关闭 PCM 音量衰减。
- Spotify 子进程失败后退避重启；AirPlay 出错时先暂停 Spotify，再以退避方式重连并恢复当前请求。网页主动暂停会取消输出自动恢复。
- 服务重启后恢复账号在线状态，不主动开始播放。设备离线不使管理服务健康检查失败。

## 管理 API

所有 `/api` 接口除登录外需要管理 Cookie。修改请求带 `X-Spoticonn-Request: 1`，浏览器 Origin 必须匹配当前服务。JSON 字段之外的未知字段会被拒绝。

| 方法与路径 | 请求 / 作用 |
| --- | --- |
| `POST /api/auth/login` | `{ "password": "…" }` |
| `POST /api/auth/logout` | 撤销当前管理会话 |
| `GET /api/state` | 完整界面状态，不包含凭据 |
| `GET /api/events` | SSE，`state` 事件携带完整状态 |
| `GET /api/accounts` | 账号列表与会话状态 |
| `POST /api/accounts` | `{ "label": "我的账号" }`，创建临时配对设备 |
| `POST /api/accounts/{id}/rebind` | 删除该账号旧凭据并重新绑定，保留 device ID |
| `DELETE /api/accounts/{id}` | 停止账号实例并删除凭据 |
| `GET /api/airplay/devices` | 自动发现的 AirPlay 2 设备 |
| `POST /api/airplay/pairings` | `{ "device_id": "…" }` |
| `POST /api/airplay/pairings/{id}/pin` | `{ "pin": "1234" }` |
| `POST /api/airplay/pairings/{id}/cancel` | 取消进行中的配对 |
| `GET /api/settings` | 读取全局配置 |
| `PUT /api/settings` | `{ "name": "客厅", "target_id": "…", "volume": 30 }`，完整替换 |
| `POST /api/playback` | `{ "action": "pause" }`；支持 `resume`、`next`、`prev`、`seek`、`volume`，后两者带 `value`（毫秒或 0–100） |
| `GET /healthz`、`GET /readyz` | 管理服务健康状态，不代表真实设备播放验收 |

## 目录与交接

`internal/bridge` 管理生命周期和接管；`internal/spotify`、`internal/airplay` 适配固定引擎；`internal/audio` 管理 PCM；`web` 是管理界面；`deploy` 是 k3s 清单。

部署、备份和实机验收请从 [docs/HOME_HANDOFF.md](docs/HOME_HANDOFF.md) 开始。第三方许可证见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
