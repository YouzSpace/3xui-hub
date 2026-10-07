<?php

namespace App\Http\Controllers\Api;

use App\Http\Controllers\Controller;
use App\Models\AsyncTask;
use App\Models\Node;
use App\Services\AsyncTaskService;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;

/**
 * 用户端：GET /api/me 返回当前用户信息，同时同步流量。
 */
class UserController extends Controller
{
    use ApiResponse;

    public function __construct(
        private AsyncTaskService $tasks,
    ) {}

    public function me(Request $request): \Illuminate\Http\JsonResponse
    {
        $user = $request->user();

        if (!$user) {
            return $this->error('未认证', 401);
        }

        $user->load('plan');

        return $this->success([
            'id' => $user->id,
            'email' => $user->email,
            'token' => $user->token,
            'protocol' => $user->protocol,
            'plan_id' => $user->plan_id,
            'plan_type' => $user->plan?->type,
            'plan_name' => $user->plan?->name,
            // 套餐是否含本地节点（纯第三方套餐：用户端不显示流量，只显示到期日期）
            'plan_include_local' => $user->plan ? (bool) $user->plan->includesLocal() : true,
            'plan_months' => $user->plan?->months,
            'plan_price' => (float) ($user->plan?->price ?? 0),
            'plan_reset_price' => (float) ($user->plan?->reset_price ?? 0),
            'traffic_limit' => (int) $user->traffic_limit,
            'traffic_used' => (int) $user->traffic_used,
            'monthly_traffic_used' => (int) $user->monthly_traffic_used,
            'monthly_traffic_limit' => (int) $user->monthly_traffic_limit,
            'next_traffic_reset_at' => $user->next_traffic_reset_at?->toIso8601String(),
            'expired_at' => $user->expired_at?->toIso8601String(),
            'enabled' => (bool) $user->enabled,
        ]);
    }

    /**
     * 手动同步当前用户在所有节点上的流量。
     */
    public function syncTraffic(Request $request): \Illuminate\Http\JsonResponse
    {
        $user = $request->user();
        $nodeIds = Node::where('enabled', true)->pluck('id')->map(fn ($id) => (int) $id)->all();
        $result = $this->tasks->findOrCreateTrafficSync($nodeIds, 'user', $user->id, $user->id);
        $task = $result['task'];

        return $this->success([
            'task_id' => $task->id,
            'status' => $task->status,
            'queued_nodes' => count($nodeIds),
            'already_running' => !$result['created'],
        ], $result['created'] ? '同步任务已提交' : '已有同步任务正在执行');
    }

    /**
     * 查询当前用户最近一次流量同步任务状态（供前端轮询到终态后刷新）。
     */
    public function syncTaskStatus(Request $request): \Illuminate\Http\JsonResponse
    {
        $user = $request->user();
        $task = AsyncTask::query()
            ->where('type', 'traffic_sync')
            ->where('subject_type', 'user')
            ->where('subject_id', $user->id)
            ->latest('id')
            ->first();

        if (!$task) {
            return $this->success(['task_id' => null, 'status' => null], '暂无同步任务');
        }

        return $this->success([
            'task_id' => $task->id,
            'status' => $task->status,
            'total' => $task->total,
            'completed' => $task->completed,
            'failed' => $task->failed,
            'attempts' => $task->attempts,
            'max_attempts' => $task->max_attempts,
            'error' => $this->tasks->publicError($task),
        ]);
    }
}
