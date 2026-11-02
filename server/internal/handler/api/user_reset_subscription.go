package api

// user_reset_subscription.go 是用户端「一键重置订阅」。
//
// 为什么用户需要它：订阅链接的 token 同时是登录凭证（POST /api/login 的入参就是
// users.token），分享订阅链接等于把登录入口给出去。此前处置手段只有整号删除或找管理员，
// 链接一旦泄露，用户自己什么都做不了。
//
// ## 与管理员版（admin/user.go 的 userResetSubscription）的差别
//
//   - 只能重置**自己**（用户 id 来自鉴权中间件注入的 user，不接受请求体里的 id）；
//   - 因此必须**限频**：管理员一天点不了几次，用户自己手滑可以连点。
//
// ## 铁律：绝不踢掉当前登录态
//
// 换的是 users.token + users.uuid（订阅凭据），**不碰 access_tokens 表**
// （登录态是 access_tokens 里的 bearer token，与 users.token 完全无关）。
// 效果：
//   - 用户当前这个浏览器/App **不需要重新登录**，页面继续可用；
//   - 但下一次登录（POST /api/login 用旧 token）会失败 —— 因为 users.token 已经换了。
//
// 对比：改密码会**故意**调 DeleteAccessTokensByUser 踢掉所有会话，那是「密码泄露」
// 的正确处置；订阅泄露只换订阅凭据，两件事不能混。
//
// ⚠️ 以后改这个文件时：**不要**在这里加删除 access_tokens 的逻辑。

import (
	"context"
	"errors"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	// subResetInterval 是同一用户两次重置之间的最小间隔。
	// 24 小时：正常用户一辈子点不了几次；泄漏事故里也来得及处理。
	subResetInterval = 24 * time.Hour
	// subResetCachePrefix 是限频键前缀（与 rate:* 那批共用 cache 表）。
	subResetCachePrefix = "user:reset-sub:"
	// subResetRateCode 是限频时的业务码（前端据此提示「太频繁」而不是「失败」）。
	subResetRateCode = 429
)

// handleResetSubscription 是 POST /api/reset-subscription。
//
// 返回：{"token": ..., "uuid": ..., "sub_path": "/api/sub/<新token>"}
// 前端拿 sub_path 拼出完整地址展示，**绝不复用页面里可能还留着的旧 user.token**。
func (e *UserExtras) handleResetSubscription(c *gin.Context) {
	u := currentUser(c)
	if u == nil {
		fail(c, "未认证", 401)
		return
	}
	ctx := c.Request.Context()
	now := e.handler.now()

	// ── 限频：24 小时内只能一次 ──
	key := subResetCachePrefix + itoa64(u.ID)
	if raw, ok, err := e.handler.cache.Get(ctx, key); err == nil && ok {
		if at, perr := parseCacheTime(raw); perr == nil {
			if remain := subResetInterval - now.Sub(at); remain > 0 {
				fail(c, "重置太频繁：24 小时内只能重置一次，请约 "+humanSpan(remain)+"后再试",
					subResetRateCode)
				return
			}
		}
	}

	newToken := "sub_" + randomString(32)
	newUUID := newUUIDv4()

	// 只改 token 与 uuid。**不写 access_tokens**，所以当前登录态原样保留。
	if _, err := e.db.ExecContext(ctx,
		"UPDATE users SET token = ?, uuid = ?, updated_at = ? WHERE id = ?",
		newToken, newUUID, dbTimeLiteral(now), u.ID); err != nil {
		e.handler.logger.Printf("重置订阅：更新 users 失败 user=%d: %v", u.ID, err)
		fail(c, "服务器错误", 500)
		return
	}

	// 记限频（失败不影响本次重置结果，只记一条日志）。
	if err := e.handler.cache.Put(ctx, key, now.Format(time.RFC3339), subResetInterval); err != nil {
		e.handler.logger.Printf("重置订阅：写限频键失败（忽略）user=%d: %v", u.ID, err)
	}

	// 节点侧按新凭据重建。失败只记日志：本地已经换完了，回滚反而更糟，
	// 下一轮对账/重拉会把节点补齐（与管理员版同口径）。
	if err := e.rebuildNodeCredentials(ctx, u.ID, newUUID, now); err != nil {
		e.handler.logger.Printf("重置订阅 %d 后重建节点 client 失败（忽略）: %v", u.ID, err)
	}

	// 该用户在线的前端立刻收到新地址（弹窗之外的第二重保障）。
	// 推送失败不影响接口结果：前端本来就用接口返回值就地刷新。
	notifier := NewUserNotifier(e.handler.userHub, e.handler.logger)
	if err := notifier.NotifySubscriptionReset(ctx, u.ID, newToken, "/api/sub/"+newToken); err != nil {
		e.handler.logger.Printf("重置订阅 %d 后推送通知失败（忽略）: %v", u.ID, err)
	}

	success(c, jo(
		"token", newToken,
		"uuid", newUUID,
		"sub_path", "/api/sub/"+newToken,
	), "订阅已重置，旧链接立即失效；当前登录不受影响")
}

// rebuildNodeCredentials 给该用户所在的裸内核节点下发「删旧 client + 加新 client」。
//
// 用与管理员版同一套指令通道（node_api_commands 的 rmu / adu），只是这里直接写表，
// 避免 api 包反向依赖 admin 包（两个 handler 包互不引用）。
//
// 顺序必须是「先 rmu 再 adu」：旧 uuid 与新 uuid 是两个不同身份，先删再加才能保证
// 节点上不留旧凭据（否则旧链接对应的 uuid 在节点上还能用）。
func (e *UserExtras) rebuildNodeCredentials(ctx context.Context, userID uint64, newUUID string, now time.Time) error {
	email := "ch_user_" + itoa64(userID)

	rows, err := e.db.QueryContext(ctx,
		"SELECT id FROM nodes WHERE enabled = 1 AND (driver_type IS NULL OR driver_type = '' OR driver_type = 'xray')")
	if err != nil {
		return err
	}
	var nodeIDs []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		nodeIDs = append(nodeIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(nodeIDs) == 0 {
		return nil
	}

	ts := dbTimeLiteral(now)
	rmuPayload := `{"email":"` + email + `"}`
	aduPayload := `{"email":"` + email + `","id":"` + newUUID + `"}`

	for _, nid := range nodeIDs {
		if _, err := e.db.ExecContext(ctx,
			"INSERT INTO node_api_commands (node_id, user_id, action, payload, status, created_at, updated_at) "+
				"VALUES (?, ?, 'rmu', ?, 'pending', ?, ?)",
			nid, userID, rmuPayload, ts, ts); err != nil {
			return err
		}
		if _, err := e.db.ExecContext(ctx,
			"INSERT INTO node_api_commands (node_id, user_id, action, payload, status, created_at, updated_at) "+
				"VALUES (?, ?, 'adu', ?, 'pending', ?, ?)",
			nid, userID, aduPayload, ts, ts); err != nil {
			return err
		}
	}
	return nil
}

// parseCacheTime 读回限频键里存的时间（DB cache 可能回 string 或 []byte）。
func parseCacheTime(raw any) (time.Time, error) {
	switch v := raw.(type) {
	case string:
		return time.Parse(time.RFC3339, v)
	case []byte:
		return time.Parse(time.RFC3339, string(v))
	}
	return time.Time{}, errBadCacheValue
}

// humanSpan 把剩余时长说成人话。
func humanSpan(d time.Duration) string {
	if d >= time.Hour {
		return itoa64(uint64(d.Hours())) + " 小时"
	}
	if d >= time.Minute {
		return itoa64(uint64(d.Minutes())) + " 分钟"
	}
	return itoa64(uint64(d.Seconds())) + " 秒"
}

// dbTimeLiteral 把时间格式化成库里的 datetime 字面量（项目约定：一律 UTC 字面量）。
func dbTimeLiteral(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05") }

// itoa64 是 uint64 → 十进制串（包内已有 itoa 处理 int64，避免重名）。
func itoa64(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// errBadCacheValue 表示 cache 里存的值不是预期的时间串（当作「没有记录」处理）。
var errBadCacheValue = errors.New("cache value is not a time")
