<?php

namespace App\Jobs;

use App\Models\Node;
use Illuminate\Bus\Queueable;
use Illuminate\Contracts\Queue\ShouldQueue;
use Illuminate\Foundation\Bus\Dispatchable;
use Illuminate\Queue\InteractsWithQueue;
use Illuminate\Queue\SerializesModels;

/**
 * 流量同步总 Job（M7.2）：遍历 enabled 节点，每节点派发 SyncNodeTrafficJob。
 * 失败隔离——单节点失败不影响其余。
 */
class SyncTrafficJob implements ShouldQueue
{
    use Dispatchable, InteractsWithQueue, Queueable, SerializesModels;

    public function handle(): void
    {
        Node::where('enabled', true)->each(function (Node $node) {
            SyncNodeTrafficJob::dispatch($node->id);
        });
    }
}
