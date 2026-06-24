<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;
use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\File;

/**
 * 数据库备份管理。
 * GET /admin-api/backup/export    → 导出数据库
 * POST /admin-api/backup/import   → 导入数据库
 * POST /admin-api/backup/preview  → 预览差异
 */
class BackupController extends Controller
{
    use ApiResponse;

    private function dbPath(): string
    {
        return database_path('database.sqlite');
    }

    /**
     * 导出数据库（下载文件）。
     */
    public function export()
    {
        $path = $this->dbPath();

        if (!File::exists($path)) {
            return $this->error('数据库文件不存在', 404);
        }

        // 断开连接，确保 WAL 数据写入主文件
        DB::disconnect();
        DB::reconnect();
        DB::statement('PRAGMA wal_checkpoint(TRUNCATE)');

        $filename = 'controlhub-backup-' . date('YmdHis') . '.sqlite';

        return response(file_get_contents($path), 200, [
            'Content-Type' => 'application/x-sqlite3',
            'Content-Disposition' => 'attachment; filename="' . $filename . '"',
            'Content-Length' => filesize($path),
        ]);
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
        $tmpPath = storage_path('app/tmp/backup_preview.sqlite');

        // 调试信息
        \Log::info('Backup preview debug', [
            'file_exists' => $file ? 'yes' : 'no',
            'file_size' => $file ? $file->getSize() : 0,
            'file_real_path' => $file ? $file->getRealPath() : 'null',
            'tmp_path' => $tmpPath,
        ]);

        $dir = dirname($tmpPath);
        if (!File::isDirectory($dir)) {
            File::makeDirectory($dir, 0755, true);
        }

        // 使用 copy 而不是 move，确保文件完整性
        $copied = copy($file->getRealPath(), $tmpPath);

        \Log::info('Backup copy result', [
            'copied' => $copied ? 'yes' : 'no',
            'tmp_size' => file_exists($tmpPath) ? filesize($tmpPath) : 0,
        ]);

        try {
            $diff = $this->calculateDiff($tmpPath);
        } catch (\Throwable $e) {
            return $this->error('无法读取备份文件：' . $e->getMessage(), 400);
        }

        \Log::info('Backup diff result', ['diff' => $diff]);

        return $this->success($diff);
    }

    /**
     * 导入数据库。
     * mode: overwrite（覆盖）| merge（增量合并）
     */
    public function import(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->validate([
            'mode' => ['required', 'in:overwrite,merge'],
        ]);

        $tmpPath = storage_path('app/tmp/backup_preview.sqlite');

        if (!File::exists($tmpPath)) {
            return $this->error('请先上传备份文件', 400);
        }

        $mode = $data['mode'];

        try {
            if ($mode === 'overwrite') {
                $this->overwriteImport($tmpPath);
            } else {
                $this->mergeImport($tmpPath);
            }
        } catch (\Throwable $e) {
            return $this->error('导入失败：' . $e->getMessage(), 500);
        } finally {
            File::delete($tmpPath);
        }

        return $this->success(null, $mode === 'overwrite' ? '已覆盖导入' : '已增量合并');
    }

    /**
     * 计算差异。
     */
    private function calculateDiff(string $importPath): array
    {
        // 动态配置导入数据库连接
        config()->set('database.connections.sqlite_import', [
            'driver' => 'sqlite',
            'database' => $importPath,
            'prefix' => '',
        ]);

        $importDb = DB::connection('sqlite_import');
        $tables = ['users', 'nodes', 'plans', 'orders', 'payment_configs', 'admins', 'node_inbounds'];
        $diff = [];

        foreach ($tables as $table) {
            $currentCount = DB::table($table)->count();
            $importCount = $importDb->table($table)->count();

            // 计算新增记录
            $newRecords = 0;
            if (in_array($table, ['users', 'plans', 'payment_configs'])) {
                $matchField = $table === 'users' ? 'email' : 'name';
                $currentNames = DB::table($table)->pluck($matchField)->filter()->toArray();
                $importNames = $importDb->table($table)->pluck($matchField)->filter()->toArray();
                $newRecords = count(array_diff($importNames, $currentNames));
            } elseif ($table === 'nodes') {
                $currentKeys = DB::table($table)->get()->map(fn ($n) => $n->host . ':' . $n->port)->toArray();
                $importKeys = $importDb->table($table)->get()->map(fn ($n) => $n->host . ':' . $n->port)->toArray();
                $newRecords = count(array_diff($importKeys, $currentKeys));
            } elseif ($table === 'orders') {
                $currentNos = DB::table($table)->pluck('order_no')->toArray();
                $importNos = $importDb->table($table)->pluck('order_no')->toArray();
                $newRecords = count(array_diff($importNos, $currentNos));
            } elseif ($table === 'node_inbounds') {
                $currentCount2 = DB::table($table)->count();
                $newRecords = max(0, $importCount - $currentCount2);
            }

            $diff[] = [
                'table' => $table,
                'current' => $currentCount,
                'import' => $importCount,
                'new' => $newRecords,
            ];
        }

        return $diff;
    }

    /**
     * 覆盖导入。
     */
    private function overwriteImport(string $importPath): void
    {
        $dbPath = $this->dbPath();

        // 断开当前连接
        DB::disconnect();

        // 删除旧的 WAL 和 SHM 文件
        File::delete($dbPath . '-wal');
        File::delete($dbPath . '-shm');

        // 替换数据库文件
        File::copy($importPath, $dbPath);

        // 重新连接
        DB::reconnect();
    }

    /**
     * 增量合并导入。
     */
    private function mergeImport(string $importPath): void
    {
        // 动态配置导入数据库连接
        config()->set('database.connections.sqlite_import', [
            'driver' => 'sqlite',
            'database' => $importPath,
            'prefix' => '',
        ]);

        $importDb = DB::connection('sqlite_import');

        DB::transaction(function () use ($importDb) {
            // 合并套餐
            $importPlans = $importDb->table('plans')->get();
            foreach ($importPlans as $plan) {
                if (!DB::table('plans')->where('name', $plan->name)->exists()) {
                    DB::table('plans')->insert((array) $plan);
                }
            }

            // 合并用户
            $importUsers = $importDb->table('users')->get();
            foreach ($importUsers as $user) {
                if ($user->email && !DB::table('users')->where('email', $user->email)->exists()) {
                    DB::table('users')->insert((array) $user);
                }
            }

            // 合并节点
            $importNodes = $importDb->table('nodes')->get();
            foreach ($importNodes as $node) {
                if (!DB::table('nodes')->where('host', $node->host)->where('port', $node->port)->exists()) {
                    DB::table('nodes')->insert((array) $node);
                }
            }

            // 合并支付配置
            $importPayments = $importDb->table('payment_configs')->get();
            foreach ($importPayments as $payment) {
                if (!DB::table('payment_configs')->where('name', $payment->name)->exists()) {
                    DB::table('payment_configs')->insert((array) $payment);
                }
            }

            // 合并节点入站
            $importInbounds = $importDb->table('node_inbounds')->get();
            foreach ($importInbounds as $inbound) {
                if (!DB::table('node_inbounds')
                    ->where('node_id', $inbound->node_id)
                    ->where('protocol', $inbound->protocol)
                    ->where('inbound_id', $inbound->inbound_id)
                    ->exists()) {
                    DB::table('node_inbounds')->insert((array) $inbound);
                }
            }

            // 合并订单
            $importOrders = $importDb->table('orders')->get();
            foreach ($importOrders as $order) {
                if (!DB::table('orders')->where('order_no', $order->order_no)->exists()) {
                    DB::table('orders')->insert((array) $order);
                }
            }
        });
    }
}
