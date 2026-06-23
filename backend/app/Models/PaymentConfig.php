<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;

/**
 * 支付配置。
 */
class PaymentConfig extends Model
{
    protected $fillable = [
        'name',
        'gateway',
        'query_gateway',
        'member_id',
        'api_key',
        'notify_url',
        'bank_code',
        'enabled',
    ];

    protected $hidden = ['api_key'];

    protected function casts(): array
    {
        return [
            'enabled' => 'boolean',
        ];
    }
}
