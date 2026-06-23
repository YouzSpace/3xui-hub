<?php

namespace App\Traits;

use Illuminate\Http\JsonResponse;

/**
 * 统一 API 响应封装。
 * 成功：{code:0, msg, data}
 * 失败：{code, msg}
 * code === 0 表示成功，非 0 表示错误。
 */
trait ApiResponse
{
    protected function success(mixed $data = null, string $msg = 'ok', int $httpStatus = 200): JsonResponse
    {
        return response()->json([
            'code' => 0,
            'msg' => $msg,
            'data' => $data,
        ], $httpStatus);
    }

    protected function error(string $msg, int $code = 400, int $httpStatus = 200): JsonResponse
    {
        return response()->json([
            'code' => $code,
            'msg' => $msg,
            'data' => null,
        ], $httpStatus);
    }
}
