<?php

namespace App\Services;

use App\Drivers\NodeDriverFactory;
use App\Jobs\ProvisionClientJob;
use App\Models\Node;
use App\Models\Plan;
use App\Models\User;
use Illuminate\Support\Carbon;
use Illuminate\Support\Str;

/**
 * 用户管理服务（M3 + M13 套餐联动）。
 * - 创建用户：自动生成 token(sub_+32)、uuid(v4)，在各节点 provision
 * - provisionAllUsersToNode：新节点创建后，把已有用户同步过去
 * - resetTraffic / renew：流量重置和续费
 */
class UserAdminService
{
    public function __construct(private NodeDriverFactory $driverFactory)
    {
    }

    /**
     * 切换套餐时重新计算用户的流量限额和到期时间。
     * 无套餐 → 清零限额、永不过期。
     * 切换套餐 → 重置已用流量，直接使用新套餐额度。
     */
    public function applyPlan(User $user): void
    {
        // 购买/续费/管理员改套餐 → 恢复该用户所有自动通知：
        // 递增时代号让旧的「已发过」标记全部作废，下个周期再触发条件会重新提醒。
        MailNotifyService::restoreReminders($user);

        $plan = $user->plan;

        if (!$plan) {
            $user->forceFill([
                'traffic_limit' => 0,
                'traffic_used' => 0,
                'monthly_traffic_used' => 0,
                'monthly_traffic_limit' => 0,
                'next_traffic_reset_at' => null,
                'expired_at' => null,
            ])->save();
            return;
        }

        if ($plan->isPeriod()) {
            $user->forceFill([
                'traffic_limit' => $plan->period_traffic ?? 0,
                'traffic_used' => 0,
                'monthly_traffic_used' => 0,
                'monthly_traffic_limit' => $plan->monthly_traffic ?? 0,
                'expired_at' => Carbon::now()->addMonths($plan->months)->toDateString(),
                'next_traffic_reset_at' => Carbon::now()->addDays(30),
            ])->save();
        } else {
            $user->forceFill([
                'traffic_limit' => $plan->total_traffic ?? 0,
                'traffic_used' => 0,
                'monthly_traffic_used' => 0,
                'monthly_traffic_limit' => 0,
                'expired_at' => null,
            ])->save();
        }

        // 重置 3x-ui 流量统计
        $this->resetClientOnNodes($user);
    }

    /**
     * 创建用户（token/uuid 自动生成）+ 在各 enabled 节点上 provision 3x-ui client。
     * 支持套餐绑定：传 plan_id 时自动填充 traffic_limit/expired_at。
     */
    public function create(array $data): User
    {
        // 套餐联动：自动填充流量和到期时间
        $trafficLimit = (int) ($data['traffic_limit'] ?? 0);
        $monthlyTrafficLimit = (int) ($data['monthly_traffic_limit'] ?? 0);
        $expiredAt = $data['expired_at'] ?? null;

        $nextTrafficResetAt = null;

        if (!empty($data['plan_id'])) {
            $plan = Plan::find($data['plan_id']);
            if ($plan) {
                if ($plan->isPeriod()) {
                    $trafficLimit = $plan->period_traffic ?? 0;
                    $monthlyTrafficLimit = $plan->monthly_traffic ?? 0;
                    $expiredAt = Carbon::now()->addMonths($plan->months)->toDateString();
                    $nextTrafficResetAt = Carbon::now()->addDays(30);
                } else {
                    $trafficLimit = $plan->total_traffic ?? 0;
                    $monthlyTrafficLimit = 0;
                    $expiredAt = null;
                }
            }
        }

        $user = User::create([
            'email' => $data['email'] ?? null,
            'token' => $data['token'] ?? ('sub_' . Str::random(32)),
            'uuid' => $data['uuid'] ?? Str::uuid()->toString(),
            'protocol' => $data['protocol'] ?? 'vless',
            'plan_id' => $data['plan_id'] ?? null,
            'traffic_limit' => $trafficLimit,
            'traffic_used' => 0,
            'monthly_traffic_used' => 0,
            'monthly_traffic_limit' => $monthlyTrafficLimit,
            'next_traffic_reset_at' => $nextTrafficResetAt,
            'expired_at' => $expiredAt,
            'enabled' => (bool) ($data['enabled'] ?? true),
        ]);

        $this->provisionClient($user);

        return $user;
    }

    /**
     * 在各 enabled 节点上同步 3x-ui client（ch_user_{id}）。
     * client 存在则更新配置，不存在则创建。
     *
     * 本方法只负责「遍历 enabled 节点」，单节点动作在 provisionClientOnNode 里，
     * 与 ProvisionClientJob（异步路径）共用同一段实现，避免两套逻辑漂移。
     */
    public function provisionClient(User $user): void
    {
        $clientData = $this->prepareClientData($user);

        foreach (Node::where('enabled', true)->get() as $node) {
            $this->provisionClientOnNode($user, $node, $clientData);
        }
    }

    /**
     * 注册路径用的异步版：把「用户 × enabled 节点」拆成 N 个 ProvisionClientJob 派发后立即返回。
     *
     * 同样只负责遍历，单节点逻辑与 provisionClient 共用 provisionClientOnNode。
     * 这里【不】预先生成 clientData 塞进 Job：Job 执行时按当时的用户数据重算，
     * 否则「注册后立刻购买套餐」的窗口里，Job 会拿注册瞬间的 enable=false
     * 覆盖支付流程刚写进面板的 enable=true。
     *
     * 派发本身若抛异常由调用方决定如何处理（注册接口只记日志，不影响注册成功）。
     *
     * @return int 派发的 Job 数（= enabled 节点数，含没有该协议入站的节点；
     *             这类节点由 Job 内部按入站为空跳过，与同步版遍历时一致）
     */
    public function dispatchProvisionClient(User $user): int
    {
        $nodes = Node::where('enabled', true)->get();

        foreach ($nodes as $node) {
            ProvisionClientJob::dispatch($user->id, $node->id)->afterCommit();
        }

        return $nodes->count();
    }

    /**
     * 构造写入面板的 client 负载。
     *
     * 副作用（有意为之，位置与拆分前一致，在逐节点写入之前）：
     * 负载以 enable=true 写回面板等于抹掉了「已关闭」的既有状态，必须清空
     * traffic_disabled_at，否则扫描器会以为用户仍关闭而跳过校验（最长 6 小时）。
     */
    public function prepareClientData(User $user): array
    {
        $clientData = [
            'email' => $user->clientEmail(),
            'enable' => $user->plan_id ? (bool) $user->enabled : false,  // 无套餐禁用
            'totalGB' => $user->traffic_limit > 0 ? (int) $user->traffic_limit : 0,
            'expiryTime' => $user->expired_at ? (int) ($user->expired_at->timestamp * 1000) : 0,
            'limitIp' => 0,
        ];

        if ($clientData['enable']) {
            app(BanService::class)->forgetDisabledState($user);
        }

        return $clientData;
    }

    /**
     * 在【单个节点】上 provision 该用户 —— 同步遍历与 ProvisionClientJob 的唯一共用入口。
     *
     * $clientData 按引用传入：拆分前 provisionClient 的循环体直接改外层 $clientData
     * （命中已存在 client 时写 'id'），该键会带到下一个节点的 create 负载里。
     * 保留引用是为了让拆分后的同步路径与拆分前逐行等价，不要改成传值。
     *
     * 单节点语义（与拆分前循环体一致）：
     * - 该节点无此协议入站 → 直接跳过，不发任何请求
     * - client 已存在 → 逐入站 updateClient，id 优先回写 uuid（3x-ui v3.7.0 行号坑）
     * - client 不存在 → createClient，并用返回的 uuid 补 user.uuid（仅当用户还没有 uuid）
     * - 单个入站失败只 report()，不影响同节点其它入站
     * - 单节点失败只 report()，由调用方决定是否影响其它节点（同步版继续下一个节点，
     *   Job 版则只有这一个节点的 Job 失败）
     */
    public function provisionClientOnNode(User $user, Node $node, array &$clientData): void
    {
        $email = $clientData['email'];

        if ($node->isXray()) {
            $this->provisionXrayClientOnNode($user, $node, $clientData);

            return;
        }

        $inboundIds = $node->inboundIdsFor($user->protocol);
        if (empty($inboundIds)) {
            return;
        }

        $client = $this->driverFactory->make($node);
        try {
            // 先检查 client 是否存在
            $existing = $client->getClient($email);
            if ($existing) {
                // 已存在：更新配置
                // 3x-ui v3.7.0 返回的 id 是 clients 表自增行号，uuid 才是 Xray 用的
                // client id。若把行号写回 id，3x-ui 会把该 client 的 uuid 覆盖成数字，
                // 导致用户断连。优先取 uuid，旧版 3x-ui（id 即 uuid、无 uuid 键）回退 id。
                $clientData['id'] = $existing['uuid'] ?? ($existing['id'] ?? null);
                foreach ($inboundIds as $inboundId) {
                    try {
                        $client->updateClient($email, $clientData, $inboundId);
                    } catch (\Throwable $e) {
                        report($e);
                    }
                }
            } else {
                // 不存在：创建新 client
                $created = $client->createClient($clientData, $inboundIds);
                if (!empty($created['uuid']) && !$user->uuid) {
                    $user->forceFill(['uuid' => $created['uuid']])->save();
                }
            }
        } catch (\Throwable $e) {
            report($e);
        }
    }

    /**
     * xray 节点上的单用户同步：入队 adu/rmu 指令（agent 秒级执行，不等回执）。
     *
     * 与 3x-ui 路径同口径：只处理该节点应承载的协议（P1 仅 vless）；
     * enable=false（无套餐/已关闭）语义 = 内核移除（rmu），启用 = adu 重加。
     */
    private function provisionXrayClientOnNode(User $user, Node $node, array &$clientData): void
    {
        if ($user->protocol !== 'vless') {
            return;
        }

        $clientData['uuid'] = (string) $user->uuid;

        try {
            $driver = $this->driverFactory->make($node);
            if ($driver->getClient($clientData['email']) !== null) {
                $driver->updateClient($clientData['email'], $clientData);
            } else {
                $driver->createClient($clientData, []);
            }
        } catch (\Throwable $e) {
            report($e);
        }
    }

    /**
     * 新节点创建后，把所有已启用用户同步到该节点上。
     * 单用户失败不阻断。
     */
    public function provisionAllUsersToNode(Node $node): void
    {
        $users = User::whereNotNull('plan_id')->get();

        foreach ($users as $user) {
            $inboundIds = $node->inboundIdsFor($user->protocol);
            if (empty($inboundIds)) {
                continue;
            }

            try {
                $this->driverFactory->make($node)->createClient([
                    'email' => $user->clientEmail(),
                    'enable' => (bool) $user->enabled,
                    'totalGB' => $user->traffic_limit > 0 ? (int) $user->traffic_limit : 0,
                    'expiryTime' => $user->expired_at ? (int) ($user->expired_at->timestamp * 1000) : 0,
                    'limitIp' => 0,
                ], $inboundIds);

                // 同 provisionClient：以 enable=true 建 client 后撤销「已关闭」标记
                if ($user->enabled) {
                    app(BanService::class)->forgetDisabledState($user);
                }
            } catch (\Throwable $e) {
                report($e);
            }
        }
    }

    /**
     * 总量套餐续费：重置总流量 + 重新启用 + 3x-ui 重置。
     */
    public function renew(User $user): void
    {
        // 续费 → 恢复该用户所有自动通知（旧防重标记全部作废）
        MailNotifyService::restoreReminders($user);

        $user->forceFill([
            'traffic_used' => 0,
            'monthly_traffic_used' => 0,
            'enabled' => true,
        ])->save();

        $this->resetClientOnNodes($user);
    }

    /**
     * 重置用户当月流量：清零 monthly_traffic_used + 总流量加月流量额度 + 同步 3x-ui + 打开连接。
     * 注意：管理员手动重置不更新 next_traffic_reset_at，只清零已用流量。
     */
    public function resetTraffic(User $user): void
    {
        // 重置流量 → 恢复该用户所有自动通知（旧防重标记全部作废）
        MailNotifyService::restoreReminders($user);

        if ($user->isPeriodPlan()) {
            $addTraffic = $user->monthly_traffic_limit ?? 0;
            $user->forceFill([
                'monthly_traffic_used' => 0,
                'traffic_limit' => ($user->traffic_limit ?? 0) + $addTraffic,
            ])->save();
        } else {
            $user->forceFill(['traffic_used' => 0])->save();
        }

        // 同步新的流量限制到 3x-ui + 重置流量计数 + 打开连接
        $user->refresh();
        $this->provisionClient($user);
        $this->resetClientOnNodes($user);
        app(BanService::class)->toggleClient($user, true);
    }

    public function resetClientOnNodes(User $user): void
    {
        $email = $user->clientEmail();
        foreach (Node::where('enabled', true)->get() as $node) {
            try {
                $this->driverFactory->make($node)->resetClientTraffic($email);
            } catch (\Throwable $e) {
                report($e);
            }
        }
    }
}
