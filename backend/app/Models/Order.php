<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\BelongsTo;

/**
 * 订单。
 */
class Order extends Model
{
    protected $fillable = [
        'order_no',
        'user_id',
        'plan_id',
        'amount',
        'status',
        'payment_config_id',
        'trade_no',
        'paid_at',
        'pay_ip',
    ];

    protected function casts(): array
    {
        return [
            'amount' => 'decimal:2',
            'paid_at' => 'datetime',
        ];
    }

    public function user(): BelongsTo
    {
        return $this->belongsTo(User::class);
    }

    public function plan(): BelongsTo
    {
        return $this->belongsTo(Plan::class);
    }

    public function paymentConfig(): BelongsTo
    {
        return $this->belongsTo(PaymentConfig::class);
    }

    /** 生成订单号 */
    public static function generateOrderNo(): string
    {
        return date('YmdHis') . str_pad(random_int(0, 999999), 6, '0', STR_PAD_LEFT);
    }
}
