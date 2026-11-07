package admin

// 余额域（管理端）的单测：内存假 Store + httptest，不连库。
//
// 覆盖三件容易写错的事：
//  1. 兑换码格式/批量生成/参数校验（value 必须是整数分，count 1-100）；
//  2. 删除（物理删除）对任意状态生效，不存在回 404；
//  3. 人工调账必须把**带符号的变动额**写进流水（变异测试锚点：
//     buildBalanceLog 里 amount 恒 0 → TestBuildBalanceLogKeepsSignedAmount 必红）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"controlhub-server/internal/httpx"

	"github.com/gin-gonic/gin"
)

// ---- 假 Store ----

type fakeBalanceStore struct {
	nextGiftcardID uint64
	giftcards      map[uint64]*Giftcard
	users          map[uint64]int64
	logs           map[uint64][]BalanceLog

	nextFolderID uint64
	folders      map[uint64]string

	insertCalls   int
	conflictOnce  bool // 第一次 InsertGiftcards 返回 ErrGiftcardCodeTaken（模拟码撞车）
	lastInserted  []NewGiftcard
	lastAdjust    *BalanceAdjust
	adjustFailure error
}

func newFakeBalanceStore() *fakeBalanceStore {
	return &fakeBalanceStore{
		nextGiftcardID: 1,
		giftcards:      map[uint64]*Giftcard{},
		users:          map[uint64]int64{},
		logs:           map[uint64][]BalanceLog{},
		nextFolderID:   1,
		folders:        map[uint64]string{},
	}
}

func (f *fakeBalanceStore) addGiftcard(g Giftcard) uint64 {
	if g.ID == 0 {
		g.ID = f.nextGiftcardID
		f.nextGiftcardID++
	}
	copied := g
	f.giftcards[g.ID] = &copied
	return g.ID
}

func (f *fakeBalanceStore) GiftcardPage(_ context.Context, filter GiftcardFilter, page, perPage int64) ([]Giftcard, int64, error) {
	out := []Giftcard{}
	for _, g := range f.giftcards {
		if filter.Status != nil && g.Status != *filter.Status {
			continue
		}
		// 归档口径：FolderID=nil 只出未归档；指向某 id 只出该文件夹内的码。
		if filter.FolderID != nil {
			if g.FolderID == nil || *g.FolderID != *filter.FolderID {
				continue
			}
		} else if g.FolderID != nil {
			continue
		}
		out = append(out, *g)
	}
	switch filter.ValueOrder {
	case "asc":
		sort.Slice(out, func(i, j int) bool {
			if out[i].Value != out[j].Value {
				return out[i].Value < out[j].Value
			}
			return out[i].ID > out[j].ID
		})
	case "desc":
		sort.Slice(out, func(i, j int) bool {
			if out[i].Value != out[j].Value {
				return out[i].Value > out[j].Value
			}
			return out[i].ID > out[j].ID
		})
	default:
		sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	}
	total := int64(len(out))
	start := (page - 1) * perPage
	if start < 0 || start >= total {
		return []Giftcard{}, total, nil
	}
	end := start + perPage
	if end > total {
		end = total
	}
	return out[start:end], total, nil
}

func (f *fakeBalanceStore) GiftcardByID(_ context.Context, id uint64) (*Giftcard, error) {
	g, ok := f.giftcards[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	copied := *g
	return &copied, nil
}

func (f *fakeBalanceStore) InsertGiftcards(_ context.Context, rows []NewGiftcard) error {
	f.insertCalls++
	if f.conflictOnce && f.insertCalls == 1 {
		return ErrGiftcardCodeTaken
	}
	for _, r := range rows {
		for _, existing := range f.giftcards {
			if existing.Code == r.Code {
				return ErrGiftcardCodeTaken
			}
		}
	}
	f.lastInserted = append([]NewGiftcard(nil), rows...)
	for _, r := range rows {
		f.addGiftcard(Giftcard{
			Code:      r.Code,
			Name:      r.Name,
			Value:     r.Value,
			Status:    GiftcardStatusActive,
			ExpiresAt: r.ExpiresAt,
			CreatedBy: r.CreatedBy,
			Note:      r.Note,
			FolderID:  r.FolderID,
			CreatedAt: &r.CreatedAt,
		})
	}
	return nil
}

// —— 兑换码文件夹（测试用内存实现）——

func (f *fakeBalanceStore) GiftcardFolderCounts(_ context.Context, filter GiftcardFilter) ([]GiftcardFolder, error) {
	counts := map[uint64]int64{}
	for _, g := range f.giftcards {
		if filter.Status != nil && g.Status != *filter.Status {
			continue
		}
		if g.FolderID != nil {
			counts[*g.FolderID]++
		}
	}
	out := []GiftcardFolder{}
	for id, name := range f.folders {
		out = append(out, GiftcardFolder{ID: id, Name: name, CodeCount: counts[id]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (f *fakeBalanceStore) GiftcardFolderByID(_ context.Context, id uint64) (*GiftcardFolder, error) {
	name, ok := f.folders[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return &GiftcardFolder{ID: id, Name: name}, nil
}

func (f *fakeBalanceStore) GiftcardFolderByName(_ context.Context, name string) (*GiftcardFolder, error) {
	for id, n := range f.folders {
		if n == name {
			return &GiftcardFolder{ID: id, Name: n}, nil
		}
	}
	return nil, nil
}

func (f *fakeBalanceStore) GiftcardFolderCreate(_ context.Context, name string) (*GiftcardFolder, error) {
	id := f.nextFolderID
	f.nextFolderID++
	f.folders[id] = name
	return &GiftcardFolder{ID: id, Name: name}, nil
}

func (f *fakeBalanceStore) GiftcardFolderUpdate(_ context.Context, id uint64, name string) error {
	if _, ok := f.folders[id]; !ok {
		return sql.ErrNoRows
	}
	f.folders[id] = name
	return nil
}

func (f *fakeBalanceStore) GiftcardFolderDelete(_ context.Context, id uint64) error {
	delete(f.folders, id)
	return nil
}

func (f *fakeBalanceStore) DetachGiftcardsFromFolder(_ context.Context, folderID uint64) error {
	for _, g := range f.giftcards {
		if g.FolderID != nil && *g.FolderID == folderID {
			g.FolderID = nil
		}
	}
	return nil
}

func (f *fakeBalanceStore) GiftcardIDsInFolder(_ context.Context, folderID uint64) ([]uint64, error) {
	out := []uint64{}
	for id, g := range f.giftcards {
		if g.FolderID != nil && *g.FolderID == folderID {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] > out[j] })
	return out, nil
}

func (f *fakeBalanceStore) DeleteGiftcard(_ context.Context, id uint64) (bool, error) {
	if _, ok := f.giftcards[id]; !ok {
		return false, nil
	}
	delete(f.giftcards, id)
	return true, nil
}

func (f *fakeBalanceStore) UserBalance(_ context.Context, userID uint64) (int64, error) {
	bal, ok := f.users[userID]
	if !ok {
		return 0, ErrUserNotFound
	}
	return bal, nil
}

func (f *fakeBalanceStore) UserBalanceLogs(_ context.Context, userID uint64, page, perPage int64) ([]BalanceLog, int64, error) {
	all := append([]BalanceLog(nil), f.logs[userID]...)
	sort.Slice(all, func(i, j int) bool { return all[i].ID > all[j].ID })
	total := int64(len(all))
	start := (page - 1) * perPage
	if start < 0 || start >= total {
		return []BalanceLog{}, total, nil
	}
	end := start + perPage
	if end > total {
		end = total
	}
	return all[start:end], total, nil
}

func (f *fakeBalanceStore) AdjustUserBalance(_ context.Context, in BalanceAdjust) (*BalanceAdjustment, error) {
	f.lastAdjust = &in
	if f.adjustFailure != nil {
		return nil, f.adjustFailure
	}
	current, ok := f.users[in.UserID]
	if !ok {
		return nil, ErrUserNotFound
	}
	next := current + in.Amount
	if next < 0 {
		return nil, ErrBalanceNotEnough
	}
	f.users[in.UserID] = next
	log := buildBalanceLog(in.UserID, in.Amount, BalanceTypeAdmin, in.OperatorID, next, in.Note, in.Now)
	log.ID = uint64(len(f.logs[in.UserID]) + 1)
	f.logs[in.UserID] = append(f.logs[in.UserID], *log)
	return &BalanceAdjustment{Balance: next, Log: log}, nil
}

// ---- 测试脚手架 ----

func newBalanceTestRouter(d *BalanceDomain, adminID uint64) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/admin-api")
	if adminID > 0 {
		// 键名与 httpx.admin_auth.go 的 ctxAdminKey 一致；改那边这个测试会立刻红，
		// 不会静默失去 operator_id。
		g.Use(func(c *gin.Context) {
			c.Set("httpx.admin", &httpx.AdminIdentity{ID: adminID})
			c.Next()
		})
	}
	d.Register(g)
	return r
}

func callBalance(t *testing.T, r *gin.Engine, method, path, body string) map[string]any {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("%s %s → HTTP %d，期望 200（管理端统一 HTTP 200）", method, path, w.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s 响应不是 JSON: %v (%s)", method, path, err, w.Body.String())
	}
	return out
}

func balanceData(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	data, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatalf("data 不是对象: %#v", resp["data"])
	}
	return data
}

func balanceIntField(t *testing.T, m map[string]any, key string) int64 {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		t.Fatalf("字段 %s 不是数字: %#v", key, m[key])
	}
	return int64(v)
}

func balanceStringField(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key].(string)
	if !ok {
		t.Fatalf("字段 %s 不是字符串: %#v", key, m[key])
	}
	return v
}

// ---- 用例：流水 ----

// TestBuildBalanceLogKeepsSignedAmount 是变异测试锚点。
//
// 把 buildBalanceLog 里的 Amount 改成 0（或绝对值），本用例立刻红：
// 流水必须记录带符号的真实变动额，否则后台永远对不上账。
func TestBuildBalanceLogKeepsSignedAmount(t *testing.T) {
	op := uint64(9)
	note := "手动扣减"
	at := time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)

	log := buildBalanceLog(42, -1500, BalanceTypeAdmin, &op, 3500, &note, at)
	if log.Amount != -1500 {
		t.Fatalf("流水金额应为 -1500（负=扣），实际 %d", log.Amount)
	}
	if log.BalanceAfter != 3500 {
		t.Fatalf("balance_after 应为 3500，实际 %d", log.BalanceAfter)
	}
	if log.Type != BalanceTypeAdmin {
		t.Fatalf("流水类型应为 %s，实际 %s", BalanceTypeAdmin, log.Type)
	}
	if log.OperatorID == nil || *log.OperatorID != 9 {
		t.Fatalf("operator_id 应为 9，实际 %v", log.OperatorID)
	}
	if log.UserID != 42 {
		t.Fatalf("user_id 应为 42，实际 %d", log.UserID)
	}
	if log.Note == nil || *log.Note != note {
		t.Fatalf("note 未保存：%v", log.Note)
	}
	if log.CreatedAt == nil || !log.CreatedAt.Equal(at) {
		t.Fatalf("created_at 不对：%v", log.CreatedAt)
	}
}

// ---- 用例：兑换码 ----

func TestGiftcardStoreGeneratesFormattedCodes(t *testing.T) {
	store := newFakeBalanceStore()
	d := NewBalanceDomain(BalanceDeps{Store: store})
	r := newBalanceTestRouter(d, 7)

	resp := callBalance(t, r, http.MethodPost, "/admin-api/giftcards",
		`{"count":3,"value":5000,"name":"活动码","note":"双十一"}`)
	if code := balanceIntField(t, resp, "code"); code != 0 {
		t.Fatalf("生成兑换码应成功，实际 code=%d msg=%v", code, resp["msg"])
	}
	data := balanceData(t, resp)
	codes, ok := data["codes"].([]any)
	if !ok || len(codes) != 3 {
		t.Fatalf("应返回 3 个码：%#v", data["codes"])
	}
	seen := map[string]bool{}
	for _, raw := range codes {
		code := raw.(string)
		if len(code) != 19 {
			t.Fatalf("码长度应为 19（XXXX-XXXX-XXXX-XXXX），实际 %q", code)
		}
		parts := strings.Split(code, "-")
		if len(parts) != 4 {
			t.Fatalf("码应有 4 组：%q", code)
		}
		for _, part := range parts {
			if len(part) != 4 || strings.ToUpper(part) != part {
				t.Fatalf("码组应为 4 位大写 hex：%q", code)
			}
			for _, ch := range part {
				if !strings.ContainsRune("0123456789ABCDEF", ch) {
					t.Fatalf("码里出现非 hex 字符：%q", code)
				}
			}
		}
		if seen[code] {
			t.Fatalf("批内出现重复码：%q", code)
		}
		seen[code] = true
	}

	if len(store.lastInserted) != 3 {
		t.Fatalf("应落库 3 行，实际 %d", len(store.lastInserted))
	}
	for _, row := range store.lastInserted {
		if row.Value != 5000 {
			t.Fatalf("面值应为 5000 分，实际 %d", row.Value)
		}
		if row.Name != "活动码" {
			t.Fatalf("名称应为「活动码」，实际 %q", row.Name)
		}
		if row.Note == nil || *row.Note != "双十一" {
			t.Fatalf("备注未落库：%v", row.Note)
		}
		if row.CreatedBy == nil || *row.CreatedBy != 7 {
			t.Fatalf("created_by 应为当前管理员 7，实际 %v", row.CreatedBy)
		}
	}
}

func TestGiftcardStoreRetriesOnCodeConflict(t *testing.T) {
	store := newFakeBalanceStore()
	store.conflictOnce = true
	d := NewBalanceDomain(BalanceDeps{Store: store})
	r := newBalanceTestRouter(d, 0)

	resp := callBalance(t, r, http.MethodPost, "/admin-api/giftcards", `{"count":2,"value":100}`)
	if code := balanceIntField(t, resp, "code"); code != 0 {
		t.Fatalf("撞码后应自动重生成并成功，实际 code=%d msg=%v", code, resp["msg"])
	}
	if store.insertCalls != 2 {
		t.Fatalf("应重试 2 次（第一次撞车），实际 %d 次", store.insertCalls)
	}
}

func TestGiftcardStoreRejectsBadInput(t *testing.T) {
	store := newFakeBalanceStore()
	d := NewBalanceDomain(BalanceDeps{Store: store})
	r := newBalanceTestRouter(d, 0)

	cases := []struct {
		name string
		body string
		want string
	}{
		{"缺面值", `{"count":1}`, "请填写面值"},
		{"面值为 0", `{"count":1,"value":0}`, "面值必须大于 0"},
		{"面值为负", `{"count":1,"value":-100}`, "面值必须大于 0"},
		{"面值小数分", `{"count":1,"value":10.5}`, "面值必须是整数"},
		{"数量为 0", `{"count":0,"value":100}`, "生成数量需在 1-100 之间"},
		{"数量超上限", `{"count":101,"value":100}`, "生成数量需在 1-100 之间"},
		{"过期时间格式错", `{"count":1,"value":100,"expires_at":"明天"}`, "过期时间格式不正确"},
		{"过期时间已过", `{"count":1,"value":100,"expires_at":"2020-01-01 00:00:00"}`, "过期时间必须晚于当前时间"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := callBalance(t, r, http.MethodPost, "/admin-api/giftcards", tc.body)
			if code := balanceIntField(t, resp, "code"); code == 0 {
				t.Fatalf("非法输入应被拒，实际成功：%v", resp)
			}
			msg, _ := resp["msg"].(string)
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("文案应包含 %q，实际 %q", tc.want, msg)
			}
			if store.insertCalls != 0 {
				t.Fatalf("校验失败不能落库，实际 insertCalls=%d", store.insertCalls)
			}
		})
	}
}

func TestGiftcardIndexFiltersByStatus(t *testing.T) {
	store := newFakeBalanceStore()
	store.addGiftcard(Giftcard{Code: "AAAA-AAAA-AAAA-AAAA", Name: "a", Value: 100, Status: GiftcardStatusActive})
	store.addGiftcard(Giftcard{Code: "BBBB-BBBB-BBBB-BBBB", Name: "b", Value: 200, Status: GiftcardStatusUsed})
	d := NewBalanceDomain(BalanceDeps{Store: store})
	r := newBalanceTestRouter(d, 0)

	resp := callBalance(t, r, http.MethodGet, "/admin-api/giftcards?status=active", "")
	data := balanceData(t, resp)
	list, _ := data["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("status=active 应只回 1 条，实际 %d", len(list))
	}
	row := list[0].(map[string]any)
	if got := balanceStringField(t, row, "code"); got != "AAAA-AAAA-AAAA-AAAA" {
		t.Fatalf("筛错了行：%s", got)
	}
	if got := balanceIntField(t, data, "total"); got != 1 {
		t.Fatalf("total 应为 1，实际 %d", got)
	}
	if got := balanceIntField(t, row, "value"); got != 100 {
		t.Fatalf("面值应为 100 分（整数，不做元转换），实际 %d", got)
	}

	bad := callBalance(t, r, http.MethodGet, "/admin-api/giftcards?status=nope", "")
	if code := balanceIntField(t, bad, "code"); code == 0 {
		t.Fatalf("非法 status 应被拒：%v", bad)
	}
}

// ---- 用例：兑换码文件夹与面值排序（HTTP 契约）----

// TestGiftcardIndexReturnsFoldersAndSortsByValue 验列表接口返回 folders + value_order 生效。
func TestGiftcardIndexReturnsFoldersAndSortsByValue(t *testing.T) {
	store := newFakeBalanceStore()
	fid, _ := store.GiftcardFolderCreate(context.Background(), "活动A")
	f2, _ := store.GiftcardFolderCreate(context.Background(), "活动B")
	inA := fid.ID
	inB := f2.ID
	store.addGiftcard(Giftcard{Code: "A-1", Value: 100, Status: GiftcardStatusActive, FolderID: &inA})
	store.addGiftcard(Giftcard{Code: "A-2", Value: 900, Status: GiftcardStatusActive, FolderID: &inA})
	store.addGiftcard(Giftcard{Code: "B-1", Value: 500, Status: GiftcardStatusActive, FolderID: &inB})
	store.addGiftcard(Giftcard{Code: "U-1", Value: 300, Status: GiftcardStatusActive})
	store.addGiftcard(Giftcard{Code: "U-2", Value: 200, Status: GiftcardStatusActive})
	d := NewBalanceDomain(BalanceDeps{Store: store})
	r := newBalanceTestRouter(d, 0)

	// 1) 默认只出未归档（2 条），folders 始终全量返回 + 计数
	resp := callBalance(t, r, http.MethodGet, "/admin-api/giftcards", "")
	data := balanceData(t, resp)
	list, _ := data["list"].([]any)
	if len(list) != 2 {
		t.Fatalf("默认应只回未归档 2 条，实际 %d", len(list))
	}
	folders, _ := data["folders"].([]any)
	if len(folders) != 2 {
		t.Fatalf("folders 应 2 个，实际 %d", len(folders))
	}
	counts := map[string]int64{}
	for _, f := range folders {
		fm := f.(map[string]any)
		counts[balanceStringField(t, fm, "name")] = balanceIntField(t, fm, "code_count")
	}
	if counts["活动A"] != 2 || counts["活动B"] != 1 {
		t.Fatalf("文件夹计数不对: %+v", counts)
	}

	// 2) value_order=desc 在未归档里应 300 在前
	resp2 := callBalance(t, r, http.MethodGet, "/admin-api/giftcards?value_order=desc", "")
	l2 := balanceData(t, resp2)["list"].([]any)
	if got := balanceIntField(t, l2[0].(map[string]any), "value"); got != 300 {
		t.Fatalf("value_order=desc 首条应 300，实际 %d", got)
	}
	// 3) value_order=asc 应 200 在前
	resp3 := callBalance(t, r, http.MethodGet, "/admin-api/giftcards?value_order=asc", "")
	l3 := balanceData(t, resp3)["list"].([]any)
	if got := balanceIntField(t, l3[0].(map[string]any), "value"); got != 200 {
		t.Fatalf("value_order=asc 首条应 200，实际 %d", got)
	}
	// 4) folder=id 只出该文件夹
	resp4 := callBalance(t, r, http.MethodGet, "/admin-api/giftcards?folder="+strconv.FormatUint(fid.ID, 10), "")
	l4 := balanceData(t, resp4)["list"].([]any)
	if len(l4) != 2 {
		t.Fatalf("folder 应回 2 条，实际 %d", len(l4))
	}
	// 5) 非法排序被拒
	bad := callBalance(t, r, http.MethodGet, "/admin-api/giftcards?value_order=nope", "")
	if balanceIntField(t, bad, "code") == 0 {
		t.Fatalf("非法 value_order 应被拒: %v", bad)
	}
}

// TestGiftcardFolderCRUDViaHTTP 验文件夹建/改名/删（含 with_codes 两种语义）。
func TestGiftcardFolderCRUDViaHTTP(t *testing.T) {
	store := newFakeBalanceStore()
	d := NewBalanceDomain(BalanceDeps{Store: store})
	r := newBalanceTestRouter(d, 1)

	// 建
	resp := callBalance(t, r, http.MethodPost, "/admin-api/giftcard-folders", `{"name":"活动A"}`)
	fid := balanceIntField(t, balanceData(t, resp), "id")
	if fid == 0 {
		t.Fatalf("建文件夹没回 id: %v", resp)
	}
	// 空名被拒
	if code := balanceIntField(t, callBalance(t, r, http.MethodPost, "/admin-api/giftcard-folders", `{"name":"  "}`), "code"); code == 0 {
		t.Fatal("空文件夹名应被拒")
	}
	// 改名
	idStr := strconv.FormatInt(fid, 10)
	callBalance(t, r, http.MethodPut, "/admin-api/giftcard-folders/"+idStr, `{"name":"活动A改"}`)
	if f, _ := store.GiftcardFolderByID(context.Background(), uint64(fid)); f == nil || f.Name != "活动A改" {
		t.Fatalf("改名没生效: %+v", f)
	}
	// 放一个码进去
	fidU := uint64(fid)
	store.addGiftcard(Giftcard{Code: "X-1", Value: 100, Status: GiftcardStatusActive, FolderID: &fidU})

	// 只删文件夹 → 码退回未归档
	callBalance(t, r, http.MethodDelete, "/admin-api/giftcard-folders/"+idStr, "")
	if f, _ := store.GiftcardFolderByID(context.Background(), fidU); f != nil {
		t.Fatal("文件夹应已删除")
	}
	if g, _ := store.GiftcardByID(context.Background(), 1); g == nil || g.FolderID != nil {
		t.Fatalf("码应保留并退回未归档: %+v", g)
	}

	// 再造一个文件夹 + 码，测 with_codes
	f2, _ := store.GiftcardFolderCreate(context.Background(), "活动B")
	f2id := f2.ID
	store.addGiftcard(Giftcard{Code: "Y-1", Value: 200, Status: GiftcardStatusActive, FolderID: &f2id})
	callBalance(t, r, http.MethodDelete, "/admin-api/giftcard-folders/"+strconv.FormatUint(f2id, 10)+"?with_codes=1", "")
	if _, err := store.GiftcardFolderByID(context.Background(), f2id); err == nil {
		t.Fatal("文件夹B应已删除")
	}
	// Y-1 已被一并删除
	found := false
	for _, g := range store.giftcards {
		if g.Code == "Y-1" {
			found = true
		}
	}
	if found {
		t.Fatal("with_codes=1 应把文件夹内的码一起删掉")
	}

	// 不存在的文件夹 → 404
	nf := callBalance(t, r, http.MethodDelete, "/admin-api/giftcard-folders/99999", "")
	if balanceIntField(t, nf, "code") != 404 {
		t.Fatalf("不存在文件夹应回 404: %v", nf)
	}
}

// TestGiftcardStoreArchivesToFolder 验生成时带 folder_id / folder_name 会归档。
func TestGiftcardStoreArchivesToFolder(t *testing.T) {
	store := newFakeBalanceStore()
	d := NewBalanceDomain(BalanceDeps{Store: store})
	r := newBalanceTestRouter(d, 1)

	// folder_name 不存在 → 自动建
	callBalance(t, r, http.MethodPost, "/admin-api/giftcards",
		`{"count":2,"value":100,"folder_name":"新建的组"}`)
	if len(store.lastInserted) != 2 || store.lastInserted[0].FolderID == nil {
		t.Fatalf("folder_name 应自动建文件夹并归档: %+v", store.lastInserted)
	}
	if len(store.folders) != 1 {
		t.Fatalf("应自动建出 1 个文件夹，实际 %d", len(store.folders))
	}
	// 同一 folder_name 再用一次 → 复用，不重复建
	callBalance(t, r, http.MethodPost, "/admin-api/giftcards",
		`{"count":1,"value":100,"folder_name":"新建的组"}`)
	if len(store.folders) != 1 {
		t.Fatalf("同名 folder_name 应复用文件夹，实际 %d 个", len(store.folders))
	}
}

// ---- 用例：用户余额 ----

func TestUserBalanceShow(t *testing.T) {
	store := newFakeBalanceStore()
	store.users[8] = 3500
	store.logs[8] = []BalanceLog{
		{ID: 1, UserID: 8, Amount: 5000, Type: BalanceTypeAdmin, BalanceAfter: 5000, CreatedAt: timePtr(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))},
		{ID: 2, UserID: 8, Amount: -1500, Type: "consume", BalanceAfter: 3500, CreatedAt: timePtr(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC))},
	}
	d := NewBalanceDomain(BalanceDeps{Store: store})
	r := newBalanceTestRouter(d, 0)

	resp := callBalance(t, r, http.MethodGet, "/admin-api/users/8/balance", "")
	data := balanceData(t, resp)
	if got := balanceIntField(t, data, "balance"); got != 3500 {
		t.Fatalf("余额应为 3500 分，实际 %d", got)
	}
	logs, _ := data["logs"].([]any)
	if len(logs) != 2 {
		t.Fatalf("应回 2 条流水，实际 %d", len(logs))
	}
	first := logs[0].(map[string]any) // id 倒序
	if got := balanceIntField(t, first, "amount"); got != -1500 {
		t.Fatalf("最新流水金额应为 -1500，实际 %d", got)
	}

	missing := callBalance(t, r, http.MethodGet, "/admin-api/users/404/balance", "")
	if code := balanceIntField(t, missing, "code"); code != 404 {
		t.Fatalf("不存在的用户应回 404，实际 %v", missing)
	}
}

func TestUserBalanceAdjustWritesSignedLog(t *testing.T) {
	store := newFakeBalanceStore()
	store.users[8] = 1000
	d := NewBalanceDomain(BalanceDeps{Store: store})
	r := newBalanceTestRouter(d, 33)

	resp := callBalance(t, r, http.MethodPost, "/admin-api/users/8/balance",
		`{"amount":500,"note":"手动充值"}`)
	if code := balanceIntField(t, resp, "code"); code != 0 {
		t.Fatalf("调账应成功：%v", resp)
	}
	data := balanceData(t, resp)
	if got := balanceIntField(t, data, "balance"); got != 1500 {
		t.Fatalf("调整后余额应为 1500，实际 %d", got)
	}
	if got := balanceIntField(t, data, "amount"); got != 500 {
		t.Fatalf("回执金额应为 500，实际 %d", got)
	}
	if store.lastAdjust == nil || store.lastAdjust.Amount != 500 {
		t.Fatalf("Store 收到的金额不对：%+v", store.lastAdjust)
	}
	if store.lastAdjust.OperatorID == nil || *store.lastAdjust.OperatorID != 33 {
		t.Fatalf("operator_id 应为当前管理员 33，实际 %v", store.lastAdjust.OperatorID)
	}
	if got := store.users[8]; got != 1500 {
		t.Fatalf("用户余额应更新为 1500，实际 %d", got)
	}
	log := store.logs[8]
	if len(log) != 1 || log[0].Amount != 500 || log[0].BalanceAfter != 1500 {
		t.Fatalf("流水不对：%+v", log)
	}

	// 负向调账：流水必须记负数，余额递减。
	neg := callBalance(t, r, http.MethodPost, "/admin-api/users/8/balance", `{"amount":-1500}`)
	if code := balanceIntField(t, neg, "code"); code != 0 {
		t.Fatalf("扣减到 0 应允许：%v", neg)
	}
	if got := balanceIntField(t, balanceData(t, neg), "balance"); got != 0 {
		t.Fatalf("扣减后余额应为 0，实际 %d", got)
	}
	log = store.logs[8]
	if len(log) != 2 || log[1].Amount != -1500 {
		t.Fatalf("扣减流水金额应为 -1500，实际 %+v", log)
	}

	// 余额不足：拒绝且不落流水。
	over := callBalance(t, r, http.MethodPost, "/admin-api/users/8/balance", `{"amount":-1}`)
	if code := balanceIntField(t, over, "code"); code != 400 {
		t.Fatalf("扣成负数应回 400，实际 %v", over)
	}
	if len(store.logs[8]) != 2 {
		t.Fatalf("失败的调账不能写流水，实际 %d 条", len(store.logs[8]))
	}
}

func TestUserBalanceAdjustRejectsBadInput(t *testing.T) {
	store := newFakeBalanceStore()
	store.users[8] = 1000
	d := NewBalanceDomain(BalanceDeps{Store: store})
	r := newBalanceTestRouter(d, 0)

	cases := []struct{ name, body, want string }{
		{"缺金额", `{}`, "请填写调整金额"},
		{"金额为 0", `{"amount":0}`, "调整金额不能为 0"},
		{"金额小数", `{"amount":1.5}`, "调整金额必须是整数"},
		{"金额非数字", `{"amount":"abc"}`, "调整金额必须是整数"},
		{"备注超长", `{"amount":100,"note":"` + strings.Repeat("字", 256) + `"}`, "备注不能超过 255 个字符"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := callBalance(t, r, http.MethodPost, "/admin-api/users/8/balance", tc.body)
			if code := balanceIntField(t, resp, "code"); code != 400 {
				t.Fatalf("应回 400，实际 %v", resp)
			}
			msg, _ := resp["msg"].(string)
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("文案应包含 %q，实际 %q", tc.want, msg)
			}
			if store.lastAdjust != nil {
				t.Fatalf("校验失败不应调 Store")
			}
		})
	}

	missing := callBalance(t, r, http.MethodPost, "/admin-api/users/999/balance", `{"amount":100}`)
	if code := balanceIntField(t, missing, "code"); code != 404 {
		t.Fatalf("不存在的用户应回 404，实际 %v", missing)
	}
}

// TestBalanceRoutesRegisterAlongsideUserDomain 确保余额域挂到同一个 /admin-api 组时
// 不与用户域已占用的 /users/:user... 冲突。
//
// Gin 同一层级只允许一种参数名（用户域用 :user），本域也必须用 :user；
// 参数名写错会在启动时 panic，这个用例把它提前到单测里暴露。
func TestBalanceRoutesRegisterAlongsideUserDomain(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/admin-api")
	noop := func(c *gin.Context) {}
	g.GET("/users", noop)
	g.GET("/users/:user", noop)
	g.PUT("/users/:user", noop)
	g.POST("/users/:user/reset-traffic", noop)
	g.POST("/users/:user/renew", noop)

	d := NewBalanceDomain(BalanceDeps{Store: newFakeBalanceStore()})
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("路由注册 panic（多半是 :user / :giftcard 参数名冲突）: %v", p)
			}
		}()
		d.Register(g)
	}()

	routes := map[string]bool{}
	for _, ri := range r.Routes() {
		routes[ri.Method+" "+ri.Path] = true
	}
	for _, want := range []string{
		"GET /admin-api/giftcards",
		"POST /admin-api/giftcards",
		"DELETE /admin-api/giftcards/:giftcard/delete",
		"POST /admin-api/giftcards/batch-delete",
		"GET /admin-api/users/:user/balance",
		"POST /admin-api/users/:user/balance",
	} {
		if !routes[want] {
			t.Fatalf("缺少路由 %s", want)
		}
	}
}
