<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Builder;
use Illuminate\Database\Eloquent\Attributes\Fillable;
use Illuminate\Database\Eloquent\Model;

/**
 * xray 节点 API 指令队列（双通道的「API 通道」执行单元）。
 *
 * Driver 侧 enqueue() 下发一条 xray API 指令 → agent 经 /node-api/execute
 * 长轮询取走（acknowledge）→ 本地执行 `xray api adu/rmu` → 回执 success/failed。
 * payload 格式见迁移注释（adu=完整入站片段，rmu={tag,emails}）。
 */
#[Fillable([
    'node_id',
    'user_id',
    'action',
    'payload',
    'status',
    'result',
    'acknowledged_at',
    'executed_at',
])]
class NodeApiCommand extends Model
{
    public const ACTION_ADD_USER = 'adu';

    public const ACTION_REMOVE_USER = 'rmu';

    /** agent 自升级（面板入队 → 节点下载校验后替换自己，见 agent/upgrade.go）。 */
    public const ACTION_UPGRADE_AGENT = 'upgrade';

    /**
     * Reality 目标探测（入站表单「检测目标」→ 节点侧真实 TLS 握手），
     * payload = {"targets":["host:port", ...]}，result 存结果 JSON 数组。
     */
    public const ACTION_REALITY_SCAN = 'reality_scan';

    public const STATUS_PENDING = 'pending';

    public const STATUS_ACKNOWLEDGED = 'acknowledged';

    public const STATUS_SUCCESS = 'success';

    public const STATUS_FAILED = 'failed';

    protected function casts(): array
    {
        return [
            'user_id' => 'integer',
            'payload' => 'array',
            'acknowledged_at' => 'datetime',
            'executed_at' => 'datetime',
        ];
    }

    /** 取某节点待执行的指令（agent execute 长轮询，按时间序最多 $limit 条）。 */
    public static function pendingFor(int $nodeId, int $limit = 5): \Illuminate\Database\Eloquent\Collection
    {
        return static::where('node_id', $nodeId)
            ->where('status', self::STATUS_PENDING)
            ->orderBy('id')
            ->limit($limit)
            ->get();
    }

    /** agent 取走指令：pending → acknowledged（原子更新，防多 agent 重取）。 */
    public function acknowledge(): bool
    {
        return static::whereKey($this->id)
            ->where('status', self::STATUS_PENDING)
            ->update([
                'status' => self::STATUS_ACKNOWLEDGED,
                'acknowledged_at' => now(),
            ]) > 0;
    }

    /** agent 回执：记录 xray 命令执行结果。 */
    public function complete(bool $ok, ?string $result = null): void
    {
        $this->forceFill([
            'status' => $ok ? self::STATUS_SUCCESS : self::STATUS_FAILED,
            'result' => $result,
            'executed_at' => now(),
        ])->save();
    }

    /** 某节点尚未终态的指令数（0 = 队列已清空，agent 可安全全量兜底同步）。 */
    public static function inFlightCount(int $nodeId): int
    {
        return static::where('node_id', $nodeId)
            ->whereIn('status', [self::STATUS_PENDING, self::STATUS_ACKNOWLEDGED])
            ->count();
    }

    /** pending 指令的查询构建器。 */
    public function scopePending(Builder $q): Builder
    {
        return $q->where('status', self::STATUS_PENDING);
    }
}
