<?php

namespace App\Console\Commands;

use App\WebSocket\NodeWsServer;
use Illuminate\Console\Command;

/**
 * xray 节点 WS 长连接服务（systemd 前台运行；见部署侧 3xui-hub-node-ws.service）。
 *
 * 本地开发直接 `php artisan node:ws-serve`（Ctrl+C 停止）。
 */
class NodeWsServe extends Command
{
    protected $signature = 'node:ws-serve';

    protected $description = '启动 xray 节点 WebSocket 长连接服务（Workerman）';

    public function handle(): int
    {
        $listen = (string) config('xray.ws_listen', '127.0.0.1:8091');

        $this->info("xray 节点 WS 服务启动：{$listen}");

        (new NodeWsServer($listen))->run();

        return self::SUCCESS;
    }
}
