# Issue #16：Apple TV 封面更新与验收

## 已确认的代码问题

基线 `269f7b328dc9f32e25991fdba5b4ba05bcc298e1` 已有封面下载与发送，问题不是缺少 Cover 字段。该版本每次调用 Metadata 都发送不带图片的 SENDMETA；下载完成再完整发送一次。同一曲目的进度、时长或恢复通知也会重复 SENDMETA。切换封面还会额外发送清除指令。这些命令序列符合 MA 注释中应避免的密集 Now Playing replace 模式，但没有实机证据能将其认定为本次用户报告的唯一根因。

另外，原 PROGRESS 位于 SENDMETA 之前，而 v0.5.3 的新 ITEMID 会将 elapsed 清零；原本只保留最近 8 个临时文件，无法保证滞后的引擎在打开旧路径前文件仍在。原状态解析会丢弃拒绝原因及数值详情。

## 本次行为

- 封面准备最多等待 200 ms，期间不持有发送器控制锁。正常下载、热缓存和晚加入成员优先收到一次 `ARTWORKFILE + SENDMETA + PROGRESS`。START/Join 可以立即结束等待，并先发送文字，再启动音频。慢下载完成后只发 `ARTWORK`。
- 文字身份、图片准备状态、已写入 FIFO 的封面身份和进度分别记录。进度与时长变化不重发文字；完全相同的进度也不重复发送。文字细化仅在文字确有变化时发送，并保留同曲目已交付的封面。没有引入另一个元数据来源；两个成员都使用当前 Spotify Track 的副本。
- 同专辑新曲目仍将封面合入 SENDMETA：v0.5.3 在新 ITEMID 的无图更新中会清除上一首图片。相同图片文件复用；暂停、恢复和 seek 保留同曲目已交付身份，只重新校正进度。新子进程有独立状态，必须接收完整当前信息。
- 新曲目在无图或失败时通过 SENDMETA 清除 MRP 旧图；同曲目移除、更换失败或超过等待预算时，只在确有旧图时发一次 `ARTWORK=`。准备及时完成的替换不先清除旧图。成功准备图片不等于 FIFO 交付，失败写入不会提交交付身份，后续 Metadata 可重试。
- 切歌、FLUSH、STANDBY、关闭使旧下载代次失效。下载取消、超时、格式错误、写文件失败均保留文字和音频控制流程；普通进度通知不重复尝试失败下载。
- 输入经过大小、类型、解码和尺寸检查，统一重编码为最长边不超过 512 像素、保持比例的 baseline JPEG。透明像素在白底上合成，文件不超过 1 MiB。原下载地址校验、共享缓存和超时继续生效。
- 每个子进程使用私有目录（0700）和不可变图片文件（0600），按完整图片字节的 SHA-256 去重。已交付路径保留至子进程退出，不再按最近 8 个文件删除。内存下载缓存仍限 8 项；磁盘占用随该子进程见过的不同封面数量增长，正常退出时统一清理。当前引擎没有可靠的逐文件消费确认，不能用 FIFO 写入或去重后缺省的 MRP 状态作为删除依据。

## 上游版本与命令依据

| 项目 | 核对结果 |
| --- | --- |
| Spoticonn | `scripts/fetch-engines.sh` 固定 cliairplay v0.5.3，两种 Linux 架构分别校验 SHA-256；本次不变更引擎 |
| MA 2.10.2 | [Dockerfile](https://github.com/music-assistant/server/blob/2.10.2/Dockerfile) 同样固定 v0.5.3，并校验 SHA256SUMS 清单及对应发布物；并非不同引擎版本 |
| 图片准备 | MA [constants.py](https://github.com/music-assistant/server/blob/2.10.2/music_assistant/providers/airplay/constants.py) 使用 512 像素、1.5 s 等待；Spoticonn 使用 512 像素、不放大小图，等待缩短为 200 ms |
| 合并与补发 | MA [stream.py](https://github.com/music-assistant/server/blob/2.10.2/music_assistant/providers/airplay/stream.py) 的 send_metadata、_render_artwork_bounded、_render_and_send_artwork 使用合并、有限等待和 ARTWORK 补发，并分别去重文字、封面与进度 |
| 命令契约 | cliairplay [cliairplay.c](https://github.com/music-assistant/airplay-cli/blob/v0.5.3/src/cliairplay.c) 中 ARTWORKFILE 是一次性文件暂存，SENDMETA 消费它；ARTWORK 直接应用文件；空 ARTWORK 走本地拒绝/清除路径，空 ARTWORKFILE 仅取消暂存 |
| 图片保留 | cliairplay [ap2_mrp.c](https://github.com/music-assistant/airplay-cli/blob/v0.5.3/src/ap2_mrp.c) 的 ap2_mrp_set_track 在新曲目无图时清除旧图，在同曲目文字细化时保留；相同图片字节保留 ArtworkIdentifier |

## 可观察证据

封面诊断带固定成员类型（AppleTV / HomePod / AirPlay）及随机子进程 session，发送记录含 generation、item_changed、cover_present、attached、clear 和白名单命令类型。准备记录含 JPEG 格式、字节数、尺寸与文件关闭状态；不会输出曲目 URI、名称、远程 URL、本地路径、凭据或原始引擎行。

MRP 记录保留 `path=command/channel`、`artwork=posted/rejected`、白名单 reason，以及合法的 status、clear_status、bytes、width、height、precision、components、progressive、staging_max_bytes。未知 reason 统一为 unknown；不回显未知字段或原始文本。时间由现有诊断事件记录。接收端事件没有曲目/代次标识，不能把一个迟到状态强行归到当时的当前曲目，应通过同一 session 的时间顺序核对。

`fifo=written` 只证明命令交付给管道；`posted status=200` 只证明对应协议响应，均不证明电视屏幕显示成功。主动清除也可能产生 `rejected reason=invalid_artwork`，需结合前面的 `clear=true` 区分。

## 自动验证与实机边界

本地 Go 全量 race 测试与 go vet 通过。命令序列和模拟子进程测试覆盖冷/热缓存、慢下载、START/Join 中断等待、文字/图片/进度去重、暂停/恢复、FLUSH、旧下载隔离、晚加入期间切歌、写入失败重试、透明 PNG、JPEG 归一化、文件权限与生命周期、拒绝状态脱敏。模拟子进程实际打开图片并校验字节摘要，但没有运行真实 Apple TV 协议栈。

没有连接真实 Spotify、Apple TV 或 HomePod，没有部署 home，也没有取得同曲目 MA 实机对照。因此本 PR 可以合并代码修复，**不能据此关闭 issue #16 或勾选屏幕显示验收**。

在 home 部署明确的合并提交后，记录 Spoticonn 镜像提交、MA 2.10.2/引擎版本、tvOS/HomePod 固件、设备输出关系、测试曲目与操作时间；封面异常区分空白、旧封面和偶发缺失。对同曲目和同设备关系进行 MA 对照，不假定上游注释等于本次对照成功。

| 验收项 | 通过条件与记录 | 实机结果 |
| --- | --- | --- |
| 晚加入 | HomePod 先出声，Apple TV 加入后显示当前正确封面；保留屏幕照片与相应成员诊断 | 待执行 |
| 切歌 | 连续切歌与同专辑切歌，图片均对应当前曲目，不残留上一首 | 待执行 |
| 暂停/恢复/seek | 封面保持正确；同曲目没有无必要的 SENDMETA/ARTWORK 重发，HomePod 音频正确 | 待执行 |
| 冷/热缓存 | 首次播放与重复播放均正常，核对合并发送及本地文件准备记录 | 待执行 |
| 慢下载 | START 不等待完整下载；超出预算后补发 ARTWORK，屏幕最终更新 | 待执行 |
| 透明 PNG | 白底 JPEG 显示正确，记录转换后尺寸与字节数 | 待执行 |
| 无封面/下载失败 | 旧图被清除，文字继续更新，HomePod 出声与 Apple TV 加入不受影响 | 待执行 |
| 接收端拒绝 | 可由白名单 reason、状态码、尺寸/字节数与操作顺序判断失败层次，诊断不含敏感信息 | 待执行 |
| MA 对照 | 相同曲目、输出关系与操作分别记录成功/失败及屏幕照片，明确 MA 2.10.2 + v0.5.3 | 待执行 |
