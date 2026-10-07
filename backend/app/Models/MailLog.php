<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;

/**
 * 邮件发送日志。配合 MailLogService 写入，记录每封的成败与失败原因。
 */
class MailLog extends Model
{
    protected $fillable = [
        'type',
        'status',
        'to_email',
        'to_user_id',
        'subject',
        'error',
        'scene',
        'batch_id',
    ];

    protected function casts(): array
    {
        return [
            'to_user_id' => 'integer',
            'batch_id'   => 'integer',
        ];
    }

    public const TYPE_NOTIFY   = 'notify';
    public const TYPE_BATCH    = 'batch';
    public const TYPE_TEST     = 'test';
    public const TYPE_SCHEDULE = 'schedule';

    public const STATUS_SENT   = 'sent';
    public const STATUS_FAILED = 'failed';
}