# CLAUDE.md —— 给 AI 编码助手的仓库说明 / Notes for AI coding agents

本文件描述这个仓库的结构、命令和硬规矩。开工前读完，能省掉一轮返工。
Read this before changing code: it lists the layout, the commands, and the rules that are not negotiable.

## 一句话 / What this repo is

ControlHub：企业级跨境电商网络服务运营平台。Go 后端 + Vue 3 前端 + 裸内核节点 agent + 一键安装脚本，面向面板运维者自建部署。
ControlHub is a self-hosted operations platform: Go backend, Vue 3 SPA, node agent for bare-kernel nodes, and a one-line installer.

## 目录 / Layout

| 路径 | 内容 |
|---|---|
| `server/` | Go 后端（`cmd/server` 入口，`internal/` 按领域分包；`schema/` 建表 SQL） |
| `server/internal/web/` | 前端静态站点服务；`embed_on.go`（`-tags=embed` 内嵌产物）/ `embed_off.go`（本地读磁盘） |
| `frontend/` | Vue 3 + Vite 单页应用，构建产物落 `server/internal/web/dist` |
| `agent/` | 节点 agent（独立版本号，独立 `go.mod`），源码变更时才 bump 版本 |
| `install.sh` / `uninstall.sh` / `3hub` | 服务器侧安装、卸载、运维命令 |
| `docker/` | 单容器部署（Dockerfile / compose / nginx / supervisord） |
| `backend/` | 历史遗留目录：运行时 `.env` 模板、`storage/` 资产与 `manifest.json` |
| `.github/workflows/release.yml` | 打 tag 后 CI 交叉编译并发布二进制 |

`server` 与 `agent` 是两个独立 Go module，各自 `go build`，不要混。
`server` and `agent` are separate Go modules — build them separately.

## 命令 / Commands

```bash
make help            # 所有目标
make build           # 前端 → 后端（顺序不能反，后端要 embed 前端产物）
make build-frontend
make build-backend   # -tags=embed
make test            # go test + vitest
make typecheck       # 前端 vue-tsc
make verify          # build + test + typecheck：提交前必跑
make lint            # golangci-lint（可选依赖）
```

本地起服务见 [CONTRIBUTING.md](CONTRIBUTING.md)；部署见 [INSTALL.md](INSTALL.md) 与 `docker/`。

## 硬规矩 / Rules

1. **先读代码再改**。改动前把相关实现读通；不要凭印象改符号名、字段名或返回结构。
2. **最小改动**。不顺手重构、不重排格式、不加没被要求的功能与依赖。
3. **改完必须真跑**：`make verify`（或至少 `make build` + 相关测试）。不接受「看起来对」。
4. **数据库变更必须幂等**：新表写进 `server/` 的 `ensureSchema`，老用户升级不跑手工 SQL。
5. **可调参数走环境变量**（超时、重试、阈值），不要硬编码。
6. **API 统一返回 `{code, data}`**，前端 axios 依赖这个结构。
7. **不要手改构建产物**：`server/internal/web/dist` 由 `make build-frontend` 生成，改前端要改 `frontend/src`。
8. **前端文案面向普通用户**：不讲技术栈、数据库、服务器细节。

Minimal diffs, read before writing, always verify with `make verify`, idempotent schema changes, tunables from env, `{code, data}` responses, never hand-edit build output.

## 红线 / Red lines

- 不提交 `.env`、密钥、证书私钥、数据库备份、`server/storage/`、`node_modules/`、构建产物。
- 不在仓库里写死生产地址、真实凭据、用户数据。
- 不改 CI 发布流程（`.github/workflows/`）到会影响产物可用性；确需改动时说明影响。
- 不 force push、不改 remote、不动 git 历史。
- 不自动部署到别人的服务器；部署动作由维护者执行。
- 安全问题不走公开 issue，按 [SECURITY.md](SECURITY.md) 报。

Never commit secrets or build output, never rewrite history or remotes, never deploy to someone else's server.

## 版本 / Versioning

bug → patch+1；新功能 → minor+1 且 patch 清零；重构 / 不兼容 → major+1 且 patch 清零。版本号三处对齐：`frontend/package.json`、`README.md`、Release tag `vX.Y.Z`。agent 版本独立，仅 `agent/` 源码变更时 bump。

## 交付时说什么 / What to report back

改了什么 → 影响范围 → 实测输出（真实命令与结果）→ 还剩什么没做。不要贴大段计划，也不要贴没跑过的结论。

Report: what changed, what it affects, the real command output that proves it, and what is left.

---

# English (short version)

ControlHub is a self-hosted platform: Go backend (`server/`), Vue 3 SPA (`frontend/`, built into `server/internal/web/dist` and embedded with `-tags=embed`), a node agent (`agent/`, its own module and version), plus `install.sh` / `uninstall.sh` / `3hub` and `docker/`.

Commands: `make help|build|build-frontend|build-backend|test|typecheck|verify|lint`. Build order matters — the SPA must be built before the backend embeds it. Run `make verify` before handing work back.

Rules: read the code before editing, keep diffs minimal, verify with real output, make schema changes idempotent (`ensureSchema`), read tunables from env, keep API responses `{code, data}`, never hand-edit build artifacts, and keep user-facing copy free of stack details.

Red lines: no secrets, no build output, no `.env`, no force push, no remote changes, no deploying to someone else's server, no public disclosure of security issues (see [SECURITY.md](SECURITY.md)).
