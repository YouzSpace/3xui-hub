<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;

/**
 * 第三方订阅链接（外部机场）。
 * 后台定时拉取 → 解析成节点链接缓存 → 按套餐配置注入用户订阅。
 * url 含机场 token，属敏感信息，仅管理端展示；节点在用户端统一命名，不暴露机场原名。
 */
class ThirdPartySub extends Model
{
    protected $table = 'third_party_subs';

    protected $fillable = [
        'name',
        'url',
        'enabled',
        'cached_links',
        'node_count',
        'last_fetched_at',
        'last_status',
        'last_error',
    ];

    protected function casts(): array
    {
        return [
            'enabled' => 'boolean',
            'node_count' => 'integer',
            'last_fetched_at' => 'datetime',
        ];
    }

    /**
     * 缓存的节点链接（多行拆成数组，跳过空行）。
     * 拉取失败时保留上次成功缓存（cached_links 不清空），所以这里是「最后可用」的链接。
     */
    public function getCachedLinks(): array
    {
        if (!$this->cached_links) {
            return [];
        }

        $lines = preg_split('/\r\n|\r|\n/', (string) $this->cached_links);

        return array_values(array_filter(array_map('trim', $lines), fn ($l) => $l !== ''));
    }
}
