<?php

namespace App\Console\Commands;

use App\Services\PhpFpmTuner;
use Illuminate\Console\Command;

/**
 * php-fpm 并发（pm.max_children）调优 —— 给「已装机器」用，不重装、不手改配置。
 *
 * 发行版默认是 5，面板一有并发就排队。新装机器由 install.sh 安装时已设好；本命令负责
 * 已装机器 + 以后后台「档位」按钮的执行入口（后台只允许执行这些固定档位，不做自由输入）。
 *
 * 安全：改前备份、`php-fpm -t` 校验、reload 失败自动回滚（配置写错会让面板自己都打不开）。
 */
class TunePhpFpmCommand extends Command
{
    protected $signature = 'fpm:tune
        {--preset=auto : auto|small|medium|large（auto=按内存与核数自动算）}
        {--dry-run : 只看推荐值，不改配置}';

    protected $description = '调整 php-fpm 并发上限（pm.max_children），带备份与失败回滚';

    public function handle(PhpFpmTuner $tuner): int
    {
        if (file_exists('/.dockerenv')) {
            $this->error('当前是容器环境：容器内的 php-fpm 池由镜像里的 docker/www.conf 决定（已是 50），');
            $this->error('请在部署侧改 docker/www.conf 后重建容器，不要在这里改。');

            return self::FAILURE;
        }

        if (! file_exists('/proc/meminfo')) {
            $this->error('未检测到 Linux 环境（/proc/meminfo 不存在），本命令只在面板服务器上执行。');

            return self::FAILURE;
        }

        $target = $tuner->detectTarget();
        if ($target === null) {
            $this->error('没找到 php-fpm 池配置，已探测：' . implode('、', array_column((array) config('panel.fpm.targets', []), 'conf')));

            return self::FAILURE;
        }

        $children = $this->resolveChildren($tuner);
        if ($children === null) {
            return self::FAILURE;
        }

        $current = $tuner->currentChildren($target['conf']);
        $this->line("配置文件：{$target['conf']}");
        $this->line("服务名　：{$target['service']}");
        $this->line('当前并发：' . ($current === null ? '未设置（发行版默认 5）' : (string) $current));
        $this->line("建议并发：{$children}");

        if ($this->option('dry-run')) {
            $this->info('--dry-run：未改动任何配置。');

            return self::SUCCESS;
        }

        if ($current === $children) {
            $this->info('当前值已与建议值一致，无需改动。');

            return self::SUCCESS;
        }

        $result = $tuner->apply($target['conf'], $target['service'], $children);
        if (! $result['ok']) {
            $this->error($result['reason']);

            return self::FAILURE;
        }

        $tuner->pruneBackups($target['conf']);

        $this->info("php-fpm 并发已设为 {$result['children']}（原配置备份：{$result['backup']}）");
        $this->line('如需回滚：cp -a ' . $result['backup'] . ' ' . $target['conf'] . ' && systemctl reload ' . $target['service']);

        return self::SUCCESS;
    }

    /** 解析目标并发：auto 走自动计算，其余走 config 里的预制档位。 */
    private function resolveChildren(PhpFpmTuner $tuner): ?int
    {
        $preset = (string) $this->option('preset');
        $presets = (array) config('panel.fpm.presets', []);

        if ($preset === 'auto') {
            $specs = $tuner->specs();
            $recommended = $tuner->recommend($specs['mem_mb'], $specs['cpus']);
            $this->line("机器规格：内存 {$specs['mem_mb']}MB / {$specs['cpus']} 核");

            return $recommended;
        }

        if (! array_key_exists($preset, $presets)) {
            $this->error('未知档位：' . $preset . '（可选：auto、' . implode('、', array_keys($presets)) . '）');

            return null;
        }

        return (int) $presets[$preset];
    }
}
