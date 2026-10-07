<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\HasMany;

/**
 * 套餐模板。
 * type='period'：周期套餐（months + monthly_traffic + period_traffic）
 * type='total'：总量套餐（total_traffic）
 */
class Plan extends Model
{
    protected $fillable = [
        'name',
        'price',
        'reset_price',
        'type',
        'months',
        'monthly_traffic',
        'period_traffic',
        'total_traffic',
        'is_active',
        'include_local',
        'third_party_sub_ids',
    ];

    protected function casts(): array
    {
        return [
            'months' => 'integer',
            'monthly_traffic' => 'integer',
            'period_traffic' => 'integer',
            'total_traffic' => 'integer',
            'is_active' => 'boolean',
            'include_local' => 'boolean',
            'third_party_sub_ids' => 'array',
        ];
    }

    /** 该套餐注入的第三方订阅 ID 列表（third_party_sub_ids JSON 数组，空 = 不含第三方） */
    public function thirdPartySubIdList(): array
    {
        return array_values(array_filter((array) ($this->third_party_sub_ids ?? [])));
    }

    /** 该套餐是否包含本地节点（老套餐默认 true，行为不变） */
    public function includesLocal(): bool
    {
        return (bool) ($this->include_local ?? true);
    }

    public function users(): HasMany
    {
        return $this->hasMany(User::class);
    }

    public function orders(): HasMany
    {
        return $this->hasMany(Order::class);
    }

    /** 是否周期套餐 */
    public function isPeriod(): bool
    {
        return $this->type === 'period';
    }

    /** 是否总量套餐 */
    public function isTotal(): bool
    {
        return $this->type === 'total';
    }
}
