# 贡献指南 / Contributing

感谢你愿意改进 ControlHub（企业级跨境电商网络服务运营平台）。提交前读完这一页，能省掉大部分来回沟通。

Thanks for improving ControlHub. Reading this page first saves most of the back-and-forth.

## 环境要求 / Requirements

| 组件 / Tool | 版本 / Version | 说明 / Notes |
|---|---|---|
| Go | 1.25+ | 后端 `server/` / backend |
| Node.js | 20.19+ | 前端 `frontend/` / frontend |
| MySQL | 8+ | 只有本地跑起服务时需要 / only for running locally |
| golangci-lint | v2 | 可选，`make lint` 用到 / optional |
| GNU make | 3.81+ | 统一构建入口 / build entry point |

## 常用命令 / Commands

```bash
make help            # 列出所有目标
make build           # 先构建前端（产物进 server/internal/web/dist），再编译内嵌前的后端
make build-frontend  # 只构建前端
make build-backend   # 只编译后端（-tags=embed，前端产物编进二进制）
make test            # 后端 go test + 前端 vitest
make typecheck       # 前端 vue-tsc 类型检查
make verify          # build + test + typecheck，提交前跑这个
make lint            # golangci-lint run（需本机安装）
```

`make build` 的顺序不能反：前端产物要先进 `server/internal/web/dist`，后端才能用 `-tags=embed` 把它编进二进制。首次 `make build-frontend` 会自动 `npm ci`（`node_modules` 缺失时）。

`make build` order matters: the SPA is built into `server/internal/web/dist` first, then the backend embeds it via `-tags=embed`. Frontend deps are installed automatically (`npm ci`) when `node_modules` is missing.

## 本地起服务 / Run Locally

```bash
# 1. 配置：从模板拷一份，补 APP_KEY 和数据库
cp .env.example .env
# APP_KEY 必须是 base64: 加 32 字节密钥，例如：
#   APP_KEY=base64:$(openssl rand -base64 32)

# 2. 后端（默认 :8080，可用 LISTEN_ADDR 改）
cd server && go run ./cmd/server

# 3. 前端开发服务器（5173，/api 与 /admin-api 代理到 8080）
cd frontend && npm run dev
```

后端启动时必须能读到 `.env`（默认相对 `server/` 目录查找 `../.env`，可用 `CONFIG_ENV` 指定其他路径），否则会报 `APP_KEY 无效`。

The backend reads `.env` (lookup is relative to `server/` → `../.env`, override with `CONFIG_ENV`). Without a valid `APP_KEY` it refuses to start.

## 提交改动 / Pull Requests

1. Fork 后从 `main` 切分支：`fix/xxx`、`feat/xxx`。
2. 改动保持最小：不顺手重排无关代码、不引新依赖、不动与本议题无关的文件。
3. 提交前跑 `make verify`。测试挂了不要提交「先合再说」。
4. PR 说明写清三件事：**改了什么 / 影响范围 / 怎么验证的**（贴真实输出，不要「看起来对」）。
5. 涉及界面改动的，附一张截图。

Keep changes minimal, run `make verify` before pushing, and describe what changed, what it affects, and how you verified it. Include a screenshot for UI changes.

## 代码约定 / Conventions

- 提交信息用 `feat:` / `fix:` / `docs:` / `refactor:` / `perf:` / `test:` / `chore:` 前缀，正文用中文或英文都可以，说清动机。
- 后端：新增数据库表必须写进 `server/` 的幂等建表逻辑（`ensureSchema`），老用户升级不跑手工 SQL；可调参数走环境变量，不硬编码超时和阈值。
- 前端：Vue 3 组合式 API + `<script setup>`，与现有组件的写法保持一致。
- 接口返回统一 `{code, data}`，错误也用同一结构，不要给前端造例外。
- 不要提交：`node_modules/`、构建产物、`.env`、`server/storage/`、数据库备份、任何密钥。

Backend: new tables go into idempotent `ensureSchema`, tunables come from env. Frontend: Vue 3 `<script setup>`. API responses are `{code, data}`. Never commit secrets, build artifacts or `node_modules/`.

## 版本号 / Versioning

- bug 修复 → patch +1
- 新功能 → minor +1，patch 清零
- 重构 / 不兼容变更 → major +1，patch 清零

版本号在 `frontend/package.json`、`README.md` 与 Release tag（`vX.Y.Z`）三处保持一致；打 tag 会触发 CI 编译并发布二进制。

The version must match in `frontend/package.json`, `README.md` and the `vX.Y.Z` tag; tagging triggers CI builds.

## 安全问题 / Security

不要用公开 issue 报漏洞，走 [SECURITY.md](SECURITY.md) 的私密渠道。

Report vulnerabilities privately — see [SECURITY.md](SECURITY.md).
