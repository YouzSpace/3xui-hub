<?php

namespace App\Http\Controllers\Api;

use App\Http\Controllers\Controller;
use App\Services\ProtocolSwitchService;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;

/**
 * 用户端协议切换（M10.1）：POST /api/protocol {protocol}。
 */
class ProtocolController extends Controller
{
    use ApiResponse;

    public function __construct(private ProtocolSwitchService $service)
    {
    }

    public function switch(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->validate([
            'protocol' => ['required', 'in:vless,trojan'],
        ]);

        $user = $request->user();

        try {
            $this->service->switch($user, $data['protocol']);
        } catch (\DomainException $e) {
            return $this->error($e->getMessage(), 400);
        } catch (\Throwable $e) {
            report($e);
            return $this->error('协议切换失败：' . $e->getMessage(), 500);
        }

        return $this->success(['protocol' => $user->fresh()->protocol], '切换成功');
    }
}
