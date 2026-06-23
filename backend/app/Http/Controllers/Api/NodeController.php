<?php

namespace App\Http\Controllers\Api;

use App\Http\Controllers\Controller;
use App\Models\Node;
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
            ->whereHas('inbounds', function ($q) use ($user) {
                $q->where('protocol', $user->protocol);
            })
            ->get();

        return $this->success($nodes->map(fn (Node $n) => [
            'id' => $n->id,
            'name' => $n->name,
            'host' => $n->host,
            'port' => $n->port,
            'latency' => $n->latency,
            'status' => $n->status,
        ])->values());
    }
}
