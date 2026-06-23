<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Attributes\Fillable;
use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\BelongsTo;

/**
 * 用户 Bearer access_token（ApiAuth 中间件校验）。
 * 由 POST /api/login 用 users.token 换取。
 */
#[Fillable(['user_id', 'token', 'expires_at', 'last_used_at'])]
class AccessToken extends Model
{
    protected function casts(): array
    {
        return [
            'expires_at' => 'datetime',
            'last_used_at' => 'datetime',
        ];
    }

    public function user(): BelongsTo
    {
        return $this->belongsTo(User::class);
    }

    /**
     * 是否过期（expires_at 为 null 表示永不过期）。
     */
    public function isExpired(): bool
    {
        return $this->expires_at !== null && $this->expires_at->isPast();
    }
}
