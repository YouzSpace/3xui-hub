<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Attributes\Fillable;
use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\BelongsTo;

#[Fillable(['node_id', 'protocol', 'inbound_id'])]
class NodeInbound extends Model
{
    protected $table = 'node_inbounds';

    public function node(): BelongsTo
    {
        return $this->belongsTo(Node::class);
    }
}
