<?php

namespace App\Http\Controllers\Api;

use App\Http\Controllers\Controller;
use App\Models\Node;
use App\Models\SiteConfig;
use App\Models\User;
use App\Services\TrafficSyncService;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;

/**
 * 用户端节点（M4.6）：GET /api/nodes
 * 返回当前用户可用协议下、enabled 且 status=online 的节点。
 */
class NodeController extends Controller
{
    use ApiResponse;

    public function index(Request $request): \Illuminate\Http\JsonResponse
    {
        $user = $request->user();

        $nodes = Node::where('enabled', true)
            ->where('status', 'online')
            ->where(function ($q) use ($user) {
                $q->whereHas('inbounds', function ($qq) use ($user) {
                    $qq->where('protocol', $user->protocol);
                });
                // xray 节点不落 3x-ui 入站记录：vless 用户按驱动类型纳入
                if ($user->protocol === 'vless') {
                    $q->orWhere('driver_type', 'xray');
                }
            })
            ->get();

        // 套餐不勾本地节点（纯第三方套餐）→ 本地节点列表也隐藏，与订阅注入保持一致
        if ($user->plan && !$user->plan->includesLocal()) {
            $nodes = collect();
        }

        // site_configs.show_node_multiplier：默认关（没有这行 = 关）
        $showMultiplier = in_array(SiteConfig::getValue('show_node_multiplier', ''), ['1', 'true', 'on'], true);

        $items = $nodes->map(function (Node $n) use ($showMultiplier) {
            $item = [
                'id' => $n->id,
                'name' => $n->name,
                'host' => $n->host,
                'port' => $n->isXray()
                    ? (int) ($n->driver_config['reality']['listen_port'] ?? 443)
                    : $n->port,
                'latency' => $n->latency,
                'status' => $n->status,
            ];

            // 开关关着（默认）时整个字段都不出现，行为与加倍率之前完全一致。
            // 开关开着时继承和手动设的都返回实际生效值 —— 手动设的更要让人知道。
            // 展示值与计费值必须同源：统一走 TrafficSyncService::multiplierFor()，
            // 不允许在这里再写一套取值（否则展示与计费将来必分叉）。
            if ($showMultiplier) {
                $item['traffic_multiplier'] = TrafficSyncService::multiplierFor($n);
                $item['traffic_multiplier_source'] = $n->traffic_multiplier !== null ? 'node' : 'default';
            }

            return $item;
        })->values();

        // 第三方线路：与订阅注入同源（linksForUser 已含 总开关/套餐勾选 判断）。
        // 机场节点不在面板上，测不了延迟 → latency 恒为 null，前端只显示名称+在线点。
        $items = $items->concat($this->thirdPartyNodeItems($user));

        return $this->success($items);
    }

    /**
     * 用户订阅里注入的第三方节点 → 节点列表条目（中性名「地区+序号」，机场品牌词零露出）。
     * 第三方挂了不影响本地节点列表（try/catch 兜底，与订阅生成同策略）。
     */
    private function thirdPartyNodeItems(User $user): array
    {
        try {
            $links = app(\App\Services\ThirdPartyService::class)->linksForUser($user);
        } catch (\Throwable $e) {
            report($e);
            return [];
        }

        $items = [];
        foreach ($links as $i => $link) {
            $name = '';
            if (($pos = strrpos($link, '#')) !== false) {
                $name = urldecode(substr($link, $pos + 1));
            }
            $items[] = [
                'id' => 'tp-' . $i, // 虚拟 ID（非面板节点），前端仅用于 key
                'name' => $name !== '' ? $name : '线路' . ($i + 1),
                'host' => null,
                'port' => null,
                'latency' => null,
                'status' => 'online',
            ];
        }

        return $items;
    }
}
