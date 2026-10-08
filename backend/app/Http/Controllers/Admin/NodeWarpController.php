<?php

namespace App\Http\Controllers\Admin;

use App\Drivers\Xray\WarpService;
use App\Http\Controllers\Controller;
use App\Models\Node;
use App\Models\NodeWarpAccount;
use App\Traits\ApiResponse;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;

/**
 * Admin xray 节点 WARP 管理（/admin-api/nodes/{node}/warp）。
 *
 * 生命周期：register（注册设备）→ apply（生成 wireguard 出站下发节点）
 * → rotate（一键换 IP）/ license（WARP+）/ auto_rotate_hours（定时换 IP）。
 */
class NodeWarpController extends Controller
{
    use ApiResponse;

    /** GET /admin-api/nodes/{node}/warp */
    public function show(Node $node): JsonResponse
    {
        $this->ensureXray($node);

        return $this->success($this->present($node));
    }

    /** POST /admin-api/nodes/{node}/warp/register */
    public function register(Node $node): JsonResponse
    {
        $this->ensureXray($node);

        try {
            (new WarpService($node))->register();
        } catch (\Throwable $e) {
            return $this->error('WARP 注册失败：' . $e->getMessage());
        }

        return $this->success($this->present($node->fresh()));
    }

    /** POST /admin-api/nodes/{node}/warp/apply */
    public function apply(Node $node): JsonResponse
    {
        $this->ensureXray($node);

        try {
            (new WarpService($node))->apply();
        } catch (\Throwable $e) {
            return $this->error($e->getMessage());
        }

        return $this->success([
            ...$this->present($node->fresh()),
            'config_version' => $node->fresh()->xrayConfigVersion(),
        ]);
    }

    /** POST /admin-api/nodes/{node}/warp/rotate —— 一键换 IP */
    public function rotate(Node $node): JsonResponse
    {
        $this->ensureXray($node);

        try {
            (new WarpService($node))->rotate();
        } catch (\Throwable $e) {
            return $this->error('换 IP 失败：' . $e->getMessage());
        }

        return $this->success([
            ...$this->present($node->fresh()),
            'config_version' => $node->fresh()->xrayConfigVersion(),
        ]);
    }

    /** PUT /admin-api/nodes/{node}/warp —— {license?, auto_rotate_hours?} */
    public function update(Request $request, Node $node): JsonResponse
    {
        $this->ensureXray($node);

        $data = $request->validate([
            'license' => ['nullable', 'string', 'max:128'],
            'auto_rotate_hours' => ['nullable', 'integer', 'min:0', 'max:720'],
        ]);

        $account = $node->warpAccount;
        if ($account === null) {
            return $this->error('尚未注册 WARP，请先注册');
        }

        if (! empty($data['license'])) {
            try {
                (new WarpService($node))->setLicense((string) $data['license']);
            } catch (\Throwable $e) {
                return $this->error($e->getMessage());
            }
        }

        if (array_key_exists('auto_rotate_hours', $data)) {
            $account->auto_rotate_hours = (int) $data['auto_rotate_hours'];
            $account->save();
        }

        return $this->success($this->present($node->fresh()));
    }

    /** DELETE /admin-api/nodes/{node}/warp */
    public function destroy(Node $node): JsonResponse
    {
        $this->ensureXray($node);

        (new WarpService($node))->remove();

        return $this->success(['config_version' => $node->fresh()->xrayConfigVersion()]);
    }

    // ── 内部 ─────────────────────────────────────────────

    private function ensureXray(Node $node): void
    {
        if (! $node->isXray()) {
            abort(422, '仅 xray 节点支持该操作');
        }
    }

    private function present(Node $node): array
    {
        $account = $node->warpAccount;
        $outbound = $node->xrayOutbounds()->where('tag', 'warp')->first();

        return [
            'registered' => $account !== null,
            'applied' => $outbound !== null && $outbound->enabled,
            'device_id' => $account?->device_id,
            'peer_endpoint' => $account?->peer_endpoint,
            'addresses' => array_values((array) ($account?->addresses ?? [])),
            'reserved' => array_values((array) ($account?->reserved ?? [])),
            'license_set' => is_string($account?->license_key) && strlen((string) $account->license_key) >= 26,
            'auto_rotate_hours' => (int) ($account?->auto_rotate_hours ?? 0),
            'last_rotate_at' => $account?->last_rotate_at?->toIso8601String(),
            'updated_at' => $account?->updated_at?->toIso8601String(),
        ];
    }
}
