<?php

namespace App\Services\ThreeXUi;

/**
 * 3x-ui API 调用异常。
 *
 * 触发场景：
 * - HTTP 请求本身失败（连接超时、5xx 等，原 Guzzle 异常被包装）
 * - 响应非合法 JSON 或缺少 success 字段
 * - 3x-ui 返回 success===false（业务失败），msg 带回原因
 *
 * 不抛异常的特例：getClient / getClientTraffic 等「可能不存在」的查询，
 * 当 client 不存在时返回 null 而非抛出（见 requestNullable）。
 */
class ThreeXUiException extends \RuntimeException
{
}
