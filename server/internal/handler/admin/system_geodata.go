package admin

// 管理端 Geo 数据（/admin-api/geodata…）。
//
// 逐条对齐 backend/app/Http/Controllers/Admin/GeodataController.php。
// 文件下载 / 节点分发的实际工作在 GeodataService 接缝后面。

import (
	"github.com/gin-gonic/gin"
)

// geodataIndex 对齐 GET /admin-api/geodata。
func (d *SystemDomain) geodataIndex(c *gin.Context) {
	if d.geo == nil {
		d.fail(c, "geodata 服务未配置", 500)
		return
	}
	files, err := d.geo.Status(ctxOf(c))
	if err != nil {
		d.failServer(c, "读取 geo 文件状态", err)
		return
	}
	nodes, err := d.geo.NodeStatuses(ctxOf(c))
	if err != nil {
		d.failServer(c, "读取节点 geo 状态", err)
		return
	}
	filesOut := make(jsArr, 0, len(files))
	for i := range files {
		filesOut = append(filesOut, presentGeodataFile(&files[i]))
	}
	nodesOut := make(jsArr, 0, len(nodes))
	for i := range nodes {
		nodesOut = append(nodesOut, presentGeodataNode(&nodes[i]))
	}
	d.success(c, jo("files", filesOut, "nodes", nodesOut))
}

// geodataUpdate 对齐 POST /admin-api/geodata/update（同步执行）。
func (d *SystemDomain) geodataUpdate(c *gin.Context) {
	if d.geo == nil {
		d.fail(c, "geodata 服务未配置", 500)
		return
	}
	results, err := d.geo.Update(ctxOf(c))
	if err != nil {
		d.failServer(c, "更新 geo 文件", err)
		return
	}
	failed, updated := 0, 0
	for i := range results {
		switch results[i].Status {
		case "failed":
			failed++
		case "updated":
			updated++
		}
	}

	msg := "已是最新，无需更新"
	if failed > 0 {
		msg = "部分文件更新失败，详见结果"
	} else if updated > 0 {
		msg = "已更新 " + itoaInt(updated) + " 个文件，可分发到节点"
	}
	d.success(c, jo("results", resultMap(results)), msg)
}

// resultMap 对齐 PHP `$results` 的 map 形态（键 = 文件名）。
func resultMap(results []GeodataUpdateResult) *jsObj {
	out := &jsObj{}
	for _, r := range results {
		item := jo("status", r.Status)
		if r.Status != "failed" {
			item.Put("size", r.Size)
			item.Put("sha256", r.SHA256)
		}
		if r.Message != "" {
			item.Put("message", r.Message)
		}
		out.Put(r.Name, item)
	}
	return out
}

// geodataDispatch 对齐 POST /admin-api/geodata/dispatch。
func (d *SystemDomain) geodataDispatch(c *gin.Context) {
	if d.geo == nil {
		d.fail(c, "geodata 服务未配置", 500)
		return
	}
	queued, skipped, missing, err := d.geo.DispatchToNodes(ctxOf(c))
	if err != nil {
		d.failServer(c, "分发 geo 文件", err)
		return
	}
	if missing {
		d.fail(c, "面板上还没有 geo 文件，请先执行「检查并更新」", 400)
		return
	}
	msg := "已下发 " + itoaInt(queued) + " 个节点"
	if skipped > 0 {
		msg += "（" + itoaInt(skipped) + " 个节点已有在途任务，跳过）"
	}
	d.success(c, jo("queued", queued, "skipped", skipped, "missing", missing), msg)
}

func presentGeodataFile(f *GeodataFile) *jsObj {
	var updatedAt, checkedAt any
	if f.UpdatedAt != nil {
		updatedAt = *f.UpdatedAt
	}
	if f.CheckedAt != nil {
		checkedAt = *f.CheckedAt
	}
	return jo(
		"name", f.Name,
		"exists", f.Exists,
		"size", f.Size,
		"sha256", f.SHA256,
		"updated_at", updatedAt,
		"checked_at", checkedAt,
	)
}

func presentGeodataNode(n *GeodataNodeStatus) *jsObj {
	var status, result any
	if n.Status != nil {
		status = *n.Status
	}
	if n.Result != nil {
		result = *n.Result
	}
	return jo(
		"node_id", n.NodeID,
		"node_name", n.NodeName,
		"enabled", n.Enabled,
		"status", status,
		"result", result,
		"finished_at", nullableISO(n.FinishedAt),
	)
}
