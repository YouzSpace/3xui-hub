<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Attributes\Fillable;
use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\BelongsTo;

/**
 * xray 节点路由规则（按 sort 渲染进 routing.rules，排在内置基础规则之后）。
 */
#[Fillable([
    'node_id',
    'domains',
    'ips',
    'port',
    'network',
    'protocol',
    'inbound_tag',
    'outbound_tag',
    'remark',
    'enabled',
    'sort',
])]
class XrayRoutingRule extends Model
{
    protected function casts(): array
    {
        return [
            'domains' => 'array',
            'ips' => 'array',
            'enabled' => 'boolean',
            'sort' => 'integer',
        ];
    }

    public function node(): BelongsTo
    {
        return $this->belongsTo(Node::class);
    }

    public static function enabledFor(Node $node): \Illuminate\Database\Eloquent\Collection
    {
        return static::where('node_id', $node->id)
            ->where('enabled', true)
            ->orderBy('sort')
            ->orderBy('id')
            ->get();
    }
}
