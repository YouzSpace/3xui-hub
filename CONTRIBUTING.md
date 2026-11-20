# 贡献指南 / Contributing

ControlHub（仓库 `3xui-hub`）是 Go + Vue 3 的代理运营面板。提交前读完这一页，能省掉大部分来回沟通。

Thanks for improving ControlHub. This page is the short version; the long version (component tiers, composables, splitting big files, anti-patterns) is in [docs/CODE-STYLE.md](docs/CODE-STYLE.md).

## 环境 / Requirements

| 组件 / Tool | 版本 / Version | 说明 / Notes |
|---|---|---|
| Go | 1.25+ | `server/` 与 `agent/`（两个独立 module）/ backend + agent |
| Node.js | 20.19+ | 前端 `frontend/` / frontend |
| MySQL | 8+ | 只有本地跑起服务时需要 / only for running locally |
| GNU make | 3.81+ | 统一构建入口，所有命令都在 Makefile 里 / build entry point |
| golangci-lint | v2 | 可选，`make lint` 用到（见下文「lint 现状」）/ optional |

装依赖（起服务、默认账号、Docker 部署见 README，这里不重复）：

```bash
cd server && go mod download
cd agent && go mod download     # 独立 module，别漏
cd frontend && npm ci
```

`make build-frontend` 在 `frontend/node_modules` 缺失时会自动 `npm ci`。

## 提交前必须通过 / Must pass before submitting

一条不过就不要提交「先合再说」。

```bash
make verify                  # build（前端 + 后端 + agent）+ test（后端 + agent + 前端）+ typecheck
cd server && go vet ./...    # vet 目前没并进 make verify，单独跑
```

等价的逐项命令（CI 与本机一致）：

```bash
# 后端（真构建带 -tags=embed，前端产物靠它编进二进制）
cd server && go build -tags=embed ./... && go vet ./... && go test ./...

# agent（独立 module，单独 build / test）
cd agent && go build ./... && go test ./...

# 前端
cd frontend && npm run build && npx vitest run && npm run typecheck
```

`make build` 的顺序不能反：前端产物先落到 `server/internal/web/dist`，后端才能用 `-tags=embed` 把它编进二进制。

**lint 现状**：`make lint` 目前是红的——存量代码还有 `unused` / `errcheck` 告警（Makefile 里有注明，不阻塞 build / test）。所以提交门槛是 `make verify` + `go vet`，不是 lint 全绿；但新增代码不要引入新的 lint 告警。

## 代码规范 / Code Conventions

细则见 [docs/CODE-STYLE.md](docs/CODE-STYLE.md)。这里是要点。

### Go

- 单文件 ≤800 行，超了按职责拆文件。参照 `server/internal/handler/admin/node_*.go`：crud / domain / hop / models / ops / present / region / sysctl / validate 各一个文件，存储层再按域拆成 `node_*_store.go`。
- 函数 ≤4 个参数，超了用 options struct：`collector.Options`、`task.QueueOptions`、`api.PaymentServiceOptions` 是现成例子。
- 错误必须包装：`fmt.Errorf("xxx: %w", err)`。禁止用 `%v` 包装 `err` 把链截断（存量代码里还有一批，改到哪个文件顺手改掉）。
- SQL 必须参数化，占位符 `?`。表名 / 列名只允许是代码内白名单常量（`internal/task/retention.go`、`internal/nodeapi/repo.go` 里的 `%s` 属于这一类），**任何用户输入都不许拼进 SQL 字符串**。
- 建表用 `CREATE TABLE IF NOT EXISTS`，加列用 `ensureColumn`（都在 `server/cmd/server/schema.go`），禁止 `DROP TABLE` / `DROP COLUMN`：老用户升级必须全自动，不能让人跑手工 SQL。
- 新功能必须带测试。后端已有 70 多个 `_test.go`；新代码至少覆盖到所在包现有水平，别让新包零测试。
- 可调参数（超时、重试上限、阈值、批量大小）走环境变量 / config，不硬编码。

### Vue

- 单文件 ≤500 行，超了抽子组件。`views/admin/Dashboard.vue` 里拆出 `components/admin/TrafficPanel.vue` 就是这么做的。
- 组件分四类，放对目录：
  - Base：`components/ui/`（Button / Input / Modal / Icon … 原子件，不含业务）
  - Feature：`views/`（一个路由页一个文件，负责取数 + 组合）
  - Layout：`layouts/`（`AdminLayout.vue` / `UserLayout.vue`）
  - Presentational：`components/admin/`、`components/user/`（有业务含义、只吃 props 的展示件）
- 逻辑复用走 `composables/`，不要复制粘贴 loading / 分页 / confirm。后台列表页统一用 `composables/usePagedList.js`（页码走 `?page=`、翻页 abort 上一个请求、深页走 cursor）。
- 新代码必须写 JSDoc 类型注释（不强制 TS，但类型要标）：

  ```js
  /**
   * @param {number} planId
   * @param {number} quantity
   * @returns {Promise<{created: Array, failed: Array}>}
   */
  ```

- 图标统一用 `components/ui/Icon.vue`（内联 SVG，`currentColor`），禁止 emoji 当图标。
- 颜色只用 token：`var(--*)`，定义在 `frontend/src/assets/styles/base/tokens.css`，禁止硬编码 hex。
- 新页面必须响应式：<768px 侧栏变抽屉、表格横向滚动。

### 数据库

- 金额一律 `BIGINT` 存「分」，禁止浮点（参考 `server/cmd/server/schema.go` 里的 `amount bigint ... COMMENT '...(分)'`）。
- 余额扣减必须 `SELECT ... FOR UPDATE` + 事务，不要「先查后写」。
- 支付回调必须幂等：先查订单 status，已处理直接返回成功（`internal/handler/api/payment_balance.go` 的幂等闸门就是按订单 status 做的）。
- 加表 / 加列只走 `ensureSchema` / `ensureColumn`，删表删列不行。

## 接口返回 / API Envelope

```json
成功：{"code":0,"data":...}
失败：{"code":非0,"msg":"...","data":null}   // HTTP 状态默认仍是 200
```

前端按这套结构解，不要为新接口另发明形状。成功体常量 `{"code":0,"data":...}` 是不带 `msg` 的，别顺手加。

## PR 规范 / Pull Requests

1. 一个 PR 只做一件事：结构改动和行为改动分开提，别在同一个 PR 里顺手重排无关代码、升级依赖。
2. 标题 `type(scope): 说明`，type 用 `feat` / `fix` / `perf` / `refactor` / `docs` / `test` / `chore`。
3. 描述必须写清三件事：**改了什么 / 影响范围 / 怎么验证的**（贴真实命令和输出，不要「看起来对」）。
4. 界面改动附一张截图。
5. 不提交：`node_modules/`、构建产物（`frontend/dist`、`server/internal/web/dist`）、`.env`、`server/storage/`、日志、数据库备份、任何密钥。

Keep a PR to one thing, use `type(scope): summary` titles, and state what changed / what it affects / how it was verified with real output. Include a screenshot for UI changes.

## 分支 / Branches

- 从 `main` 切：`feat/xxx`、`fix/xxx`、`refactor/xxx`。
- 不直接推 `main`，走 PR。

## 版本号 / Versioning

- bug 修复 → patch +1
- 新功能 → minor +1，patch 清零
- 重构 / 不兼容变更 → major +1，patch 清零

版本号在 `frontend/package.json`、`README.md` 与 Release tag（`vX.Y.Z`）三处保持一致；打 tag 会触发 CI 编译并发布二进制。

The version must match in `frontend/package.json`, `README.md` and the `vX.Y.Z` tag; tagging triggers CI builds.

## 行尾 / Line Endings

见根目录 `.gitattributes`：仓库里统一存 LF；`.sh` / `.bash` 强制 LF（要在 Linux 上跑）；`.bat` / `.cmd` 用 CRLF；图片、字体、压缩包等按二进制处理不做转换。

Windows 上 `core.autocrlf=true` 会把工作区文件转成 CRLF，这不影响仓库内容。不要手动改行尾，也不要跑 `git add --renormalize .`（会产生几千行无意义 diff）。

## 安全问题 / Security

不要用公开 issue 报漏洞，走 [SECURITY.md](SECURITY.md) 的私密渠道。

Report vulnerabilities privately — see [SECURITY.md](SECURITY.md).
