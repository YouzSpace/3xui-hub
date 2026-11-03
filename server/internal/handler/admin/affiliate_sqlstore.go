package admin

// affiliateSQLStore 是 AffiliateStore 的 database/sql 实现。
//
// 时间口径与全仓一致（data-contract §4）：DSN 已 parseTime=true&loc=UTC，
// 这里只读 UTC 字面量、不做时区换算。

import (
	"context"
	"database/sql"
)

// NewAffiliateSQLStore 用已有连接池构造返佣域 Store。
func NewAffiliateSQLStore(db *sql.DB) AffiliateStore {
	return &affiliateSQLStore{db: db}
}

type affiliateSQLStore struct {
	db *sql.DB
}

// RebatePage 返回当前页返佣流水（id 倒序）与总数。
//
// 邮箱走 LEFT JOIN users（删号后仍要能看到历史流水，邮箱列为 null）。
func (s *affiliateSQLStore) RebatePage(ctx context.Context, page, perPage int64) ([]AffiliateRebate, int64, error) {
	var total int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM affiliate_rebates").Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []AffiliateRebate{}, 0, nil
	}
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = affiliateRebatePerPage
	}
	offset := (page - 1) * perPage

	rows, err := s.db.QueryContext(ctx,
		`SELECT r.id, r.inviter_id, iu.email, r.invitee_id, eu.email,
		        r.order_id, r.order_no, r.amount, r.rate, r.created_at
		 FROM affiliate_rebates r
		 LEFT JOIN users iu ON iu.id = r.inviter_id
		 LEFT JOIN users eu ON eu.id = r.invitee_id
		 ORDER BY r.id DESC LIMIT ? OFFSET ?`, perPage, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []AffiliateRebate{}
	for rows.Next() {
		var (
			r            AffiliateRebate
			inviterEmail sql.NullString
			inviteeEmail sql.NullString
			createdAt    sql.NullTime
		)
		if err := rows.Scan(&r.ID, &r.InviterID, &inviterEmail, &r.InviteeID, &inviteeEmail,
			&r.OrderID, &r.OrderNo, &r.Amount, &r.Rate, &createdAt); err != nil {
			return nil, 0, err
		}
		r.InviterEmail = nullStrPtr(inviterEmail)
		r.InviteeEmail = nullStrPtr(inviteeEmail)
		r.CreatedAt = nullTimePtr(createdAt)
		out = append(out, r)
	}
	return out, total, rows.Err()
}

var _ AffiliateStore = (*affiliateSQLStore)(nil)
