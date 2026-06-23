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
        $attached = []; // [nodeId => newInboundId] 已 attach，用于回滚

        try {
            DB::transaction(function () use ($user, $newProtocol, $oldProtocol, $email, &$attached) {
                foreach (Node::where('enabled', true)->get() as $node) {
                    $newInbound = $node->inboundIdFor($newProtocol);
                    $oldInbound = $node->inboundIdFor($oldProtocol);

                    if ($newInbound === null) {
                        continue; // 该节点无新协议 inbound，跳过（订阅不含此节点）
                    }

                    $client = $this->clientFactory->forNode($node);

                    if ($oldInbound !== null && $oldInbound !== $newInbound) {
                        $client->attachClient($email, [$newInbound]);
                        $attached[$node->id] = $newInbound;
                        $client->detachClient($email, [$oldInbound]);
                    } else {
                        // 无旧 inbound（首次），仅 attach
                        $client->attachClient($email, [$newInbound]);
                        $attached[$node->id] = $newInbound;
                    }
                }

                $user->forceFill(['protocol' => $newProtocol])->save();
            });
        } catch (\Throwable $e) {
            // 回滚：detach 已 attach 的新 inbound，恢复旧
            foreach ($attached as $nodeId => $newInbound) {
                $node = Node::find($nodeId);
                if (!$node) {
                    continue;
                }
                try {
                    $this->clientFactory->forNode($node)->detachClient($email, [$newInbound]);
                    $oldInbound = $node->inboundIdFor($oldProtocol);
                    if ($oldInbound !== null) {
                        $this->clientFactory->forNode($node)->attachClient($email, [$oldInbound]);
                    }
                } catch (\Throwable $rollbackErr) {
                    report($rollbackErr);
                }
            }
            throw $e;
        }
    }
}
