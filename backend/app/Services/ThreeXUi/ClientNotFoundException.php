<?php

namespace App\Services\ThreeXUi;

/**
 * 3x-ui 上「该 email 的 client 不存在」。
 *
 * 与真正的失败（网络超时 / 鉴权失败 / 5xx / 非法响应）区分开：前者对调用方意味着
 * 「这台节点无需操作这个用户」，后者意味着「没确认到，下一轮要重试」。
 * 3x-ui 对不存在的 email 返回 HTTP 200 + {success:false, msg:"... record not found"}，
 * 只差在文案上，所以由 ThreeXUiClient::isClientNotFound() 判定后包成本异常。
 */
class ClientNotFoundException extends ThreeXUiException
{
}
