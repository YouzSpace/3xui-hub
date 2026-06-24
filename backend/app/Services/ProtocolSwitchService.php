<?php

namespace App\Services;

use App\Models\Node;
use App\Models\User;
use Illuminate\Support\Facades\DB;

/**
 * 协议切换服务（M10）。事务化 attach/detach。
 * client email/uuid 全程不变 → 订阅地址不变，用户更新订阅即生效。
 *
 * 流程（每节点）：attachClient(新 inbound) 成功后 detachClient(旧 inbound)。
 * 任一失败 → 回滚已 attach 的新 inbound，保留旧，users.protocol 不变。
 */
class ProtocolSwitchService
{
    public function __construct(private ThreeXUiClientFactory $clientFactory)
    {
    }

    public function switch(User $user, string $newProtocol): void
    {
        if (!in_array($newProtocol, ['vless', 'trojan'], true)) {
            throw new \DomainException('不支持的协议');
        }

        $oldProtocol = $user->protocol;
        if ($oldProtocol === $newProtocol) {
            return;
        }

        $email = $user->clientEmail();
        $attached = []; // [nodeId => [newInboundIds]] 已 attach，用于回滚

        try {
            DB::transaction(function () use ($user, $newProtocol, $oldProtocol, $email, &$attached) {
                foreach (Node::where('enabled', true)->get() as $node) {
                    $newInbounds = $node->inboundIdsFor($newProtocol);
                    $oldInbounds = $node->inboundIdsFor($oldProtocol);

                    if (empty($newInbounds)) {
                        continue; // 该节点无新协议 inbound，跳过
                    }

                    $client = $this->clientFactory->forNode($node);

                    // attach 所有新协议 inbound
                    $client->attachClient($email, $newInbounds);
                    $attached[$node->id] = $newInbounds;

                    // detach 所有旧协议 inbound（排除和新协议相同的）
                    $toDetach = array_diff($oldInbounds, $newInbounds);
                    if (!empty($toDetach)) {
                        $client->detachClient($email, array_values($toDetach));
                    }
                }

                $user->forceFill(['protocol' => $newProtocol])->save();
            });
        } catch (\Throwable $e) {
            // 回滚：detach 已 attach 的新 inbound，恢复旧
            foreach ($attached as $nodeId => $newInbounds) {
                $node = Node::find($nodeId);
                if (!$node) {
                    continue;
                }
                try {
                    $this->clientFactory->forNode($node)->detachClient($email, $newInbounds);
                    $oldInbounds = $node->inboundIdsFor($oldProtocol);
                    if (!empty($oldInbounds)) {
                        $this->clientFactory->forNode($node)->attachClient($email, $oldInbounds);
                    }
                } catch (\Throwable $rollbackErr) {
                    report($rollbackErr);
                }
            }
            throw $e;
        }
    }
}
