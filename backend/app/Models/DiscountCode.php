<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\BelongsTo;

/**
 * 折扣码（购买折扣码）。
 * 三种来源共用一张表：
 * - invite：邀请码，系统给每个用户自动生成，别人可用，每个用户一生只能用一次
 * - redeem：兑换码，被邀请人支付成功后自动发给邀请人，仅本人可用，一次性
 * - admin：管理员优惠码，全场景可用，受全站总次数上限约束
 */
class DiscountCode extends Model
{
    public const SOURCE_INVITE = 'invite';
    public const SOURCE_REDEEM = 'redeem';
    public const SOURCE_ADMIN = 'admin';

    public const STATUS_ACTIVE = 'active';
    public const STATUS_USED_UP = 'used_up';
    public const STATUS_EXPIRED = 'expired';
    public const STATUS_REFRESHED = 'refreshed';

    protected $fillable = [
        'code',
        'source',
        'user_id',
        'discount',
        'max_uses',
        'used_count',
        'max_uses_per_user',
        'expires_at',
        'note',
        'status',
        'plan_id',
        'source_order_id',
        'refreshed_at',
        'next_refresh_at',
    ];

    protected function casts(): array
    {
        return [
            'discount' => 'decimal:2',
            'expires_at' => 'datetime',
            'refreshed_at' => 'datetime',
            'next_refresh_at' => 'datetime',
            'user_id' => 'integer',
            'plan_id' => 'integer',
            'source_order_id' => 'integer',
            'max_uses' => 'integer',
            'used_count' => 'integer',
            'max_uses_per_user' => 'integer',
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

    /**
     * 该码是否可用：状态 active、未用满、未过期。
     */
    public function isUsable(): bool
    {
        if ($this->status !== self::STATUS_ACTIVE) {
            return false;
        }
        if ($this->used_count >= $this->max_uses) {
            return false;
        }
        if ($this->expires_at && $this->expires_at->isPast()) {
            return false;
        }

        return true;
    }

    /** 码体字符集：Base32 去掉易混的 0 O 1 I L（手抄邀请码时最容易被认错的那几个）。 */
    private const CODE_ALPHABET = '23456789ABCDEFGHJKMNPQRSTUVWXYZ';

    /** 码体长度：8 位 Base32（老码是 6 位十六进制，只有 1677 万种，脚本穷举得动）。 */
    private const CODE_LENGTH = 8;

    /**
     * 生成一个唯一码：3 位前缀 + 8 位 Base32 随机串，中间无分隔符（如 INVK7M2P9QT）。
     * 循环查重确保唯一（code 上有 unique 索引，兜底）。
     *
     * 前缀固定三位（INV / GIF / PRO），只作人眼区分类别用，**不参与校验**：
     * 校验一律按整串精确查库、以 source 字段判类别，不做任何前缀解析。
     */
    public static function generateCode(string $prefix): string
    {
        do {
            $code = strtoupper($prefix) . self::randomBody();
        } while (static::where('code', $code)->exists());

        return $code;
    }

    /**
     * 取一个 8 位码体，密码学安全随机源。
     *
     * 逐字符取 random_bytes 的 1 个字节；尾部余数直接丢弃重取，保证每个字符等概率
     * （字符集 31 个，256 = 8×31 + 8，不做这一步的话前 8 个字符会略偏多）。
     */
    private static function randomBody(): string
    {
        $alphabet = self::CODE_ALPHABET;
        $size     = strlen($alphabet);
        $limit    = intdiv(256, $size) * $size;
        $body     = '';

        while (strlen($body) < self::CODE_LENGTH) {
            $byte = ord(random_bytes(1));
            if ($byte >= $limit) {
                continue;
            }
            $body .= $alphabet[$byte % $size];
        }

        return $body;
    }
}
