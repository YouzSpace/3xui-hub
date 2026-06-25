<?php

namespace App\Models;

use Database\Factories\UserFactory;
use Illuminate\Database\Eloquent\Attributes\Fillable;
use Illuminate\Database\Eloquent\Attributes\Hidden;
use Illuminate\Database\Eloquent\Factories\HasFactory;
use Illuminate\Database\Eloquent\Relations\BelongsTo;
use Illuminate\Foundation\Auth\User as Authenticatable;
use Illuminate\Notifications\Notifiable;

/**
 * ControlHub 用户（system-design §5.1）。
 * 与 3x-ui client 的对应：email 字段 = "ch_user_{id}"（在 M5 端点中使用）。
 * token 为长期订阅/登录令牌，uuid 为 Xray client id。
 */
#[Fillable([
    'email',
    'password',
    'token',
    'uuid',
    'protocol',
    'plan_id',
    'traffic_limit',
    'traffic_used',
    'monthly_traffic_used',
    'monthly_traffic_limit',
    'expired_at',
    'enabled',
])]
#[Hidden(['token', 'uuid'])]
class User extends Authenticatable
{
    /** @use HasFactory<UserFactory> */
    use HasFactory, Notifiable;

    protected function casts(): array
    {
        return [
            'expired_at' => 'datetime',
            'enabled' => 'boolean',
            'traffic_limit' => 'integer',
            'traffic_used' => 'integer',
            'monthly_traffic_used' => 'integer',
            'monthly_traffic_limit' => 'integer',
            'plan_id' => 'integer',
        ];
    }

    public function plan(): BelongsTo
    {
        return $this->belongsTo(Plan::class);
    }

    /**
     * 该用户在 3x-ui 各节点上对应的 client email（M5 主键）。
     */
    public function clientEmail(): string
    {
        return 'ch_user_' . $this->id;
    }

    /** 是否绑定周期套餐 */
    public function isPeriodPlan(): bool
    {
        return $this->plan?->type === 'period';
    }

    /** 是否绑定总量套餐 */
    public function isTotalPlan(): bool
    {
        return $this->plan?->type === 'total';
    }
}
