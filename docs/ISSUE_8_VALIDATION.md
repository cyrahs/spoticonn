# Issue #8：iOS 暂停控制回传

## 已确认的复现条件

2026-09-08 用户确认入口为 **iOS 控制中心 → 控制其他扬声器与电视 → Apple TV → 暂停**。Apple TV + HomePod 组合正在播放时，此按钮不能让 HomePod 停止出声；Spotify Connect 可以暂停。

按钮语义确认为暂停。尚未记录设备型号、iOS/tvOS/HomePod 固件、实际请求落到哪个成员，以及用户点击时接收端发出的协议事件。没有依据认定全部 iOS 控制失效，也没有把封面或曲目信息作为控制验收。

## 源码确认的缺口与修复

固定版本 cliairplay v0.5.3 已有接收端控制回调，并在 stdout 输出 `[EVENT] remote command=…`。Spoticonn 原先只解析 `[STATUS]`，会忽略全部此类事件。补入严格的事件白名单，将独立输出和组合两成员的回调接到打开该输出的 Spotify 账号及进程代次。

暂停现在同步向 Spotify 发出 `pause`，断开 PCM 路由、排空本地音源、取消 Apple TV 加入、对保留的 HomePod 输出执行 FLUSH/STANDBY，并更新网页状态；不依赖之后的 Spotify WebSocket 确认才清空音频。暂停取消自动恢复；暂停期间的发送器断线不会启动恢复。清空／待机失败时关闭输出并报告失败，不因此恢复播放。

每个事件携带物理成员的上下文、接收时间和输出／Spotify 进程代次。账号接管、输出重建、进程重启、成员取消或退出后，旧事件不能作用于新播放。管理网页的新控制也会屏蔽此前排队的接收端请求。

两成员共享去重：播放／暂停／切换共用 500 ms 静默窗口，上一首和下一首各用 2 s 静默窗口。持续重复请求会延长窗口，避免反复切换或切歌；这也意味着窗口内的快速连续按键可能被合并。播放确认会与 Spotify 实时状态核对，已播放时不再次 resume。无法读取状态时不猜测切换方向，明确的 pause 仍尝试暂停。

| 引擎事件 | Spotify 动作 | 当前验证边界 |
| --- | --- | --- |
| `pause` | `pause` + 同步清空输出 | 模拟测试覆盖；iOS 实机待验收 |
| `play` | 已暂停且会话仍活动时 `resume` | 模拟测试覆盖；实机待验收 |
| `play_pause` | 根据 Spotify 实时状态选择暂停／恢复 | 模拟测试覆盖去重；实机待验收 |
| `next` / `previous` | `next` / `prev` | 模拟测试覆盖风暴抑制；实机待验收 |
| 原生 stop / seek / 音量 | 此版事件契约未提供，未接入 | 不宣称支持；管理网页与 Spotify 的能力另行验证 |

### DACP 与组合恢复限制

- `--dacp` 同时参与 HAP 配对身份。Spoticonn 已保存每个物理成员的 DACP 值，并与对应凭据一起用于发送；本次保持原配对身份。
- `--activeremote` 用于独立的 DACP 回调路径。当前未注册 `_dacp._tcp` 服务、未提供 DACP HTTP 回调，也未配置会话专用的 Active-Remote。**本次修复依赖引擎的 MediaRemote 事件路径**；仅走 DACP 的接收端仍不能回传控制。没有新增网络监听端口。
- 暂停会取消并关闭分阶段组合的 Apple TV 会话，而保留 HomePod 的待机会话。因此暂停后 Apple TV 原生恢复入口可能消失或不再回传，恢复播放请优先使用 Spotify 或管理网页。此 PR 不宣称完整原生控制接管。
- 诊断中的 `AirPlay 远程控制：Apple TV / pause → Spotify / pause` 表示收到了事件并尝试控制音源；若执行失败会另记固定错误。日志不含原始引擎输出、设备／账号标识、凭据或请求负载。没有出现该记录只能说明应用没有处理到有效事件，不能单凭这一点判断 iOS 把请求发给了谁。

源码依据：

- [cliairplay v0.5.3 回调及 stdout 输出](https://github.com/music-assistant/airplay-cli/blob/v0.5.3/src/cliairplay.c)：`remote_command_event`、`ap2cl_set_remote_command_callback`。
- [cliairplay v0.5.3 接收端命令解码](https://github.com/music-assistant/airplay-cli/blob/v0.5.3/src/ap2_mrp.c)：`mrp_parse_remote_command`、`sendMediaRemoteCommand`。
- [cliairplay v0.5.3 参数与双输出流约定](https://github.com/music-assistant/airplay-cli/blob/v0.5.3/README.md)：DACP/Active-Remote、配对身份及远程事件。
- [Music Assistant 2.10.2 事件解析与重复请求处理](https://github.com/music-assistant/server/blob/2.10.2/music_assistant/providers/airplay/stream.py)：`_parse_remote_event`。

## 实机验收记录

本地验证：Go 全量 race 测试、`go vet ./...`、前端 13 项测试、格式检查、TypeScript/Vite 构建及 Linux amd64／arm64 编译通过。回归测试覆盖子进程 stdout、组合成员上下文、账号／进程隔离、重复切换与切歌、暂停失败、异步确认和暂停后的断线／音频错误。

本地仅使用模拟引擎，没有访问真实 Spotify、Apple TV 或 HomePod，也没有部署家庭集群。下表由 home 实机验收补齐；**自动测试通过不能代替这些结果，也不能据此关闭 issue #8**。

先记录设备与固件、镜像提交、配对情况、组合拓扑和测试账号备注。使用上面用户确认的入口；针对 HomePod 入口另做一轮并记录差异，不假定两者相同。

| 检查 | 操作与通过条件 | 实机结果 |
| --- | --- | --- |
| 用户入口 | 控制中心 → 控制其他扬声器与电视 → Apple TV → 暂停 | 用户已确认；复测待执行 |
| 请求来源 | 对应操作时间核对脱敏诊断，分别记录 Apple TV / HomePod 是否回传及动作 | 待执行 |
| 暂停 | HomePod 停止出声，Spotify 为暂停，Apple TV 状态一致；记录延迟 | 待执行 |
| 持续暂停 | 暂停后等待至少 30 s；无恢复、无重复暂停／播放循环；模拟接收端断线仍保持暂停 | 待执行 |
| 账号接管 | A → B 后操作 B；只有 B 被控制，旧会话的迟到请求不影响 B | 待执行 |
| 成员加入 | Apple TV 加入中及加入后分别暂停，无残留声音或重新加入 | 待执行 |
| Spotify／网页恢复 | 暂停后恢复，HomePod 出声，Apple TV 重新加入 | 待执行 |
| iOS 原生恢复 | 分别记录 Apple TV 和 HomePod 入口的可见性、动作及结果，允许记录不支持 | 待执行 |
| iOS 原生切歌 | 分别验证上／下一首及快速重复点击，记录是否跳过多首 | 待执行 |
| iOS 原生音量 | 独立记录实际音量及 Spotify／网页状态，不由暂停结果推定 | 待执行 |

如果用户点击暂停仍无远程事件，继续定位设备端是否走 DACP、是否建立接收端事件通道；不要以反复重启 AirPlay 或修改封面代替控制链路调查。
