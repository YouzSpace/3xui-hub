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
        'type',
        'months',
        'monthly_traffic',
        'period_traffic',
        'total_traffic',
    ];

    protected function casts(): array
    {
        return [
            'months' => 'integer',
            'monthly_traffic' => 'integer',
            'period_traffic' => 'integer',
            'total_traffic' => 'integer',
        ];
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
