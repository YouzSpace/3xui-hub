# CLAUDE.md —— 给 AI 编码助手的仓库说明

开工前读完这一页：结构、命令、硬规矩。人类的贡献流程见 [CONTRIBUTING.md](CONTRIBUTING.md)，
细则（组件分四类、composables、大文件拆分判据、反模式清单）见 [docs/CODE-STYLE.md](docs/CODE-STYLE.md)。

## 一句话 / What this is

ControlHub（发布仓 `3xui-hub`）：自托管的代理运营面板 —— Go 后端 + Vue 3 SPA + 裸内核节点 agent + 一键安装脚本。

## 目录 / Layout

| 路径 | 内容 |
|---|---|
| `server/` | Go 后端（`cmd/server` 入口，`internal/` 按领域分包，`schema/` 建表 SQL） |
| `server/internal/web/` | 前端静态站点：`-tags=embed` 内嵌产物 / 未打 tag 时读磁盘 |
| `frontend/` | Vue 3 + Vite，产物落 `server/internal/web/dist`（不入 git） |
| `agent/` | 节点 agent，独立 module、独立版本号（仅 `agent/` 改动才 bump） |
| `assets/` | 面板分发的节点资产：安装脚本、`manifest.json`（sha256 权威）、geo 清单 |
| `install.sh` / `uninstall.sh` / `3hub` | 服务器侧安装、卸载、运维命令 |
| `docker/` | 单容器部署（Dockerfile / compose / nginx / supervisord） |
| `.github/workflows/release.yml` | 打 tag 后 CI 交叉编译并发 Release |

`server` 与 `agent` 是两个独立 Go module，各自 build / test，不要混。

## 命令 / Commands

```bash
make help            # 所有目标
make build           # 前端 → 后端(-tags=embed) → agent，顺序不能反
make build-frontend
make build-backend
make build-agent
make test            # 后端 + agent + 前端单测
make typecheck       # 前端 vue-tsc
make verify          # build + test + typecheck，提交门槛
make lint            # golangci-lint（存量有告警，会红，不阻塞）
```

`make verify` 之外还要单独跑 `cd server && go vet ./...`（vet 没并进 verify）。
前端产物是后端 embed 的输入，先有 `server/internal/web/dist` 才能编出单文件二进制。

## 硬规矩 / Rules

1. **先读代码再改**：不要凭印象改符号名、字段名、返回结构。
2. **最小改动**：不顺手重构、不重排格式、不加没被要求的功能与依赖。
3. **改完必须真跑**：`make verify`，不接受「看起来对」。涉及哪个包就跑哪个包的测试。
4. **数据库变更必须幂等**：新表进 `ensureSchema`、新列走 `ensureColumn`，禁止 `DROP`；
   老用户升级全自动，不能让人跑手工 SQL。
5. **可调参数走环境变量 / config**（超时、重试上限、阈值、批大小），不硬编码。
6. **API 统一信封**：`{"code":0,"data":...}` / `{"code":非0,"msg":"...","data":null}`，HTTP 仍 200。
7. **不手改构建产物**：`server/internal/web/dist` 由 `make build-frontend` 生成，改前端改 `frontend/src`。
8. **前端文案面向普通用户**：不讲技术栈、服务器、数据库细节。
9. **二进制资产不进 git**：xray 内核 zip、节点 agent 二进制只放上游 / 本项目 Release；
   面板本地缺文件时按 `assets/manifest.json` 的 `url` 下载 + sha256 复验 + 落盘缓存
   （实现见 `server/internal/nodeapi/assets.go`）。sha256 权威永远在 manifest。

## 代码规范要点 / Code style

### Go

- 单文件 ≤800 行，超了按职责拆（参照 `server/internal/handler/admin/node_*.go`）。
- 函数 ≤4 个参数，超了用 options struct。
- 错误必须 `fmt.Errorf("xxx: %w", err)` 包装，不用 `%v` 截断错误链。
- SQL 必须参数化（`?`）；表名列名只能是代码内白名单常量，用户输入不许拼进 SQL。
- 新代码带测试：至少覆盖到所在包现有水平，别让新包零测试。

### Vue

- 单文件 ≤500 行，超了抽子组件。
- 组件分四类放对目录：Base `components/ui/`、Layout `layouts/`、Feature `views/`、
  Presentational `components/admin|user/`；Base 组件不发请求。
- 复用逻辑走 `composables/`（列表页统一用 `usePagedList.js`），不复制粘贴 loading / 分页 / confirm。
- 新代码写 JSDoc 类型注释；图标用 `components/ui/Icon.vue`（禁 emoji）；颜色只用 `var(--*)` token。
- 新页面必须响应式（<768px 侧栏变抽屉、表格横向滚动）。

### 数据库

- 金额一律 `BIGINT` 存「分」，禁浮点。
- 余额扣减 `SELECT ... FOR UPDATE` + 事务，不要「先查后写」。
- 支付回调必须幂等（先查订单 status，已处理直接返回成功）。

## 行尾 / Line endings

见根目录 `.gitattributes`：仓库里统一存 LF；`*.sh` / `*.bash` 强制 LF（在 Linux 上跑）；
`*.bat` / `*.cmd` / `*.ps1` 强制 CRLF；图片、字体、zip 等按 binary 不做转换。
Windows 上 `core.autocrlf=true` 把工作区转成 CRLF 不影响仓库内容；不要手动改行尾，
也不要跑 `git add --renormalize .`（会产生几千行无意义 diff）。

## 红线 / Red lines

- 不提交 `.env`、密钥、证书、数据库备份、`server/storage/`、`node_modules/`、构建产物。
- 不在仓库里写死生产地址、真实凭据、用户数据。
- 不改 CI 发布流程到影响产物可用性；确需改动时说明影响。
- 不 force push、不改 remote、不动 git 历史。
- 不自动部署到服务器（部署由维护者执行）。
- 安全问题不走公开 issue，按 [SECURITY.md](SECURITY.md) 报。

## 版本号 / Versioning

bug 修复 → patch +1；新功能 → minor +1 且 patch 清零；重构 / 不兼容 → major +1 且 patch 清零。
版本号三处对齐：`frontend/package.json`、`README.md`、Release tag `vX.Y.Z`（打 tag 触发 CI 出二进制）。

## 交付时说什么 / What to report

改了什么 → 影响范围 → 实测输出（真实命令与结果）→ 还剩什么没做。不要贴大段计划，也不要贴没跑过的结论。
