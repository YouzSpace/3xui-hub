<?php

namespace App\Http\Middleware;

use App\Models\Node;
use Closure;
use Illuminate\Http\Request;
use Symfony\Component\HttpFoundation\Response;

/**
 * xray 节点鉴权（/api/node-api/*）。
 *
 * 节点侧用 X-Node-Secret 头携带 32B 节点密钥（安装命令生成，加密入库）；
 * 面板逐个 xray 节点解密比对（hash_equals 防时序侧信道）。密钥轮换后旧
 * agent 立即 401，节点页显示离线，提示重新执行安装命令。
 */
class NodeAuth
{
    public function handle(Request $request, Closure $next): Response
    {
        $secret = (string) $request->header('X-Node-Secret', '');
        if ($secret === '') {
            return response()->json(['code' => 401, 'msg' => 'missing node secret'], 401);
        }

        foreach (Node::where('driver_type', 'xray')->get() as $node) {
            $candidate = $node->nodeSecret();
            if (is_string($candidate) && $candidate !== '' && hash_equals($candidate, $secret)) {
                $request->attributes->set('node', $node);

                return $next($request);
            }
        }

        return response()->json(['code' => 401, 'msg' => 'invalid node secret'], 401);
    }
}
