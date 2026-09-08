# 给 home agent 的部署与验收交接

## 已确定的用户要求

- 部署在 home 的 **Linux + k3s**。
- 多个 Premium 账号各自保持远程 Connect 会话，Spotify 中显示一个相同名称的播放器。
- 共用一个可在网页切换的 AirPlay 输出；后发起播放的账号直接接管，前一个暂停。
- 首次添加账号改用网页 OAuth 授权并粘贴完整回调地址，后续设备凭据持久化。Spotify 不再使用局域网配对。
- 默认输出为 Apple TV／HomePod 合体目标。用户已经用 Apple 发送端确认：Apple TV 待机时目标仍可见，能播放且电视保持关闭。
- 管理网页只开放在内网／VPN，有管理密码。
- **用户明确要求由 home 上的 agent 最后部署。本地开发没有执行任何 home 写操作。**

## 本地验证记录（2026-09-05）

- `go test -race ./...`、`go vet ./...` 通过。覆盖账号接管、迟到事件、音频隔离、断线恢复及取消恢复、凭据持久化、管理认证，以及模拟引擎的 FIFO／HTTP／WebSocket 交互。
- 服务存在已登录的 SSE 连接时，SIGTERM 能正常关闭并以成功状态退出。
- 前端 8 项测试、TypeScript 检查和 Vite 生产构建通过；网页已经嵌入 Go 二进制。
- 本机浏览器检查了登录、添加账号提示、设置弹窗和桌面／390 px 手机布局；手机无横向溢出，最后检查未发现浏览器错误。
- Linux amd64／arm64 交叉编译通过；`kubectl kustomize deploy/home` 可正常渲染。
- amd64 上游引擎发布物摘要及动态库需求已检查。**本机没有 Docker，尚未执行完整镜像构建或 Linux 容器运行验证。**
- **没有绑定真实 Spotify 账号，也没有向 Apple TV／HomePod 播放。下方实机验收全部待执行。**

## OAuth 改动的本地补充验证（2026-09-05）

- 完整 `go test -race ./...` 和 `go vet ./...` 通过；新增 PKCE、回调来源与 state 校验、重放拒绝、取消／超时、授权期间删除／重新登录、重复账号和凭据恢复测试。
- 前端现在有 10 项测试，全部通过；格式检查、TypeScript 和生产构建通过，Linux amd64／arm64 二进制构建通过。
- 在独立临时数据目录中检查了授权表单、桌面和手机布局、错误回调提示；生成的链接可以打开 Spotify 登录页。没有输入真实 Spotify 凭据，没有完成真实 OAuth 兑换或播放验收。
- 本次仅修改本地工作区，home 部署和实机验收仍由 home agent 完成。

## 部署前读取实际环境

不要使用其他机器遗留的 kubeconfig，不要假定 SSH 地址就是设备所在 LAN 地址。本地曾有名为 `nas-us` 的 kubectl 上下文，不能将其当作 home 集群。

在 home 上确认：

```sh
uname -m
ip -br address
ip route
sudo k3s kubectl get nodes -o wide
sudo k3s kubectl get storageclass
sudo k3s kubectl get pods -A -o wide
ss -lntup
```

记录实际架构（amd64 或 arm64）、唯一音频节点、LAN IP、到 Apple TV 的物理接口和存储类。检查 TCP 8080、UDP 5353、UDP 319/320 是否被其他服务使用。Spotify 登录不再需要 Zeroconf 的动态 TCP 入站端口；5353 和 PTP 仍用于 AirPlay。AirPlay 的媒体／回连还会使用协商端口；hostNetwork 节点必须允许家庭网络中的这些连接。

只给选定的一个节点加 `spoticonn/audio=true` 标签。如果节点存在 PTP 时钟服务，不要停止未知现有服务，先确定端口与时钟方案。

## 构建与装载

仓库已加入 GHCR 发布流程。优先检查 `cyrahs/spoticonn` 的 CI／Publish 是否成功，从 Actions Summary 获取 `ghcr.io/cyrahs/spoticonn` 的多架构 manifest digest，再将 home overlay 的镜像改为该地址并固定 digest。若 package 私有，需要配置 k3s 的拉取凭据。流程与标签见 [CI.md](CI.md)；首次发布尚未执行时，不要假定镜像已经存在。

如果尚无已发布镜像，仍可在 home 本地构建：

```sh
make test
docker build -t spoticonn:0.1.0 .
```

Dockerfile 支持 buildx 的 `linux/amd64` 与 `linux/arm64`。引擎版本和两种架构的 SHA-256 摘要均固定在 `scripts/fetch-engines.sh`。镜像需要构建时访问 npm、Go 模块代理和 GitHub release；运行时不下载引擎。

可推送到用户现有镜像仓库，再修改 `deploy/home/kustomization.yaml`；也可在单节点 home 上导入本地构建的镜像：

```sh
docker save spoticonn:0.1.0 | sudo k3s ctr -n k8s.io images import -
```

没有 Docker 时，使用 home 已有的 OCI 构建工具完成同一 Dockerfile，不需要为本项目改动集群或搭建公共镜像仓库。

首次运行前检查两个二进制能启动，例如 `docker run --rm --entrypoint cliairplay spoticonn:0.1.0 --check`。镜像中包含执行 go-librespot 所需的 ALSA 运行库；ALSA 本身不作为输出设备使用。

## 填写 home 配置

修改 `deploy/home/config.patch.yaml`：

- `SPOTICONN_LISTEN_ADDR`：实际 LAN IP 和未占用端口，如 `192.168.1.10:8080`。默认 loopback 是未部署状态，不能直接照用后宣称网页可从内网访问。
- `SPOTICONN_INTERFACE`：到 Apple TV 所在局域网的接口，不能填 flannel、CNI 或只有 VPN 路由的接口。
- 若存储类不是 `local-path`，在 overlay 中补 PVC patch。保持单副本和 `Recreate`。
- 内网 HTTP 保持 `SPOTICONN_SECURE_COOKIES=false`；若已有内网 HTTPS 入口，则打开 Secure Cookie，代理透传正确的 Host 并支持 SSE。

Pod 使用非 root UID 10001，只附加 `NET_BIND_SERVICE`，AirPlay 子进程在 Linux 上继承该能力以绑定 PTP 端口；不需要 privileged、hostPID、hostIPC 或宿主机 Docker socket。

## 设置管理密码并部署

先创建 namespace 和选定节点标签：

```sh
sudo k3s kubectl apply -f deploy/base/namespace.yaml
sudo k3s kubectl label node ACTUAL_NODE spoticonn/audio=true
```

使用交互式秘密输入生成哈希文件，再创建 Secret。不要把密码或哈希提交到 Git，不要把密码回显到共享日志。`spoticonn hash-password` 从 stdin 读取；可使用已构建二进制，也可 `docker run --rm -i spoticonn:0.1.0 hash-password`。

Secret 格式：

```sh
sudo k3s kubectl -n spoticonn create secret generic spoticonn-admin \
  --from-file=admin-password-hash=/PRIVATE_PATH/admin-password-hash
```

检查渲染结果，再执行部署：

```sh
sudo k3s kubectl kustomize deploy/home
sudo k3s kubectl apply -k deploy/home
sudo k3s kubectl -n spoticonn rollout status deployment/spoticonn
sudo k3s kubectl -n spoticonn logs deployment/spoticonn
```

不要创建公网入口。hostNetwork 的访问约束需要由监听地址和宿主机／网络防火墙实施，不能假定普通 Pod NetworkPolicy 会限制它。

## 第一阶段：真实链路

1. 用户从内网／VPN 打开网页并登录，确认两个引擎都可用。
2. 网页中发现组合设备，记录显示名称、model、稳定 device ID 和成员角色。两成员组合现在从 HomePod 启动音频，再加入 Apple TV；其他拓扑仍走主设备路径。
3. 根据设备要求分别完成成员 PIN 配对，选定组合输出。按 [issue #5 验收表](ISSUE_5_VALIDATION.md) 记录冷启动对照和两种 iOS 控制结果。
4. 网页添加第一个账号，点击「登录 Spotify」，由用户授权后将浏览器中的完整 loopback 回调地址粘贴回管理网页；地址中的 `code` 和 `state` 必须保留，不能写进验收日志。无需在服务器开放 36842 端口，也无需启动临时 Spotify 配对设备。
5. 确认变为常驻会话后，再在 Spotify 选择正式名称播放；记录出声耗时。
6. 确认 Apple TV 待机、电视关闭时仍可出声。必要时检查 Apple TV 的 AirPlay 访问设置和默认输出，不自动改变用户既有家庭音频配置。
7. 添加第二个 Premium 账号，分别在断开家庭 Wi-Fi、未开 VPN 的移动网络上检查各账号的 Connect 设备列表，确认两者可以直接播放。

**如果第 6 或第 7 项失败，应将其报告为兼容性阻塞，保留诊断；不能将“管理界面已部署”或“单账号同网可播”当成目标完成。**

## 功能与稳定性验收

| 检查 | 通过条件 | 实机结果 |
| --- | --- | --- |
| 基础播放 | 播放、暂停、继续、下一首、上一首、seek、音量均正常 | 待执行 |
| iOS 暂停回传（#8） | 按 [控制验收](ISSUE_8_VALIDATION.md) 使用用户确认的 Apple TV 控制中心入口，核对成员回传、Spotify 暂停和 HomePod 静音；原生恢复、切歌、音量分别记录 | 待执行 |
| Apple TV 起播音量（#3） | 冷启动、暂停恢复、断线重连后无需手动调音量即可出声；初始音量 0 全程静音，起播期间调音量或静音保留最新值；Apple TV + HomePod 和独立 HomePod 均通过，并记录 tvOS 版本 | 待执行 |
| 账号接管 | A → B → A，重复至少 20 次，前账号暂停，听不到两路混音和旧缓存串音 | 待执行 |
| 页面关闭 | 关闭网页及手机控制 App，播放继续 | 待执行 |
| 更换输出 | 暂停、连接新目标、继续；失败显示原因并保持暂停 | 待执行 |
| 电视待机 | 合体设备从待机出声，电视保持关闭 | 待执行 |
| 播放连续性 | 连续播放至少两小时，无异常停止、显著漂移或持续内存增长 | 待执行 |
| 空闲常驻 | 空闲 24 小时后，两个账号均可从移动网络发现并播放 | 待执行 |
| 引擎异常 | 单账号进程失败仅影响该会话；输出失败先暂停再重试，不丢凭据 | 待执行 |
| 网络恢复 | 重启目标、短暂断网后可恢复；用户主动暂停可取消自动恢复 | 待执行 |
| Pod 重启 | 设备身份、目标、配对和多个账号保留；不会自行播放 | 待执行 |
| 账号维护 | 删除／重新登录生效，重复授权不会增加第二个同账号会话 | 待执行 |
| OAuth 登录 | 无 Spotify 配对端口放行时完成授权；错误 state、旧链接和重复提交被拒绝，失败可重新登录 | 待执行 |
| OAuth 恢复 | 旧账号升级后直接在线；未完成授权在服务重启后更新链接；成功登录后重启无需再次授权 | 待执行 |
| 权限与日志 | 管理接口拒绝未登录访问；凭据不出现在网页响应或应用日志 | 待执行 |

实机调试可以查看引擎退出原因，但勿直接共享原始凭据文件或未经检查的上游 debug 日志。应用默认不转发上游原始输出；网页显示经过归类的错误。查状态时不要误把 `/healthz` 成功当成扬声器健康。

## 更新、回退和备份

停止播放，先备份 PVC 中整个 `/data`，再更新到明确版本的镜像。`state.json` 存储全局配置和 AirPlay 配对；`accounts/<id>/state.json` 存储各 Spotify 会话。两部分需一起恢复，以保留身份和凭据。

更新采用单副本 Recreate。部署失败可回到上一个镜像并保留原 PVC；不要删除 PVC 来“修复”认证。当前持久化 schema 为版本 1，未知版本会拒绝启动，避免覆盖未来格式。

向用户交付真实管理 URL、镜像版本、节点与存储位置，以及上表的实际结果。长时间验收未完成时明确标记待验收，不补写通过结论。
