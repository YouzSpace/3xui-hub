<?php

namespace App\Http\Middleware;

use App\Models\OperationLog;
use Closure;
use Illuminate\Http\Request;
use Symfony\Component\HttpFoundation\Response;

/**
 * 操作日志记录中间件。
 * - admin-api 写操作（POST/PUT/DELETE）→ type=admin, actor=管理员 username
 * - 任何 4xx/5xx → 记录报错（type=system 或 user）
 * 保留最多 500 条，超出自动清理。
 */
class RecordOperationLog
{
    public function handle(Request $request, Closure $next): Response
    {
        $response = $next($request);

        // 只处理 admin-api 和用户 api
        $isApi = $request->is('admin-api/*') || $request->is('api/*');
        // 节点 agent 的高频上报（register/push/alive/execute/ack）不写操作日志，避免刷爆
        if (!$isApi || $request->is('api/ping') || $request->is('api/node-api/*')) {
            return $response;
        }

        $method = $request->method();
        $status = $response->getStatusCode();
        $error = null;

        // 从 response 中提取业务 error
        if ($response instanceof \Illuminate\Http\JsonResponse && $status >= 400) {
            $content = json_decode($response->getContent(), true);
            $error = $content['msg'] ?? $content['message'] ?? null;
        }

        $type = 'system';
        $actorId = null;
        $actorName = null;

        if ($request->is('admin-api/*')) {
            $type = 'admin';
            $actorId = $request->session()->get('admin_id');
            $admin = $request->user();
            $actorName = $admin?->username ?? 'unknown';
        } elseif ($request->is('api/*')) {
            $type = 'user';
            $user = $request->user();
            $actorId = $user?->id;
            $actorName = $user?->email ?? null;
        }

        $action = $this->describeAction($request);

        // 记录条件：
        // - 写操作（POST/PUT/DELETE）无论成败都记（这就是"操作"）
        // - 5xx 错误无论读写都记（报错信息）
        // 成功/失败的 GET 读请求（含登录轮询）不记，避免日志刷爆
        $isWrite = in_array($method, ['POST', 'PUT', 'DELETE'], true);
        if ($isWrite || $status >= 500) {
            $record = OperationLog::create([
                'type'       => $type,
                'actor_id'   => $actorId,
                'actor_name' => $actorName,
                'action'     => $action,
                'method'     => $method,
                'path'       => $request->path(),
                'status'     => $status,
                'error'      => $error ? mb_substr($error, 0, 500) : null,
                'ip'         => $request->ip(),
                'created_at' => now(),
            ]);

            // 清理超出部分（保留最近 500 条）
            $overflow = OperationLog::count() - 500;
            if ($overflow > 0) {
                $oldestIds = OperationLog::orderBy('id')->limit(min($overflow, 200))->pluck('id');
                OperationLog::whereIn('id', $oldestIds)->delete();
            }
        }

        return $response;
    }

    protected function describeAction(Request $request): string
    {
        $method = strtoupper($request->method());
        $path = $request->path();

        $map = [
            'admin-api/login'           => '登录',
            'admin-api/logout'          => '退出',
            'admin-api/change-password' => '修改密码',
            'admin-api/update-username' => '修改用户名',
            'admin-api/users'           => '管理用户',
            'admin-api/nodes'           => '管理节点',
            'admin-api/plans'          => '管理套餐',
            'admin-api/orders'          => '查看订单',
            'admin-api/sync-traffic'    => '同步流量',
            'admin-api/backup/export'   => '导出备份',
            'admin-api/backup/import'   => '导入备份',
            'admin-api/backup/preview'  => '预览备份',
            'admin-api/payments'        => '管理支付',
            'admin-api/email'           => '配置邮箱',
            'admin-api/site-settings'   => '修改站点设置',
            'admin-api/subscription-settings' => '修改订阅设置',
            'admin-api/tutorials'       => '管理教程',
            'admin-api/announcement'    => '发布公告',
            'admin-api/async-tasks'     => '异步任务',
            'admin-api/google2fa'       => '2FA设置',
            'admin-api/system/status'   => '查看系统状态',
            'api/login'                 => '用户登录',
            'api/register'              => '用户注册',
            'api/sync-traffic'          => '用户同步流量',
            'api/payment/create'        => '用户创建订单',
            'api/payment/notify'        => '支付回调',
        ];

        foreach ($map as $prefix => $desc) {
            if (str_starts_with($path, $prefix)) {
                return $desc;
            }
        }

        return "$method /$path";
    }
}
