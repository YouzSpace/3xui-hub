<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;
use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\File;
use Illuminate\Support\Facades\Process;

/**
 * 数据库备份管理（MySQL）。
 * GET /admin-api/backup/export    → 导出数据库
 * POST /admin-api/backup/import   → 导入数据库
 * POST /admin-api/backup/preview  → 预览差异
 */
class BackupController extends Controller
{
    use ApiResponse;

    /**
     * 获取 MySQL 连接配置。
     */
    private function dbConfig(): array
    {
        return [
            'host' => config('database.connections.mysql.host', '127.0.0.1'),
            'port' => config('database.connections.mysql.port', '3306'),
            'database' => config('database.connections.mysql.database', 'controlhub'),
            'username' => config('database.connections.mysql.username', 'root'),
            'password' => config('database.connections.mysql.password', ''),
        ];
    }

    /**
     * 查找 mysqldump / mysql 命令路径。
     * 优先用 PATH 中的命令，找不到则从 DB_HOST 配置推断常见安装路径。
     */
    private function findMysqlTool(string $tool): string
    {
        // 1. PATH 中有直接可用的命令
        $checkCmd = PHP_OS_FAMILY === 'Windows' ? "where $tool 2>nul" : "which $tool 2>/dev/null";
        exec($checkCmd, $out, $code);
        if ($code === 0 && !empty($out)) {
            return $tool;
        }

        // 2. 常见安装路径（Windows 手动安装 / Linux BaoTa）
        $candidates = [];
        if (PHP_OS_FAMILY === 'Windows') {
            // 从 MySQL bin 目录推断
            foreach (['C:/Program Files/MySQL', 'F:/wykf/mysql'] as $base) {
                if (is_dir($base)) {
                    $dirs = glob($base . '/mysql-*/bin/' . $tool . '.exe');
                    if ($dirs) { $candidates[] = $dirs[0]; }
                }
            }
        } else {
            // Linux BaoTa / 常见路径
            $candidates = [
                "/usr/bin/$tool",
                "/usr/local/bin/$tool",
                "/www/server/mysql/bin/$tool",
            ];
        }

        foreach ($candidates as $path) {
            if (file_exists($path)) {
                return $path;
            }
        }

        // 3. 兜底返回命令名（让调用方报错）
        return $tool;
    }

    /**
     * 确保 tmp 目录存在。
     */
    private function tmpDir(): string
    {
        $dir = storage_path('app/tmp');
        if (!File::isDirectory($dir)) {
            File::makeDirectory($dir, 0755, true);
        }
        return $dir;
    }

    /**
     * 导出数据库（zip包：dump.sql + .env）。
     */
    public function export()
    {
        $cfg = $this->dbConfig();
        $filename = 'controlhub-backup-' . date('YmdHis') . '.zip';
        $tmpDir = $this->tmpDir();
        $dumpPath = $tmpDir . '/dump.sql';

        // mysqldump 导出（直接重定向到文件，避免 exec 捕获时编码损坏）
        $mysqldump = $this->findMysqlTool('mysqldump');
        // --set-gtid-purged=OFF 是 MySQL 专用，MariaDB 不支持
        $isMariaDb = stripos(shell_exec('mysql --version 2>/dev/null') ?? '', 'mariadb') !== false;
        $gtidOpt = $isMariaDb ? '' : ' --set-gtid-purged=OFF';
        $cmd = sprintf(
            '%s --host=%s --port=%s --user=%s --password=%s --single-transaction --routines --triggers%s %s > %s 2>/dev/null',
            escapeshellarg($mysqldump),
            escapeshellarg($cfg['host']),
            escapeshellarg($cfg['port']),
            escapeshellarg($cfg['username']),
            escapeshellarg($cfg['password']),
            $gtidOpt,
            escapeshellarg($cfg['database']),
            escapeshellarg($dumpPath)
        );
        $exitCode = 0;
        exec($cmd, $output, $exitCode);

        if ($exitCode !== 0 || !File::exists($dumpPath) || File::size($dumpPath) === 0) {
            File::delete($dumpPath);
            return $this->error('数据库导出失败（mysqldump 返回 ' . $exitCode . '）', 500);
        }

        // 打包 zip
        $tmpPath = $tmpDir . '/' . $filename;
        $zip = new \ZipArchive();
        if ($zip->open($tmpPath, \ZipArchive::CREATE | \ZipArchive::OVERWRITE) !== true) {
            File::delete($dumpPath);
            return $this->error('无法创建备份文件', 500);
        }

        $zip->addFile($dumpPath, 'dump.sql');

        $envPath = base_path('.env');
        if (File::exists($envPath)) {
            $zip->addFile($envPath, '.env');
        }

        $zip->close();
        File::delete($dumpPath);

        $content = file_get_contents($tmpPath);
        File::delete($tmpPath);

        return response($content, 200, [
            'Content-Type' => 'application/zip',
            'Content-Disposition' => 'attachment; filename="' . $filename . '"',
            'Content-Length' => strlen($content),
        ]);
    }

    /**
     * 解压备份文件，返回 SQL 文件路径。兼容 .zip 和 .sql。
     * $restoreEnv: 是否恢复 .env（仅导入时恢复）。
     */
    private function extractBackupFile(string $filePath, bool $restoreEnv = false): string
    {
        $tmpDir = $this->tmpDir();
        $ext = strtolower(pathinfo($filePath, PATHINFO_EXTENSION));

        if ($ext === 'zip') {
            $zip = new \ZipArchive();
            if ($zip->open($filePath) !== true) {
                throw new \RuntimeException('无法打开 ZIP 文件');
            }

            // 导入时才恢复 .env
            if ($restoreEnv) {
                $envIndex = $zip->locateName('.env');
                if ($envIndex !== false) {
                    $envContent = $zip->getFromIndex($envIndex);
                    $envPath = base_path('.env');
                    if (File::exists($envPath)) {
                        File::copy($envPath, $envPath . '.bak');
                    }
                    file_put_contents($envPath, $envContent);
                    \Log::info('Backup import: .env restored');
                }
            }

            // 提取 dump.sql
            $sqlIndex = $zip->locateName('dump.sql');
            if ($sqlIndex === false) {
                $zip->close();
                // 兼容旧格式
                $oldIndex = $zip->locateName('database.sqlite');
                if ($oldIndex !== false) {
                    throw new \RuntimeException('这是旧版 SQLite 备份文件，无法在 MySQL 模式下导入');
                }
                throw new \RuntimeException('ZIP 中未找到 dump.sql');
            }

            $sqlTmpPath = $tmpDir . '/backup_preview.sql';
            file_put_contents($sqlTmpPath, $zip->getFromIndex($sqlIndex));
            $zip->close();

            return $sqlTmpPath;
        } elseif ($ext === 'sql') {
            // 直接 .sql 文件
            $sqlTmpPath = $tmpDir . '/backup_preview.sql';
            copy($filePath, $sqlTmpPath);
            return $sqlTmpPath;
        } else {
            throw new \RuntimeException('不支持的备份文件格式：' . $ext);
        }
    }

    /**
     * 解析 .env 文件为键值数组。
     */
    private function parseEnv(string $path): array
    {
        $result = [];
        if (!File::exists($path)) return $result;
        foreach (File::lines($path) as $line) {
            $line = trim($line);
            if ($line === '' || $line[0] === '#') continue;
            $parts = explode('=', $line, 2);
            if (count($parts) === 2) {
                $result[trim($parts[0])] = trim($parts[1], " \t\n\r\0\x0B\"");
            }
        }
        return $result;
    }

    /**
     * 更新 .env 文件中指定 key 的值。
     */
    private function updateEnvKey(string $key, string $value): void
    {
        $envPath = base_path('.env');
        if (!File::exists($envPath)) return;

        $content = File::get($envPath);
        $pattern = '/^' . preg_quote($key, '/') . '=.*/m';
        $replacement = $key . '=' . $value;

        if (preg_match($pattern, $content)) {
            $content = preg_replace($pattern, $replacement, $content);
        } else {
            $content = rtrim($content) . "\n" . $replacement . "\n";
        }

        File::put($envPath, $content);
        \Log::info("Backup import: {$key} updated to {$value}");
    }

    /**
     * 对比备份 .env 与当前 .env，返回服务器相关配置的差异。
     */
    private function compareEnv(string $backupPath): array
    {
        $zip = new \ZipArchive();
        if ($zip->open($backupPath) !== true) return [];

        $envIndex = $zip->locateName('.env');
        if ($envIndex === false) { $zip->close(); return []; }

        $backupEnvContent = $zip->getFromIndex($envIndex);
        $zip->close();

        $tmpEnv = storage_path('app/tmp/backup_env_compare');
        file_put_contents($tmpEnv, $backupEnvContent);
        $backupEnv = $this->parseEnv($tmpEnv);
        File::delete($tmpEnv);

        $currentEnv = $this->parseEnv(base_path('.env'));

        $serverKeys = [
            'APP_NAME', 'APP_URL', 'APP_KEY', 'APP_DEBUG', 'APP_ENV',
            'DB_CONNECTION', 'DB_HOST', 'DB_PORT', 'DB_DATABASE',
            'SESSION_DRIVER', 'SESSION_DOMAIN',
            'MAIL_MAILER', 'MAIL_HOST', 'MAIL_PORT', 'MAIL_FROM_ADDRESS',
            'REDIS_HOST', 'REDIS_PORT',
        ];

        $diff = [];
        foreach ($serverKeys as $key) {
            $backupVal = $backupEnv[$key] ?? null;
            $currentVal = $currentEnv[$key] ?? null;
            if ($backupVal !== null && $backupVal !== $currentVal) {
                $diff[] = [
                    'key' => $key,
                    'current' => $currentVal ?? '（未设置）',
                    'backup' => $backupVal,
                ];
            }
        }

        return $diff;
    }

    /**
     * 预览导入差异。
     */
    public function preview(Request $request): \Illuminate\Http\JsonResponse
    {
        $request->validate([
            'file' => ['required', 'file', 'max:51200'], // 50MB
        ]);

        $file = $request->file('file');
        $ext = strtolower(pathinfo($file->getClientOriginalName(), PATHINFO_EXTENSION));

        // 保留原文件到 tmp，import 时需要
        $tmpDir = $this->tmpDir();
        $savedPath = $tmpDir . '/backup_upload.' . $ext;
        copy($file->getRealPath(), $savedPath);

        try {
            $sqlPath = $this->extractBackupFile($savedPath, false);
        } catch (\Throwable $e) {
            return $this->error('无法读取备份文件：' . $e->getMessage(), 400);
        }

        try {
            $diff = $this->calculateDiffFromSql($sqlPath);
        } catch (\Throwable $e) {
            return $this->error('无法分析备份文件：' . $e->getMessage(), 400);
        }

        // 检查备份是否包含 .env
        $hasEnv = false;
        if ($ext === 'zip') {
            $zip = new \ZipArchive();
            if ($zip->open($savedPath) === true) {
                $hasEnv = $zip->locateName('.env') !== false;
                $zip->close();
            }
        }

        return $this->success([
            'diff' => $diff,
            'has_env' => $hasEnv,
            'env_diff' => $hasEnv ? $this->compareEnv($savedPath) : [],
        ]);
    }

    /**
     * 从 SQL dump 文件解析各表行数差异。
     */
    private function calculateDiffFromSql(string $sqlPath): array
    {
        $tables = ['users', 'nodes', 'plans', 'orders', 'payment_configs', 'admins', 'node_inbounds'];

        // 解析 dump.sql 中每个表的 INSERT 行数
        $importCounts = $this->parseDumpTableCounts($sqlPath);

        $diff = [];
        foreach ($tables as $table) {
            $currentCount = DB::table($table)->count();
            $importCount = $importCounts[$table] ?? 0;

            $diff[] = [
                'table' => $table,
                'current' => $currentCount,
                'import' => $importCount,
                'new' => max(0, $importCount - $currentCount),
            ];
        }

        return $diff;
    }

    /**
     * 解析 mysqldump 文件中每个表的行数（通过 INSERT 语句计数）。
     */
    private function parseDumpTableCounts(string $sqlPath): array
    {
        $counts = [];
        $currentTable = null;

        $handle = fopen($sqlPath, 'r');
        if (!$handle) return $counts;

        while (($line = fgets($handle)) !== false) {
            // 匹配 INSERT INTO `table_name` 或 INSERT INTO `table_name` VALUES
            if (preg_match('/^INSERT\s+INTO\s+`?(\w+)`?\s/i', $line, $m)) {
                $table = $m[1];
                if (!isset($counts[$table])) {
                    $counts[$table] = 0;
                }
                // 计算这行 INSERT 里的值组数（每个 VALUES (...) 算一行）
                $counts[$table] += substr_count($line, '),(') + 1;
            }
        }

        fclose($handle);
        return $counts;
    }

    /**
     * 导入数据库。
     * mode: overwrite（覆盖）| merge（增量合并）
     */
    public function import(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->validate([
            'mode' => ['required', 'in:overwrite,merge'],
            'restore_env' => ['nullable', 'boolean'],
            'site_url' => ['nullable', 'url'],
        ]);

        // 查找上传的备份文件
        $tmpDir = $this->tmpDir();
        $backupFile = null;
        foreach (['zip', 'sql'] as $ext) {
            $candidate = $tmpDir . '/backup_upload.' . $ext;
            if (File::exists($candidate)) {
                $backupFile = $candidate;
                break;
            }
        }

        if (!$backupFile) {
            return $this->error('请先上传备份文件', 400);
        }

        $mode = $data['mode'];

        try {
            // 导入时恢复 .env（如果是 zip）
            $sqlPath = $this->extractBackupFile($backupFile, !empty($data['restore_env']));

            // 如果用户指定了站点地址，更新 .env 中的 APP_URL
            if (!empty($data['site_url']) && !empty($data['restore_env'])) {
                $this->updateEnvKey('APP_URL', $data['site_url']);
            }

            if ($mode === 'overwrite') {
                $this->overwriteImport($sqlPath);
            } else {
                $this->mergeImport($sqlPath);
            }
        } catch (\Throwable $e) {
            return $this->error('导入失败：' . $e->getMessage(), 500);
        } finally {
            if (isset($sqlPath)) File::delete($sqlPath);
            File::delete($backupFile);
        }

        return $this->success(null, $mode === 'overwrite' ? '已覆盖导入' : '已增量合并');
    }

    /**
     * 执行 SQL 文件导入（覆盖模式）。
     */
    private function overwriteImport(string $sqlPath): void
    {
        $cfg = $this->dbConfig();
        $mysql = $this->findMysqlTool('mysql');

        $cmd = sprintf(
            '%s --host=%s --port=%s --user=%s --password=%s %s < %s 2>nul',
            escapeshellarg($mysql),
            escapeshellarg($cfg['host']),
            escapeshellarg($cfg['port']),
            escapeshellarg($cfg['username']),
            escapeshellarg($cfg['password']),
            escapeshellarg($cfg['database']),
            escapeshellarg($sqlPath)
        );

        $output = [];
        $exitCode = 0;
        exec($cmd, $output, $exitCode);

        if ($exitCode !== 0) {
            throw new \RuntimeException('SQL 导入失败：' . implode("\n", $output));
        }
    }

    /**
     * 增量合并导入。
     * 先导入到临时库，再逐表合并到主库。
     */
    private function mergeImport(string $sqlPath): void
    {
        $cfg = $this->dbConfig();
        $mysql = $this->findMysqlTool('mysql');
        $tmpDb = 'controlhub_merge_tmp_' . time();

        // 创建临时数据库
        DB::statement("CREATE DATABASE `{$tmpDb}` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci");

        try {
            // 导入 SQL 到临时库
            $cmd = sprintf(
                '%s --host=%s --port=%s --user=%s --password=%s %s < %s 2>nul',
                escapeshellarg($mysql),
                escapeshellarg($cfg['host']),
                escapeshellarg($cfg['port']),
                escapeshellarg($cfg['username']),
                escapeshellarg($cfg['password']),
                escapeshellarg($tmpDb),
                escapeshellarg($sqlPath)
            );

            $output = [];
            $exitCode = 0;
            exec($cmd, $output, $exitCode);

            if ($exitCode !== 0) {
                throw new \RuntimeException('临时库导入失败：' . implode("\n", $output));
            }

            // 配置临时库连接
            config()->set("database.connections.merge_tmp", [
                'driver' => 'mysql',
                'host' => $cfg['host'],
                'port' => $cfg['port'],
                'database' => $tmpDb,
                'username' => $cfg['username'],
                'password' => $cfg['password'],
                'charset' => 'utf8mb4',
                'collation' => 'utf8mb4_unicode_ci',
            ]);

            $importDb = DB::connection('merge_tmp');

            DB::transaction(function () use ($importDb) {
                // 合并套餐
                foreach ($importDb->table('plans')->get() as $plan) {
                    if (!DB::table('plans')->where('name', $plan->name)->exists()) {
                        DB::table('plans')->insert((array) $plan);
                    }
                }

                // 合并用户
                foreach ($importDb->table('users')->get() as $user) {
                    if ($user->email && !DB::table('users')->where('email', $user->email)->exists()) {
                        DB::table('users')->insert((array) $user);
                    }
                }

                // 合并节点
                foreach ($importDb->table('nodes')->get() as $node) {
                    if (!DB::table('nodes')->where('host', $node->host)->where('port', $node->port)->exists()) {
                        DB::table('nodes')->insert((array) $node);
                    }
                }

                // 合并支付配置
                foreach ($importDb->table('payment_configs')->get() as $payment) {
                    if (!DB::table('payment_configs')->where('name', $payment->name)->exists()) {
                        DB::table('payment_configs')->insert((array) $payment);
                    }
                }

                // 合并节点入站
                foreach ($importDb->table('node_inbounds')->get() as $inbound) {
                    if (!DB::table('node_inbounds')
                        ->where('node_id', $inbound->node_id)
                        ->where('protocol', $inbound->protocol)
                        ->where('inbound_id', $inbound->inbound_id)
                        ->exists()) {
                        DB::table('node_inbounds')->insert((array) $inbound);
                    }
                }

                // 合并订单
                foreach ($importDb->table('orders')->get() as $order) {
                    if (!DB::table('orders')->where('order_no', $order->order_no)->exists()) {
                        DB::table('orders')->insert((array) $order);
                    }
                }
            });
        } finally {
            // 删除临时数据库
            try {
                DB::statement("DROP DATABASE IF EXISTS `{$tmpDb}`");
            } catch (\Throwable $e) {
                \Log::warning("Failed to drop temp DB {$tmpDb}: " . $e->getMessage());
            }
        }
    }
}
