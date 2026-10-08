<?php

namespace App\Http\Controllers\Api;

use App\Http\Controllers\Controller;
use Illuminate\Http\JsonResponse;
use Symfony\Component\HttpFoundation\BinaryFileResponse;

/**
 * 节点资产分发（xray 双通道的 M0：资产随面板入库，节点只连面板）。
 *
 * 全部公开端点（节点安装/注册前无凭据可鉴权）：
 *   GET /node-bin/manifest.json        版本+架构清单（sha256 在此下发）
 *   GET /node-bin/agent/{arch}         节点 agent 二进制（Go 版；manifest agent 段校验后下发）
 *   GET /node-bin/{version}/{arch}.zip 内核二进制（对照 manifest 校验后下发）
 *   GET /node-install.sh              节点安装脚本
 *   GET /node-agent.sh                节点 agent 脚本（旧 bash 版，保留兼容；新装走二进制）
 */
class NodeAssetController extends Controller
{
    private const ASSET_ROOT = 'xray';

    private const ARCH_MAP = [
        'linux-64' => 'Xray-linux-64.zip',
    ];

    /**
     * 版本+架构清单（含 sha256/size，节点据此自校验）。
     *
     * 裸 JSON 输出（不套 {code,data} 信封）：本组端点与 zip / 脚本同为「文件下载」
     * 语义，安装脚本直接按顶层字段解析（json["default"] 等），套信封会破坏契约。
     */
    public function manifest(): JsonResponse|\Illuminate\Http\Response
    {
        $path = storage_path('app/' . self::ASSET_ROOT . '/manifest.json');
        if (! is_file($path)) {
            return response()->json(['code' => 404, 'msg' => 'asset manifest not found'], 404);
        }

        return response(file_get_contents($path), 200, [
            'Content-Type' => 'application/json; charset=utf-8',
        ]);
    }

    /**
     * 节点 agent 二进制（Go 版；白名单架构 + sha256 复验后才下发）。
     *
     * manifest 结构：agent.assets = [{arch, file, sha256, size}, ...]，
     * 与 versions 段同构，安装脚本用同一套解析逻辑。
     */
    public function agentBinary(string $arch): BinaryFileResponse|JsonResponse
    {
        if (! in_array($arch, array_keys(self::ARCH_MAP), true)) {
            return response()->json(['code' => 404, 'msg' => "unknown arch: {$arch}"], 404);
        }

        $manifestPath = storage_path('app/' . self::ASSET_ROOT . '/manifest.json');
        if (! is_file($manifestPath)) {
            return response()->json(['code' => 500, 'msg' => 'asset manifest missing on panel'], 500);
        }
        $manifest = json_decode((string) file_get_contents($manifestPath), true) ?? [];

        $asset = collect($manifest['agent']['assets'] ?? [])->firstWhere('arch', $arch);
        if ($asset === null) {
            return response()->json(['code' => 404, 'msg' => "agent asset missing: {$arch}"], 404);
        }

        $path = storage_path('app/' . self::ASSET_ROOT . '/node-bin/agent/' . $asset['file']);
        if (! is_file($path)) {
            return response()->json(['code' => 500, 'msg' => 'agent asset missing on panel'], 500);
        }

        // 面板侧最后一道校验：下发前复验 sha256（仓库被误改/污染直接拒发）
        if (hash_file('sha256', $path) !== $asset['sha256']) {
            return response()->json(['code' => 500, 'msg' => 'agent checksum mismatch, refusing to serve'], 500);
        }

        return response()->download($path, $asset['file'], [
            'Content-Type' => 'application/octet-stream',
            'X-SHA256' => $asset['sha256'],
        ]);
    }

    /**
     * 节点二进制（白名单架构 + sha256 复验后才下发，防中间人/仓库被污染）。
     *
     * arch 兼容两种写法：短名 `linux-64`，或安装脚本按 manifest.file 拼的
     * 完整文件名 `Xray-linux-64`（路由吃掉 .zip 后缀后到这里）。
     */
    public function binary(string $version, string $arch): BinaryFileResponse|\Illuminate\Http\JsonResponse
    {
        $arch = preg_replace('/^Xray-/', '', $arch);

        if (! in_array($arch, array_keys(self::ARCH_MAP), true)) {
            return response()->json(['code' => 404, 'msg' => "unknown arch: {$arch}"], 404);
        }

        $manifestPath = storage_path('app/' . self::ASSET_ROOT . '/manifest.json');
        if (! is_file($manifestPath)) {
            return response()->json(['code' => 500, 'msg' => 'asset manifest missing on panel'], 500);
        }
        $manifest = json_decode((string) file_get_contents($manifestPath), true) ?? [];

        $asset = collect($manifest['versions'][$version]['assets'] ?? [])
            ->firstWhere('arch', $arch);
        if ($asset === null) {
            return response()->json(['code' => 404, 'msg' => "unknown version/arch: {$version}/{$arch}"], 404);
        }

        $path = storage_path('app/' . self::ASSET_ROOT . '/node-bin/' . $version . '/' . $asset['file']);
        if (! is_file($path)) {
            return response()->json(['code' => 500, 'msg' => 'asset missing on panel'], 500);
        }

        // 面板侧最后一道校验：下发前复验 sha256（仓库被误改/污染直接拒发）
        if (hash_file('sha256', $path) !== $asset['sha256']) {
            return response()->json(['code' => 500, 'msg' => 'asset checksum mismatch, refusing to serve'], 500);
        }

        return response()->download($path, $asset['file'], [
            'Content-Type' => 'application/zip',
            'X-SHA256' => $asset['sha256'],
        ]);
    }

    /** 节点安装脚本（T4 源文件在 storage/app/xray/node-install.sh）。 */
    public function nodeInstall(): BinaryFileResponse|\Illuminate\Http\JsonResponse
    {
        return $this->serveScript('node-install.sh');
    }

    /** 节点 agent 脚本（T5 源文件在 storage/app/xray/node-agent.sh）。 */
    public function nodeAgent(): BinaryFileResponse|\Illuminate\Http\JsonResponse
    {
        return $this->serveScript('node-agent.sh');
    }

    private function serveScript(string $name)
    {
        $path = storage_path('app/' . self::ASSET_ROOT . '/' . $name);
        if (! is_file($path)) {
            return response()->json(['code' => 500, 'msg' => "{$name} missing on panel"], 500);
        }
        return response()->file($path, ['Content-Type' => 'text/plain; charset=utf-8']);
    }
}
