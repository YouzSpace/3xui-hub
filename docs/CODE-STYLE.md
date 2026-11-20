# 代码规范细则 / Code Style

[CONTRIBUTING.md](../CONTRIBUTING.md) 只列要点，这一页说判断标准：什么时候该抽、什么情况不该抽、大文件怎么拆、哪些写法一律退回。

规范按「原则」写，不按某一天的行数写。第 3 节的现状数字是 2026-11-17 20:24 的快照，会随拆分变化。

## 1. Vue 组件分四类：判断标准

| 类型 | 目录 | 判断标准 | 现有例子 |
|---|---|---|---|
| Base | `frontend/src/components/ui/` | 与业务无关，任何页面都能用；只吃 props、只吐事件，不发请求、不认识 store | `Button.vue`、`Input.vue`、`Modal.vue`、`Pagination.vue`、`Icon.vue`、`Skeleton.vue` |
| Layout | `frontend/src/layouts/` | 页面骨架 + 侧栏 / 顶栏，只负责插槽和导航状态 | `AdminLayout.vue`、`UserLayout.vue` |
| Feature（页面） | `frontend/src/views/` | 一个路由一个文件；负责取数、路由参数、把子组件拼起来 | `views/admin/AuditLogs.vue`、`views/admin/Users.vue` |
| Presentational | `frontend/src/components/admin/`、`components/user/` | 带业务名词、但只吃 props 的展示件；不知道数据从哪来 | `components/admin/TrafficPanel.vue`、`NodeMonitor.vue`、`NodeGlobe.vue` |

判断顺序（照着问，不用犹豫）：

1. 这段模板会在**两个以上**页面出现吗？会 → 抽（`ui/` 若与业务无关，`components/admin|user/` 若有业务名词）。
2. 只出现在当前页面，但页面已经超 500 行吗？是 → 抽成 Presentational。
3. 组件里要发请求吗？要 → 留在 `views/`，或把取数抽进 composable；Base 组件里不许有 `api.*` 调用。
4. **抽出来要传 8、10 个 props 才能渲染**吗？是 → 先别抽，多半是数据没整理好；props 超过 6 个基本说明边界错了。

## 2. composables 的写法

位置 `frontend/src/composables/`，命名 `useXxx.js`。返回状态和方法，不返回组件；不碰 DOM。

**什么时候该写新的**：

- 同一段逻辑在 ≥2 个页面出现（loading + error、翻页、confirm 弹窗、表单校验、轮询、复制到剪贴板）；
- 或者逻辑单独拿出来能写单元测试（`frontend/src/composables/__tests__/`）。

**范本**：`composables/usePagedList.js`（7 个后台列表页在用）。

它的契约写在文件头的 JSDoc 里，用的时候照这个形状：

```js
const { page, loading, loadError, pagination, hasPrev, hasNext, prev, next, fetchPage } =
  usePagedList()

const load = async () => {
  // 第二个参数是附加查询条件（search / 筛选），会并进 params
  const res = await fetchPage((params, config) => adminApi.auditLogs({ ...params, ...extra }), null)
  if (!res) return      // 被 abort 或出错时返回 null —— 不要当空数组用
  rows.value = res      // data 是数组，分页信息挂在数组的 .pagination 上
}
```

它已经处理掉、你别再重复实现的事：

- 页码走路由 `?page=N`，刷新和切页回来不丢页；
- 翻页前 `abort()` 上一个未完成请求（老响应不会覆盖新页）；
- 深页带 `cursor`（后端按主键定位，不付 OFFSET 扫全表的代价）；
- 越界页码（`?page=999`）由后端夹回有效页，这里把地址栏校正到实际页；
- keep-alive 退场（`onDeactivated`）期间不拉数据、不改 URL。

写法要求：

- 文件头必须有 JSDoc 块，说清**为什么存在 / 约定什么 / 有什么坑**（usePagedList 里那两段长注释就是标准：解释「为什么不能在退场时改 URL」这类原因，而不是复述代码）；
- 对外方法要有 `@param` / `@returns`；
- 一个 composable 只解决一件事，别做「万能 usePage」。

**同期新增的**（同一分支上补的，见 `composables/` 与 `composables/__tests__/`）：`useAsyncData.js`、`useConfirm.js`、`useCrudForm.js`。列表页的 loading / 分页、删除前确认、新增编辑表单，先看这里有没有现成的，别复制粘贴。

## 3. 巨型文件的拆分判据

阈值：**Go 单文件 ≤800 行、Vue 单文件 ≤500 行**（`.vue` 按脚本 + 模板 + 样式合计）。

拆的流程（顺序别颠倒）：

1. **按职责列清单**，不按行数切。典型切法：取数 / 校验 / 渲染 / 写库 / 后台任务分发。
2. **先搬不改**：纯移动 + 改包内引用，一个提交只做搬运。搬运和改逻辑混在一起，review 等于没有。
3. **收窄交接面**：拆出去的存储层只暴露 struct 方法，内部 SQL 不出文件；handler 只暴露 HTTP 入口。
4. **每搬一块就跑一次** `cd server && go build ./... && go test ./...`（前端 `npx vitest run`）。一口气搬完再编，报错定位成本翻十倍。
5. 顺手把「搬到的这块」的同类问题按 CONTRIBUTING 的要求带上（`%v` → `%w` 等），别扩大范围。

真实参照：`server/internal/handler/admin/` 里节点相关代码就是按职责分的——`node_crud.go`（增删改）、`node_validate.go`（入参校验）、`node_present.go`（响应整形）、`node_domain.go`、`node_region.go`、`node_hop.go`、`node_sysctl.go`、`node_ops.go` 各管一件事；存储层正在按域继续拆成 `node_*_store.go`。

**现状快照（2026-11-17 20:24）**——超阈值的文件，不在本次改动范围，只是排队：

| 文件 | 行数 |
|---|---|
| `frontend/src/views/admin/XrayInbounds.vue` | 2430 |
| `server/internal/task/commands.go` | 2353 |
| `server/internal/handler/admin/xray_inbound.go` | 2294 |
| `server/internal/handler/admin/sqlstore.go` | 2142 |
| `frontend/src/views/admin/XrayOutbounds.vue` | 1514 |
| `server/internal/handler/admin/xray_routing.go` | 1477 |
| `server/internal/collector/reconcile.go` | 1465 |
| `frontend/src/views/admin/Email.vue` | 1411 |
| `server/internal/handler/admin/email.go` | 1151 |
| `frontend/src/views/admin/Nodes.vue` | 994 |

合计：Go 有 20 / 353 个文件超 800 行，Vue 有 18 / 93 个文件超 500 行。

**例外**（允许不拆，但文件头要写一句为什么）：表结构定义（`server/cmd/server/schema.go`）、路由注册表、纯常量/映射表。

## 4. 常见反模式（见到就退回）

1. 复制粘贴 loading / 分页 / confirm —— 走 `composables/`。
2. `fmt.Errorf("xxx: %v", err)` 把错误链截断 —— 用 `%w`（2026-11-17 扫描存量 11 处）。
3. 先查余额再 `UPDATE`，不带锁 —— `SELECT ... FOR UPDATE` + 事务。
4. 支付回调不判订单 status 就发货 —— 先查 status，已处理直接返回成功（重复回调是常态，不是异常）。
5. 用 `fmt.Sprintf` 把用户输入拼进 SQL —— 参数化，只允许白名单表名列名。
6. 用 `DROP COLUMN` / 改列类型来「升级」—— 用 `ensureColumn` 加列，读的时候兼容老数据；老用户升级必须全自动。
7. 金额用 `float64` 或存「元」—— `BIGINT` 存分（`amount bigint ... (分)`）。
8. 前端硬编码颜色 `#0f172a` —— 用 token `var(--*)`，定义在 `frontend/src/assets/styles/base/tokens.css`（2026-11-17 扫描存量 461 处）。
9. emoji 当图标 —— 用 `components/ui/Icon.vue`（内联 SVG、`currentColor`）。
10. 新接口自己发明 `{code: 200, result: ...}` —— 统一 `{"code":0,"data":...}` / `{"code":非0,"msg":"...","data":null}`（HTTP 仍 200）。
