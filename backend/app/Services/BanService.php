<?php

namespace App\Services;

use App\Drivers\NodeDriverFactory;
use App\Jobs\SendMailJob;
use App\Models\MailLog;
use App\Models\Node;
use App\Models\SiteConfig;
use App\Models\User;

/**
 * 封禁服务（M8 + 套餐适配）。
 *
 * 到期 / 流量超限：不禁用用户，只关闭 3x-ui 流量。
 * 「无套餐」不在这里处理——它既不触发关流量，也不发通知（见 MailNotifyScanJob）。
 * 仅管理员手动封禁才设置 enabled=false。
 */
class BanService
{
    public function __construct(private NodeDriverFactory $driverFactory)
    {
    }

    /**
     * 判断用户是否应被关闭流量。
     * 返回 'disable'（需要关闭 3x-ui 流量）、false（正常）。
     * 永不返回 'ban'，只有管理员才能封禁用户。
     */
    public function banReason(User $user): string|false
    {
        // 到期检查 → 关闭流量
        if ($user->expired_at !== null && $user->expired_at->isPast()) {
            return 'disable';
        }

        // 周期套餐：当月流量超限 → 关闭流量
        if ($user->plan_id && $user->monthly_traffic_limit > 0 && $user->monthly_traffic_used >= $user->monthly_traffic_limit) {
            return 'disable';
        }

        // 周期/总量套餐：总流量超限 → 关闭流量
        if ($user->plan_id && $user->traffic_limit > 0 && $user->traffic_used >= $user->traffic_limit) {
            return 'disable';
        }

        return false;
    }

    /** @deprecated 使用 banReason() 替代 */
    public function isBannable(User $user): bool
    {
        return $this->banReason($user) !== false;
    }

    /**
     * 封禁用户（仅管理员手动操作）。
     * 设置 enabled=false + 关闭 3x-ui 流量。
     */
    public function ban(User $user): void
    {
        $user->forceFill(['enabled' => false])->save();
        $this->toggleClient($user, false);
    }

    public function unban(User $user): void
    {
        $user->forceFill(['enabled' => true])->save();
        $this->toggleClient($user, true);
    }

    /**
     * 流量同步后内联检查（M8.4）：满足条件则关闭 3x-ui 流量。
     * 不禁用用户，用户仍能登录续费。
     */
    public function checkAfterSync(User $user): void
    {
        if (!$user) return;

        // 与 BanCheckJob / SyncTrafficCommand 保持同一规则：
        // 已确认关闭且未超时效 → 早退，0 次 HTTP 请求。
        if ($this->isRecentlyDisabled($user)) {
            return;
        }

        $reason = $this->banReason($user);
        if ($reason !== false) {
            $this->toggleClient($user, false);
        }
    }

    /**
     * 扫描器（BanCheckJob / SyncTrafficCommand）是否还应对该用户执行关闭动作。
     *
     * 跳过依据：users.traffic_disabled_at 记录「上一次确认把 client 关掉」的时间。
     * 已关闭且未超时效（config('ban.recheck_after_hours')）→ 不再重复发请求，
     * 这正是「同一批超量用户每 5 分钟被重关一遍」的止血点。
     * 超时效后仍会重新校验一次，用于兜住「面板上被人工开回来」的状态漂移。
     */
    public function needsDisable(User $user): bool
    {
        return $this->banReason($user) !== false && !$this->isRecentlyDisabled($user);
    }

    /**
     * 该用户是否处于「已确认关闭且仍在时效内」状态（此时扫描器零 HTTP 请求跳过）。
     * 时效配 0 表示关闭该优化（每轮都重新校验）。
     */
    public function isRecentlyDisabled(User $user): bool
    {
        $disabledAt = $user->traffic_disabled_at;
        if ($disabledAt === null) {
            return false;
        }

        $hours = (int) config('ban.recheck_after_hours', 6);
        if ($hours <= 0) {
            return false;
        }

        return $disabledAt->gt(now()->subHours($hours));
    }

    /**
     * 外部路径（provision / 新建节点同步等）把 client 以 enable=true 写回 3x-ui 时调用：
     * 面板状态已不再是「已关闭」，必须撤销 traffic_disabled_at，
     * 否则扫描器会以为用户仍处于关闭态而跳过校验（最长 config('ban.recheck_after_hours')）。
     */
    public function forgetDisabledState(User $user): void
    {
        if ($user->traffic_disabled_at !== null) {
            $user->forceFill(['traffic_disabled_at' => null])->save();
        }
    }

    /**
     * 全量替换 3x-ui client 前，规范化 API 不兼容的字段类型。
     * 3x-ui 的 /clients/update 是全量替换语义，回传整个 client 对象时，
     * 以下字段若类型不符会触发 Go 反序列化失败——而 toggleClient 里被
     * catch(\Throwable) 静默吞掉，导致 enable 永远改不动（"没关"的根因）：
     * - id：3x-ui 要求 string
     * - allowedIPs / tunnelAllowedIPs：Go 用 []string 反序列化，空字符串会报
     *   "cannot unmarshal string into Go struct field .allowedIPs of type []string"
     */
    private function normalizeForUpdate(array $clientData): array
    {
        // 3x-ui v3.7.0 返回的 id 是数据库自增行号，uuid 才是 Xray 用的 client id；
        // 全量替换时把行号写回 id 会把 client 的 uuid 覆盖成数字，导致用户断连。
        // 优先用 uuid，旧版 3x-ui（id 即 uuid、无 uuid 键）回退仅做 string 强转。
        if (!empty($clientData['uuid'])) {
            $clientData['id'] = $clientData['uuid'];
        } elseif (isset($clientData['id'])) {
            $clientData['id'] = (string) $clientData['id'];
        }
        foreach (['allowedIPs', 'tunnelAllowedIPs'] as $field) {
            if (isset($clientData[$field]) && $clientData[$field] === '') {
                $clientData[$field] = [];
            }
        }
        return $clientData;
    }

    /**
     * 开关 3x-ui client（所有开关流量路径的唯一汇聚点）。
     *
     * 请求量（每用户每轮，生产 5 节点 / 9 入站）：
     * - getClient 提到入站循环外，每节点只发 1 次：同一 email 在同一节点上 client 数据
     *   相同，入站维度的差异只体现在 updateClient 的 inboundId 参数上；
     *   入站循环与「兜底不带 inboundId」的更新复用同一份响应。
     * - 改前 28 次（9 入站 ×（1 get + 1 update）+ 5 节点 ×（1 get + 1 update））
     *   → 改后 19 次（5 get + 9 入站 update + 5 兜底 update）。
     *
     * 返回值：本次调用是否已确认生效（为 true 时 users.traffic_disabled_at 已同步落库）。
     * 判定口径：
     * - 关闭（enable=false）：要求【全部启用节点】确认，才写 traffic_disabled_at。
     *   少关掉一个节点，用户就能从那个节点继续跑流量；宁可下一轮重试，
     *   也不能标记成「已关闭」而被扫描器跳过 —— 保住「兜底不漏关」的既有语义。
     * - 开启（enable=true）：只要有【任一节点】确认就清空标记。清空是更宽松的方向，
     *   最坏结果只是下一轮多做一次校验，不会漏关。
     */
    public function toggleClient(User $user, bool $enable): bool
    {
        $email = $user->clientEmail();
        $nodes = Node::where('enabled', true)->get();

        if ($nodes->isEmpty()) {
            return false; // 无节点可操作：不写标记，下一轮继续尝试
        }

        $confirmed = 0;
        foreach ($nodes as $node) {
            if ($this->applyEnableOnNode($node, $user->protocol, $email, $enable)) {
                $confirmed++;
            }
        }

        if ($enable) {
            if ($confirmed > 0) {
                $this->forgetDisabledState($user);
            }

            return $confirmed > 0;
        }

        $applied = $confirmed === $nodes->count();
        if ($applied) {
            $user->forceFill(['traffic_disabled_at' => now()])->save();

            // 流量用尽通知。注意：本分支每 ban.recheck_after_hours 小时会被重关一次
            // （isRecentlyDisabled 时效到期后重新校验），所以这里并不是天然的单次
            // 触发点——防重靠 notifyTrafficExhausted 里的月度标记。
            $this->notifyTrafficExhausted($user);
        }

        return $applied;
    }

    /**
     * 流量用尽通知（默认关闭，管理员在「设置-邮箱」里自行开启）。
     * 开关未开时这里只读一次 SiteConfig，无队列、无 SMTP 请求。
     *
     * 同一自然月同一收件人只发一封（口径见 MailNotifyService::claimMonthly）：
     * 本方法挂在「关闭成功」这个动作上，而扫描器每 ban.recheck_after_hours
     * 小时会把同一个超量用户重关一遍，没有这道标记就会一直重发。
     */
    private function notifyTrafficExhausted(User $user): void
    {
        if (!MailNotifyService::isEnabled('traffic_exhausted')) {
            return;
        }

        $rendered = MailNotifyService::render('traffic_exhausted', $user->loadMissing('plan'));
        if ($rendered === []) {
            return;
        }

        $to = $rendered['to_type'] === 'admin'
            ? SiteConfig::getValue('notify_admin_email')
            : $user->email;

        if (empty($to)) {
            return;
        }

        // 本月已给这个收件人发过 → 跳过（重关、状态漂移都不会再发一封）
        if (!MailNotifyService::claimMonthly('traffic_exhausted', $user, $rendered['to_type'], $to)) {
            return;
        }

        SendMailJob::dispatch(
            toEmail: $to,
            subject: $rendered['subject'],
            htmlBody: $rendered['body'],
            type: MailLog::TYPE_NOTIFY,
            userId: $user->id,
            scene: 'traffic_exhausted',
        );
    }

    /**
     * 在单个节点上把 client 的 enable 改成 $enable。
     *
     * @return bool 该节点是否【确认】已生效：getClient 取到了 client，且所有
     *              updateClient 调用都没抛异常。
     *              getClient 返回 null 既可能是 client 不存在，也可能是节点不可达 /
     *              鉴权失败（ThreeXUiClient::requestNullable 对两类失败一律返回 null），
     *              无法区分，故一律按「未确认」处理，交给下一轮重试。
     *              例外：该节点本来就没有该协议的入站（provisionClient 同样会跳过这类节点，
     *              用户在本节点无挂载点可关）→ 视为「无需操作」而非失败，否则这类用户
     *              永远确认不了、每轮都要被重发一遍请求，优化对他们等于失效。
     */
    private function applyEnableOnNode(Node $node, string $protocol, string $email, bool $enable): bool
    {
        try {
            $driver = $this->driverFactory->make($node);
            $inboundIds = $node->inboundIdsFor($protocol);

            // 每节点只取一次 client 数据，入站循环与兜底共用
            $resp = $driver->getClient($email);
            if ($resp === null) {
                return $inboundIds === [];
            }

            $clientData = $this->normalizeForUpdate($resp['client'] ?? $resp);
            $clientData['enable'] = $enable;

            $ok = true;

            // 逐个入站更新 enable 状态
            foreach ($inboundIds as $inboundId) {
                try {
                    $driver->updateClient($email, $clientData, $inboundId);
                } catch (\Throwable) {
                    // 入站不存在或 client 不存在，忽略；但该节点不再算「确认生效」
                    $ok = false;
                }
            }

            // 兜底不带 inboundId 再更新一次
            try {
                $driver->updateClient($email, $clientData);
            } catch (\Throwable) {
                $ok = false;
            }

            return $ok;
        } catch (\Throwable $e) {
            report($e);

            return false;
        }
    }
}