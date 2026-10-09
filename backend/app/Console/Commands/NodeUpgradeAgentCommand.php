<?php

namespace App\Console\Commands;

use App\Models\Node;
use App\Services\AgentUpgradeService;
use Illuminate\Console\Command;

/**
 * 给 xray 节点下发 agent 自升级指令（入队，节点取走后自己替换二进制并重启）。
 *
 * 什么时候用：
 *   - 面板更新后（3hub update 会自动跑一次，覆盖全部 enabled 的 xray 节点）
 *   - 手动补某个节点（--node=ID）
 *   - 节点上报的版本号不可信 / 二进制被手动改过时用 --force 强制重下
 *
 * 幂等：节点已上报同版本、或已有未终态的升级指令 → 跳过。离线节点也会入队，
 * 等它上线取走即执行（这正是「不用登录每台 VPS」的意义）。
 */
class NodeUpgradeAgentCommand extends Command
{
    protected $signature = 'node:upgrade-agent
        {--node= : 只处理指定节点 id}
        {--force : 即使节点已上报最新版本也重新下发}
        {--dry-run : 只列出将要做什么，不入队}';

    protected $description = '给 xray 节点下发 agent 升级指令（下载+校验+替换由节点自己完成）';

    public function handle(AgentUpgradeService $upgrades): int
    {
        $nodes = $this->targets();
        if ($nodes->isEmpty()) {
            $this->line('没有匹配的 xray 节点。');

            return self::SUCCESS;
        }

        $queued = 0;
        $skipped = 0;

        foreach ($nodes as $node) {
            $target = $upgrades->targetFor($node);
            $reported = $upgrades->reportedVersion($node);

            if ($this->option('dry-run')) {
                $this->line(sprintf(
                    '[dry-run] node#%d %s：上报 %s → 面板 %s',
                    $node->id,
                    $node->name,
                    $reported ?? '（未上报）',
                    $target['version'] ?? '（manifest 缺失）'
                ));

                continue;
            }

            $result = $upgrades->enqueue($node, (bool) $this->option('force'));

            if ($result['queued']) {
                $queued++;
                $this->info(sprintf('node#%d %s：已下发升级指令（→ v%s）', $node->id, $node->name, (string) $result['version']));
            } else {
                $skipped++;
                $this->line(sprintf('node#%d %s：跳过（%s）', $node->id, $node->name, $result['reason']));
            }
        }

        if ($this->option('dry-run')) {
            $this->info('--dry-run：未入队任何指令。');

            return self::SUCCESS;
        }

        $this->info("完成：下发 {$queued} 个，跳过 {$skipped} 个。");
        $this->line('节点取走指令后自动下载校验替换并重启 agent；结果见 面板 → 节点管理 的指令记录，或 `3hub log`。');

        return self::SUCCESS;
    }

    /** 目标节点：--node 指定单个（不管 enabled），否则所有 enabled 的 xray 节点。 */
    private function targets(): \Illuminate\Support\Collection
    {
        $nodeId = $this->option('node');

        if ($nodeId !== null && $nodeId !== '') {
            return Node::where('id', (int) $nodeId)->get();
        }

        return Node::where('enabled', true)
            ->where('driver_type', 'xray')
            ->orderBy('id')
            ->get();
    }
}
