<?php

namespace App\Services;

use App\Models\Node;
use App\Models\NodeApiCommand;

/**
 * agent 自升级（面板侧入队 upgrade 指令）。
 *
 * 链路：面板入队 → agent 经现有指令通道取走（WS 主通道 / HTTP 长轮询）→ 下载新二进制 →
 * sha256 校验 → 原子替换自己 → 回执 → 退出由 systemd 拉起新版（见 agent/upgrade.go）。
 *
 * 为什么走指令通道、而不是让 agent 自己定时拉 manifest 比对：
 *   升级时机由面板控制（可灰度、可单点重试、离线节点排队等上线），结果直接落在现成的
 *   指令回执/日志里；agent 侧不用多一个版本检查循环。
 *
 * 触发方式：3hub update（全量）/ `php artisan node:upgrade-agent`（手动）/ 面板节点页按钮。
 */
class AgentUpgradeService
{
    private const ASSET_ROOT = 'xray';

    private const DEFAULT_ARCH = 'linux-64';

    /** manifest 的 agent 段（缺失/损坏返回 null）。 */
    public function manifest(): ?array
    {
        $path = $this->manifestPath();
        if (! is_file($path)) {
            return null;
        }

        $manifest = json_decode((string) file_get_contents($path), true);
        $agent = is_array($manifest) ? ($manifest['agent'] ?? null) : null;

        return is_array($agent) ? $agent : null;
    }

    /**
     * 该节点的升级目标；null = 这台节点不该/不能升级。
     *
     * @return array{arch:string,file:string,sha256:string,version:string,url:string}|null
     */
    public function targetFor(Node $node): ?array
    {
        $agent = $this->manifest();
        if ($agent === null || empty($agent['assets'])) {
            return null;
        }

        $arch = $this->archOf($node);
        $asset = collect($agent['assets'])->firstWhere('arch', $arch);
        if (! is_array($asset) || empty($asset['file']) || empty($asset['sha256'])) {
            return null;
        }

        return [
            'arch' => $arch,
            'file' => (string) $asset['file'],
            'sha256' => (string) $asset['sha256'],
            'version' => (string) ($agent['version'] ?? ''),
            // 相对路径：agent 用本机 state.json 里的 hub 地址拼 —— 节点可能用 IP 连面板，
            // 用面板的 APP_URL（域名）拼会让节点解析不到
            'url' => '/node-bin/agent/' . $arch,
        ];
    }

    /** 面板上这份 agent 的版本号（manifest 缺失返回 null）。 */
    public function latestVersion(): ?string
    {
        $agent = $this->manifest();
        $version = is_array($agent) ? ($agent['version'] ?? null) : null;

        return is_string($version) && $version !== '' ? $version : null;
    }

    /** 节点当前上报的 agent 版本（未上报返回 null）。 */
    public function reportedVersion(Node $node): ?string
    {
        $version = $node->driver_config['agent_version'] ?? null;

        return is_string($version) && $version !== '' ? $version : null;
    }

    /**
     * 给节点入队一条升级指令（幂等：已是最新 / 已有在途指令都不重复入队）。
     *
     * @return array{queued:bool,reason:string,version:?string}
     */
    public function enqueue(Node $node, bool $force = false): array
    {
        if (! $node->isXray()) {
            return ['queued' => false, 'reason' => '非 xray 节点（没有 agent）', 'version' => null];
        }

        $target = $this->targetFor($node);
        if ($target === null) {
            return ['queued' => false, 'reason' => '面板 manifest 里没有该架构的 agent 资产', 'version' => null];
        }

        $reported = $this->reportedVersion($node);
        if (! $force && $reported !== null && $reported === $target['version']) {
            return ['queued' => false, 'reason' => '节点已上报 v' . $reported, 'version' => $target['version']];
        }

        if ($this->hasInFlight($node)) {
            return ['queued' => false, 'reason' => '已有待执行的升级指令', 'version' => $target['version']];
        }

        NodeApiCommand::create([
            'node_id' => $node->id,
            'action' => NodeApiCommand::ACTION_UPGRADE_AGENT,
            'payload' => [
                'file' => $target['file'],
                'sha256' => $target['sha256'],
                'version' => $target['version'],
                'url' => $target['url'],
            ],
            'status' => NodeApiCommand::STATUS_PENDING,
        ]);

        return ['queued' => true, 'reason' => '', 'version' => $target['version']];
    }

    /** 该节点是否已有未终态的升级指令。 */
    public function hasInFlight(Node $node): bool
    {
        return NodeApiCommand::where('node_id', $node->id)
            ->where('action', NodeApiCommand::ACTION_UPGRADE_AGENT)
            ->whereIn('status', [NodeApiCommand::STATUS_PENDING, NodeApiCommand::STATUS_ACKNOWLEDGED])
            ->exists();
    }

    private function archOf(Node $node): string
    {
        $arch = $node->driver_config['kernel_arch'] ?? null;

        return is_string($arch) && $arch !== '' ? $arch : self::DEFAULT_ARCH;
    }

    /** manifest 路径（测试可注入；生产走 storage）。 */
    private function manifestPath(): string
    {
        $configured = (string) config('panel.agent_manifest_path', '');

        return $configured !== '' ? $configured : storage_path('app/' . self::ASSET_ROOT . '/manifest.json');
    }
}
