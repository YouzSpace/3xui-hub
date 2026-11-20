package api

// 「验证码用哪个号发」（site_configs.verify_mail_sender）的分支单测。
//
// 这段逻辑决定验证码走池轮换还是指定账号，配错会直接让注册/找回密码卡死，
// 所以四个取值都钉住：pool / 数字 id / 空值 / 非法值。
//
// 池发信本身（挑号、换号、冷却）由 admin 包的 smtp_pool_test.go 覆盖，
// 这里只验 senderChoice 的解析与「指定号不可用就退回池」的兜底。

import (
	"context"
	"errors"
	"testing"
)

// senderStore 只实现 senderChoice 用到的 SiteConfigMany；其余方法留空（被调用即 panic）。
type senderStore struct {
	Store
	values map[string]string
	err    error
}

func (s *senderStore) SiteConfigMany(_ context.Context, keys []string) (map[string]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = s.values[k]
	}
	return out, nil
}

// senderPool 记录是否被问到「指定号可用」，并按配置回答。
type senderPool struct {
	usableByID map[uint64]bool
	asked      []uint64
}

func (p *senderPool) SendViaPool(context.Context, string, string, string) (bool, error) {
	return false, nil
}

func (p *senderPool) SendViaAccount(context.Context, uint64, string, string, string) error {
	return nil
}

func (p *senderPool) PoolAccountUsable(_ context.Context, id uint64) bool {
	p.asked = append(p.asked, id)
	return p.usableByID[id]
}

func (p *senderPool) HasUsableAccount(context.Context) bool { return true }

func TestSenderChoice(t *testing.T) {
	cases := []struct {
		name        string
		value       string // verify_mail_sender 的原始值
		configErr   error
		noPool      bool // true = 未注入池
		usable      map[uint64]bool
		wantPool    bool
		wantID      uint64
		wantAskedID uint64 // 0 = 不该问 PoolAccountUsable
	}{
		{
			name:     "空值 → 默认走池轮换",
			value:    "",
			noPool:   false,
			wantPool: true,
			wantID:   0,
		},
		{
			name:     "pool → 走池轮换",
			value:    "pool",
			wantPool: true,
			wantID:   0,
		},
		{
			name:        "数字 id 且该号可用 → 用指定号",
			value:       "42",
			usable:      map[uint64]bool{42: true},
			wantPool:    true,
			wantID:      42,
			wantAskedID: 42,
		},
		{
			name:        "数字 id 但该号不可用 → 退回池轮换",
			value:       "7",
			usable:      map[uint64]bool{7: false},
			wantPool:    true,
			wantID:      0,
			wantAskedID: 7,
		},
		{
			name:     "非法值（非数字）→ 按 pool 处理",
			value:    "default",
			wantPool: true,
			wantID:   0,
		},
		{
			name:     "负号 / 前后空白：空白被 trim，但 '-' 不是数字 → pool",
			value:    "  -3  ",
			wantPool: true,
			wantID:   0,
		},
		{
			name:     "数字 id 为 0 → 等同空值走池",
			value:    "0",
			wantPool: true,
			wantID:   0,
		},
		{
			name:      "读配置失败 → 保守走池",
			configErr: errors.New("db down"),
			wantPool:  true,
			wantID:    0,
		},
		{
			name:     "未注入池 → 不走池（历史行为：只用站点 smtp_*）",
			value:    "pool",
			noPool:   true,
			wantPool: false,
			wantID:   0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &senderStore{
				values: map[string]string{cfgVerifyMailSender: c.value},
				err:    c.configErr,
			}
			pool := &senderPool{usableByID: c.usable}
			m := NewSMTPMailer(store, nil, nil)
			if !c.noPool {
				m.SetPoolMailer(pool)
			}

			usePool, id := m.senderChoice(context.Background())
			if usePool != c.wantPool || id != c.wantID {
				t.Fatalf("senderChoice() = (%v, %d)，期望 (%v, %d)", usePool, id, c.wantPool, c.wantID)
			}
			if c.wantAskedID == 0 {
				if len(pool.asked) != 0 {
					t.Fatalf("不该询问指定号可用性，实际问了 %v", pool.asked)
				}
				return
			}
			if len(pool.asked) != 1 || pool.asked[0] != c.wantAskedID {
				t.Fatalf("应询问 id=%d 的可用性，实际 %v", c.wantAskedID, pool.asked)
			}
		})
	}
}
