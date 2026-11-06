package admin

// 删除入站时的 HY2 端口跳跃链清理单测（fix/hop-cleanup-on-inbound-delete）。
//
// 回归的是这条现场问题：面板删掉 HY2 入站只删库行 + bump 版本，节点上的
// HY2HOP-<id> nat 链留在原地，变成指向已关端口的死规则。删除路径必须补发
// 一条 firewall_hop_remove，且**只入队、不因节点离线让删除失败**。

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// hopCleanupStore 只实现删除路径真正调用到的方法，其余靠 NodeStore 空接口兜住。
type hopCleanupStore struct {
	NodeStore
	node     *NodeRecord
	inbounds map[uint64]*XrayInboundRecord
	deleted  []uint64
	bumped   uint64
	commands []hopCleanupCommand
	queueErr error // 非空时模拟入队失败
}

type hopCleanupCommand struct {
	nodeID  uint64
	action  string
	payload *jsObj
}

func (s *hopCleanupStore) NodeByID(context.Context, uint64) (*NodeRecord, error) {
	return s.node, nil
}

func (s *hopCleanupStore) XrayInboundByID(_ context.Context, _ uint64, id uint64) (*XrayInboundRecord, error) {
	if ib, ok := s.inbounds[id]; ok {
		return ib, nil
	}
	return nil, sql.ErrNoRows
}

func (s *hopCleanupStore) DeleteXrayInbound(_ context.Context, id uint64) error {
	s.deleted = append(s.deleted, id)
	delete(s.inbounds, id)
	return nil
}

func (s *hopCleanupStore) BumpConfigVersion(_ context.Context, nodeID uint64) (int64, error) {
	s.bumped = nodeID
	return 7, nil
}

func (s *hopCleanupStore) CreateCommand(_ context.Context, nodeID uint64, action string, payload any) (*CommandRecord, error) {
	if s.queueErr != nil {
		return nil, s.queueErr
	}
	obj, _ := payload.(*jsObj)
	s.commands = append(s.commands, hopCleanupCommand{nodeID: nodeID, action: action, payload: obj})
	return &CommandRecord{ID: uint64(len(s.commands)), NodeID: nodeID, Action: action}, nil
}

// jsField 取 jsObj 的原始值（同包直接读，避免为测试另造访问器）。
func jsField(o *jsObj, key string) any {
	if o == nil {
		return nil
	}
	for i, k := range o.keys {
		if k == key {
			return o.vals[i]
		}
	}
	return nil
}

func newHopCleanupDomain(store *hopCleanupStore) *NodeDomain {
	return NewNodeDomain(NodeDeps{Store: store})
}

func hopTestNode(agentVersion string) *NodeRecord {
	node := &NodeRecord{ID: 42, Name: "MYKL1", Host: "mykl1.wujiecloud.top", DriverType: "xray"}
	if agentVersion != "" {
		node.DriverConfigRaw = []byte(`{"agent_version":"` + agentVersion + `"}`)
	}
	return node
}

func hysteriaInbound(id, port uint64) *XrayInboundRecord {
	return &XrayInboundRecord{ID: id, NodeID: 42, Tag: "hy", Protocol: "hysteria", Port: int64(port)}
}

func runDelete(t *testing.T, d *NodeDomain, id string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.DELETE("/nodes/:node/xray-inbounds/:inbound", d.xrayInboundDestroy)

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/nodes/42/xray-inbounds/"+id, nil))
	return rec
}

// TestDeleteHysteriaInboundClearsHopChain 删 HY2 入站要顺带清节点上的跳跃链。
func TestDeleteHysteriaInboundClearsHopChain(t *testing.T) {
	store := &hopCleanupStore{
		node:     hopTestNode("1.2.2"),
		inbounds: map[uint64]*XrayInboundRecord{30: hysteriaInbound(30, 5001)},
	}
	rec := runDelete(t, newHopCleanupDomain(store), "30")

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", rec.Code, rec.Body.String())
	}
	if len(store.deleted) != 1 || store.deleted[0] != 30 {
		t.Fatalf("删除的入站 = %v，期望 [30]", store.deleted)
	}
	if store.bumped != 42 {
		t.Fatalf("bump 的节点 = %d，期望 42", store.bumped)
	}
	if len(store.commands) != 1 {
		t.Fatalf("入队指令数 = %d，期望 1（%+v）", len(store.commands), store.commands)
	}
	cmd := store.commands[0]
	if cmd.action != "firewall_hop_remove" {
		t.Fatalf("指令 action = %q，期望 firewall_hop_remove", cmd.action)
	}
	if cmd.nodeID != 42 {
		t.Fatalf("指令节点 = %d，期望 42", cmd.nodeID)
	}
	if got := jsonInt(jsField(cmd.payload, "inbound_id")); got != 30 {
		t.Fatalf("payload.inbound_id = %d，期望 30", got)
	}
}

// TestDeleteNonHysteriaInboundSkipsHopCleanup 非 HY2 入站没有跳跃链，不该多发指令。
func TestDeleteNonHysteriaInboundSkipsHopCleanup(t *testing.T) {
	store := &hopCleanupStore{
		node: hopTestNode("1.2.2"),
		inbounds: map[uint64]*XrayInboundRecord{
			31: {ID: 31, NodeID: 42, Tag: "vless", Protocol: "vless", Port: 8449},
		},
	}
	rec := runDelete(t, newHopCleanupDomain(store), "31")

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	if len(store.commands) != 0 {
		t.Fatalf("非 HY2 入站不应入队清链指令，实际 %+v", store.commands)
	}
}

// TestDeleteHysteriaInboundOldAgentSkipsHopCleanup agent < 1.0.8 不认该 action，
// 入队只会换回一条 unknown action，直接跳过。
func TestDeleteHysteriaInboundOldAgentSkipsHopCleanup(t *testing.T) {
	store := &hopCleanupStore{
		node:     hopTestNode("1.0.0"),
		inbounds: map[uint64]*XrayInboundRecord{30: hysteriaInbound(30, 5001)},
	}
	rec := runDelete(t, newHopCleanupDomain(store), "30")

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	if len(store.commands) != 0 {
		t.Fatalf("老 agent 不应入队清链指令，实际 %+v", store.commands)
	}
}

// TestDeleteHysteriaInboundQueueFailureKeepsDelete 入队失败也不能影响删除结果：
// 入站此时已经删掉，把删除判失败会让前端以为没删。
func TestDeleteHysteriaInboundQueueFailureKeepsDelete(t *testing.T) {
	store := &hopCleanupStore{
		node:     hopTestNode("1.2.2"),
		inbounds: map[uint64]*XrayInboundRecord{30: hysteriaInbound(30, 5001)},
	}
	store.queueErr = context.Canceled
	rec := runDelete(t, newHopCleanupDomain(store), "30")

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（入队失败不影响删除）", rec.Code)
	}
	if len(store.deleted) != 1 {
		t.Fatalf("入站应已删除，deleted=%v", store.deleted)
	}
}

// TestBatchDeleteClearsOnlyHysteriaHopChains 批量删除逐条清理，只清 HY2 那条。
func TestBatchDeleteClearsOnlyHysteriaHopChains(t *testing.T) {
	store := &hopCleanupStore{
		node: hopTestNode("1.2.2"),
		inbounds: map[uint64]*XrayInboundRecord{
			30: hysteriaInbound(30, 5001),
			31: {ID: 31, NodeID: 42, Tag: "vless", Protocol: "vless", Port: 8449},
		},
	}
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	d := newHopCleanupDomain(store)
	engine.POST("/nodes/:node/xray-inbounds/batch-delete", d.xrayInboundBatchDestroy)

	body := strings.NewReader(`{"ids":[30,31]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/nodes/42/xray-inbounds/batch-delete", body)
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Code int `json:"code"`
		Data struct {
			Deleted []uint64 `json:"deleted"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析响应失败: %v，body=%s", err, rec.Body.String())
	}
	if len(payload.Data.Deleted) != 2 {
		t.Fatalf("deleted = %v，期望两条", payload.Data.Deleted)
	}
	if len(store.commands) != 1 {
		t.Fatalf("清链指令数 = %d，期望 1（只清 HY2）", len(store.commands))
	}
	if got := jsonInt(jsField(store.commands[0].payload, "inbound_id")); got != 30 {
		t.Fatalf("payload.inbound_id = %d，期望 30", got)
	}
}
