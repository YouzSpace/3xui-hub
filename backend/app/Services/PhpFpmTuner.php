<?php

namespace App\Services;

/**
 * php-fpm 并发（pm.max_children）调优。
 *
 * 背景：发行版默认 www.conf 是 pm.max_children=5（Debian/Ubuntu 的
 * /etc/php/8.4/fpm/pool.d/www.conf 就是 5），面板一有并发（拉订阅 + 后台操作 + 队列回调）
 * 请求就排队，这是用户量上来后第一个卡的环节。Docker 版显式写了 50，裸机没人设。
 *
 * 本类给「已装机器」用（`php artisan fpm:tune`），新装机器由 install.sh 的 tune_php_fpm
 * 在安装时设好 —— 两处公式必须一致（见 config/panel.php 的 panel.fpm 注释）。
 *
 * 安全底线（改错会让面板自己都打不开）：
 *   1. 改前备份到 <conf>.bak.<时间戳>
 *   2. 先 `php-fpm -t -y <conf>` 做语法校验，不通过就回滚
 *   3. reload/restart 失败也回滚，保持原配置可用
 */
class PhpFpmTuner
{
    /** 可注入的命令执行器 fn(string $cmd): array{0:int,1:string} —— 测试用，生产走 shell。 */
    private $runner;

    public function __construct(?callable $runner = null)
    {
        $this->runner = $runner;
    }

    /**
     * 读机器规格。读不到按 0 返回，由 recommend() 兜到下限。
     *
     * @return array{mem_mb:int,cpus:int}
     */
    public function specs(): array
    {
        $memMb = 0;
        if (is_readable('/proc/meminfo')) {
            $content = (string) @file_get_contents('/proc/meminfo');
            if (preg_match('/^MemTotal:\s+(\d+)\s+kB/m', $content, $m)) {
                $memMb = (int) floor(((int) $m[1]) / 1024);
            }
        }

        $cpus = 0;
        if (is_readable('/proc/cpuinfo')) {
            $cpus = substr_count((string) @file_get_contents('/proc/cpuinfo'), "\nprocessor");
        }
        if ($cpus <= 0) {
            $cpus = (int) trim((string) @shell_exec('nproc 2>/dev/null'));
        }

        return ['mem_mb' => max(0, $memMb), 'cpus' => max(0, $cpus)];
    }

    /**
     * 推荐 max_children：内存×ratio÷单进程MB 与 核数×倍率 取小，夹到 [min,max]。
     *
     * 两个来源都可能读不到（容器/异常环境）：0 表示「未知」，未知的一方不参与取小。
     */
    public function recommend(int $memMb, int $cpus): int
    {
        $cfg = (array) config('panel.fpm');

        $byMem = $memMb > 0
            ? (int) floor($memMb * (float) $cfg['memory_ratio'] / max(1, (int) $cfg['process_memory_mb']))
            : 0;
        $byCpu = $cpus > 0 ? $cpus * (int) $cfg['cpu_multiplier'] : 0;

        $value = $byMem;
        if ($value === 0 || ($byCpu > 0 && $byCpu < $value)) {
            $value = $byCpu;
        }

        return max((int) $cfg['min_children'], min((int) $cfg['max_children'], $value));
    }

    /** pm = dynamic 下各档位（必须一起改：max_spare_servers > max_children 会让 php-fpm 起不来）。 */
    public function poolPlan(int $children): array
    {
        return [
            'pm' => 'dynamic',
            'pm.max_children' => $children,
            'pm.start_servers' => max(2, intdiv($children, 4)),
            'pm.min_spare_servers' => max(1, intdiv($children, 8)),
            'pm.max_spare_servers' => $children,
        ];
    }

    /**
     * 定位本机 php-fpm 池配置与服务名。
     *
     * @return array{conf:string,service:string}|null 都探测不到时返回 null
     */
    public function detectTarget(): ?array
    {
        foreach ((array) config('panel.fpm.targets', []) as $target) {
            $conf = (string) ($target['conf'] ?? '');
            if ($conf !== '' && is_file($conf)) {
                return ['conf' => $conf, 'service' => (string) ($target['service'] ?? 'php-fpm')];
            }
        }

        return null;
    }

    /** 当前配置里的 pm.max_children（读不到返回 null）。 */
    public function currentChildren(string $conf): ?int
    {
        $content = (string) @file_get_contents($conf);
        if (preg_match('/^\s*pm\.max_children\s*=\s*(\d+)/m', $content, $m)) {
            return (int) $m[1];
        }

        return null;
    }

    /**
     * 应用新值：备份 → 改写 → 语法校验 → reload。任一步失败即回滚。
     *
     * @return array{ok:bool,children:int,backup:?string,reason:string}
     */
    public function apply(string $conf, string $service, int $children): array
    {
        if (! is_file($conf)) {
            return ['ok' => false, 'children' => 0, 'backup' => null, 'reason' => "找不到 php-fpm 配置：{$conf}"];
        }

        $original = (string) @file_get_contents($conf);
        if ($original === '') {
            return ['ok' => false, 'children' => 0, 'backup' => null, 'reason' => "读取 {$conf} 失败"];
        }

        $backup = $conf . '.bak.' . date('YmdHis');
        if (! @copy($conf, $backup)) {
            return ['ok' => false, 'children' => 0, 'backup' => null, 'reason' => "备份到 {$backup} 失败，未改动配置"];
        }

        $updated = $this->rewrite($original, $this->poolPlan($children));
        if (@file_put_contents($conf, $updated) === false) {
            @unlink($backup);

            return ['ok' => false, 'children' => 0, 'backup' => null, 'reason' => "写入 {$conf} 失败，未改动配置"];
        }

        // 语法校验：php-fpm 二进制找不到时跳过（下面 reload 失败仍会回滚）
        $binary = $this->detectBinary();
        if ($binary !== null) {
            [$code, $out] = $this->run($binary . ' -t -y ' . escapeshellarg($conf));
            if ($code !== 0) {
                $this->restore($conf, $original, $backup);

                return ['ok' => false, 'children' => 0, 'backup' => null, 'reason' => 'php-fpm 配置校验失败，已回滚：' . $this->firstLine($out)];
            }
        }

        [$reloadCode, $reloadOut] = $this->run(
            'systemctl reload ' . escapeshellarg($service) . ' 2>/dev/null || systemctl restart ' . escapeshellarg($service)
        );
        if ($reloadCode !== 0) {
            $this->restore($conf, $original, $backup);

            return ['ok' => false, 'children' => 0, 'backup' => null, 'reason' => "重载 {$service} 失败，已回滚：{$this->firstLine($reloadOut)}"];
        }

        return ['ok' => true, 'children' => $children, 'backup' => $backup, 'reason' => ''];
    }

    /** 清掉超过 $keep 份的历史备份（反复执行时不无限堆积）。返回删除份数。 */
    public function pruneBackups(string $conf, int $keep = 3): int
    {
        $backups = glob($conf . '.bak.*') ?: [];
        if (count($backups) <= $keep) {
            return 0;
        }

        sort($backups); // 时间戳后缀，字符串排序即时间序
        $removed = 0;
        foreach (array_slice($backups, 0, count($backups) - $keep) as $file) {
            if (@unlink($file)) {
                $removed++;
            }
        }

        return $removed;
    }

    // ── 内部 ────────────────────────────────────────────────

    /**
     * 改写配置文本：已存在（可能被注释）的指令改写，完全缺失的插入 [www] 段首。
     */
    private function rewrite(string $content, array $plan): string
    {
        foreach ($plan as $key => $value) {
            $quoted = preg_quote($key, '/');
            if (preg_match('/^\s*;?\s*' . $quoted . '\s*=/m', $content)) {
                $content = (string) preg_replace(
                    '/^\s*;?\s*' . $quoted . '\s*=.*$/m',
                    $key . ' = ' . (string) $value,
                    $content
                );

                continue;
            }

            // 配置里完全没有该指令（部分发行版的 www.conf 不带这些行）→ 追加到 [www] 段首
            if (preg_match('/^\[www\]\s*$/m', $content)) {
                $content = (string) preg_replace(
                    '/^\[www\]\s*$/m',
                    "[www]\n" . $key . ' = ' . (string) $value,
                    $content,
                    1
                );

                continue;
            }

            $content .= "\n" . $key . ' = ' . (string) $value . "\n";
        }

        return $content;
    }

    private function restore(string $conf, string $original, string $backup): void
    {
        @file_put_contents($conf, $original);
        @unlink($backup);
    }

    private function detectBinary(): ?string
    {
        [$code, $out] = $this->run('command -v php-fpm');
        if ($code === 0 && trim($out) !== '') {
            return trim($this->firstLine($out));
        }

        foreach (['/usr/sbin/php-fpm', '/opt/remi/php84/root/usr/sbin/php-fpm'] as $path) {
            if (is_file($path)) {
                return $path;
            }
        }

        return null;
    }

    /** @return array{0:int,1:string} */
    private function run(string $cmd): array
    {
        if ($this->runner !== null) {
            $result = ($this->runner)($cmd);

            return [(int) ($result[0] ?? 1), (string) ($result[1] ?? '')];
        }

        $output = [];
        $code = 0;
        @exec($cmd . ' 2>&1', $output, $code);

        return [$code, implode("\n", $output)];
    }

    private function firstLine(string $text): string
    {
        $line = trim(strtok($text, "\n") ?: '');

        return $line === '' ? '(无输出)' : $line;
    }
}
