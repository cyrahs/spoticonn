# CI、PR gate 与 GHCR 发布

仓库为 `cyrahs/spoticonn`，镜像地址为 `ghcr.io/cyrahs/spoticonn`。

## 触发与模块

[ci.yml](../.github/workflows/ci.yml) 在所有 PR、merge queue、推送到 `main`、推送 `v*` 标签以及手动运行时执行。没有 workflow 级别的路径过滤；仅修改文档也会产生 `CI gate` 检查。

| 检查 | 内容 |
| --- | --- |
| Workflow and deployment configuration | actionlint、ShellCheck、gate／版本标签测试、Kustomize 渲染和 Kubernetes schema 校验 |
| Go formatting and static analysis | gofmt、go vet、go.mod／go.sum 一致性 |
| Go tests / 模块名 | store、audio、spotify、airplay、bridge、httpapi、service 七个独立 race test job |
| Frontend | Prettier、Vitest、TypeScript 和 Vite 生产构建 |
| Linux binary / 架构 | 嵌入前端产物后编译 amd64、arm64 二进制 |
| Container / 架构 | 在原生 amd64、arm64 runner 构建镜像，检查两个引擎、非 root 服务启动、健康检查和正常退出 |
| **CI gate** | 汇总上述全部 job；任何失败、取消、跳过或缺失结果都会失败 |

矩阵使用 `fail-fast: false`，可以同时看到各模块／架构的结果。PR 新提交会取消旧运行；`main` 推送按 ref 排队，以免旧运行在新运行之后覆盖滚动标签。Go 应用按 1.25 测试，配置校验工具使用 1.26；前端与 Dockerfile 一致使用 Node 22。Actions 均固定到已核对的提交 SHA，旁边注明对应版本。

## 把 CI gate 设为 PR 必需检查

首次将项目推送到 `main` 并产生 Actions 检查记录后，在 GitHub 的仓库 Rulesets／Branch protection 中：

1. 为 `main` 启用要求通过 PR 合并的规则。
2. 添加必需状态检查 **`CI gate`**，来源为 GitHub Actions，所属 workflow 为 **CI**。
3. 启用合并前分支必须更新，或者使用已配置好的 merge queue。

只需将 `CI gate` 作为必需状态检查，内部模块增加或改名时不用同步修改分支保护；增加新的顶层检查 job 时，需要同时加入 gate 的 `needs` 和命令参数。修改矩阵里的模块无需改变 gate 名称。

**workflow 文件本身不会自动修改 GitHub 分支保护规则。** 本次创建的是可被规则引用的固定检查；仓库当时为空，尚无首次运行记录。

gate 使用 `always()`，随后显式验证所有依赖必须为 `success`，避免依赖失败造成汇总 job 被跳过而失去拦截作用。merge queue 有单独触发事件。参见 [GitHub 的 job 依赖说明](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#jobsjob_idneeds)。

## 发布规则

[publish.yml](../.github/workflows/publish.yml) 是由 CI 调用的 reusable workflow，只有本仓库 `main` 或有效版本标签的 **push** 且 `CI gate` 成功后才会执行。PR、fork、merge queue 和手动 CI 检查不发布镜像。

CI 在两种原生架构上完成构建和启动检查，再把已测试的镜像归档。发布阶段下载同一次运行的归档，推送两个架构镜像，按 digest 合成 manifest 并确认包含 `linux/amd64`、`linux/arm64`。发布阶段不重新构建镜像。

| Git 引用 | 多架构镜像标签 |
| --- | --- |
| `main` | `main`、`latest`、`sha-<完整提交 SHA>` |
| `v1.2.3` | `1.2.3`、`1.2`、`sha-<完整提交 SHA>` |
| `v1.2.3-rc.1` | `1.2.3-rc.1`、`sha-<完整提交 SHA>` |

另有 `sha-<完整 SHA>-amd64` 和 `sha-<完整 SHA>-arm64` 两个发布过程使用的架构标签。`latest` 跟随 `main`，版本发布不修改它。只接受 `v` 开头的完整 SemVer 标签；`v1`、`vlatest` 等会在 CI 中失败。

使用内置 `GITHUB_TOKEN` 登录 GHCR，仅发布 job 具有 `packages: write`，无需另建 PAT。镜像包含指向本仓库的 OCI source 和 revision 标签。GitHub 的认证方式见 [发布容器镜像文档](https://docs.github.com/en/actions/tutorials/publish-packages/publish-docker-images#publishing-images-to-github-packages)。

发布成功后，在 Actions 的 Summary 中查看镜像标签和 manifest digest。home 部署建议固定到该 digest，避免滚动标签配合 `IfNotPresent` 使用旧镜像。首次发布后检查 GHCR package 的可见性：若保持私有，home 的 k3s 需要对应的只读拉取凭据；公开仓库不代表 package 已自动公开。

镜像及 digest 归档保留三天，Linux 二进制保留七天。如果归档过期，重新运行完整 CI 后再发布；不要仅重跑一个依赖过期归档的发布 job。

## 本地验证与边界

```sh
python3 -m unittest discover -s scripts/ci -p 'test_*.py' -v
actionlint
shellcheck scripts/fetch-engines.sh scripts/ci/*.sh
cd web && npm run format:check
```

`actionlint` 固定为 v1.7.12，`kubeconform` 为 v0.8.0。有 Docker 时，可用 `bash scripts/ci/smoke-image.sh spoticonn:0.1.0` 检查本地镜像。

容器启动检查不连接 Spotify 或 AirPlay 设备，不能代替 [home 实机验收](HOME_HANDOFF.md)。本机没有 Docker；首次 GitHub Actions 运行仍需验证 runner、完整镜像构建和 GHCR 权限。
