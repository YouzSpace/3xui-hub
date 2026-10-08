<?php

/**
 * Xray 节点资产 同步 / 校验（M0：资产随面板入库，节点只连面板）。
 *
 * 开发期（拉新版本进仓库，需要网络）:
 *   php scripts/xray-node-bin-sync.php [version]
 *   HUB_GH_PROXY=https://ghfast.top php scripts/xray-node-bin-sync.php 26.4.1
 *   → 下载 4 架构 zip + .dgst，sha256 校验，更新 storage/app/xray/manifest.json
 *   → 之后 git commit + 随 3hub update 分发
 *
 * 服务器端（校验部署资产完整性，零网络）:
 *   php scripts/xray-node-bin-sync.php --verify
 *   → 对照 manifest 复验本地每个资产的 sha256/size，报告缺失/损坏
 *
 * 资产目录: storage/app/xray/node-bin/{version}/Xray-{arch}.zip
 */

require __DIR__ . '/../vendor/autoload.php';

// 引导 Laravel 应用（Storage 门面需要）
$app = require __DIR__ . '/../bootstrap/app.php';
$app->make(Illuminate\Contracts\Console\Kernel::class)->bootstrap();

const ARCHS = [
    'linux-64',
];

$argv = array_slice($_SERVER['argv'], 1);
$verifyOnly = in_array('--verify', $argv, true);
$positional = array_values(array_filter($argv, fn ($a) => ! str_starts_with($a, '--')));

$assetRoot = 'xray';
// 直用 storage_path 文件操作：Laravel 11+ 的 local 盘根在 storage/app/private，
// Storage 门面解析不到 storage/app/xray（资产目录）
$manifestFile = storage_path("app/{$assetRoot}/manifest.json");
if (! is_file($manifestFile)) {
    fwrite(STDERR, "manifest.json 不存在: {$manifestFile}\n");
    exit(1);
}
$manifest = json_decode((string) file_get_contents($manifestFile), true);
if (! is_array($manifest)) {
    fwrite(STDERR, "manifest.json 解析失败: {$manifestFile}\n");
    exit(1);
}

if ($verifyOnly) {
    exit(verifyAssets($manifest));
}

$version = $positional[0] ?? $manifest['default'];
$proxy = getenv('HUB_GH_PROXY'); // 开发期可选 GitHub 镜像前缀，默认官方直连
$base = ($proxy ? rtrim($proxy, '/') : 'https://github.com') . '/XTLS/Xray-core/releases/download/v' . $version;

echo "同步 xray v{$version} 资产（仅 linux-64 / Debian12 x86_64）...\n";

$assets = [];
$failed = 0;
foreach (ARCHS as $arch) {
    $file = 'Xray-' . $arch . '.zip';
    $dir = "{$assetRoot}/node-bin/{$version}";

    // 1. 拉官方校验清单
    $dgst = downloadFile("{$base}/{$file}.dgst", "{$dir}/{$file}.dgst");
    if ($dgst === false) {
        echo "  [FAIL] {$arch}: 下载 {$file}.dgst 失败\n";
        $failed++;
        continue;
    }
    $officialSha = parseDgstSha256($dgst);
    if ($officialSha === null) {
        echo "  [FAIL] {$arch}: .dgst 解析不出 SHA2-256\n";
        $failed++;
        continue;
    }

    // 2. 下载二进制（已存在且匹配则跳过）
    $path = storage_path('app/' . "{$dir}/{$file}");
    if (is_file($path) && hash_file('sha256', $path) === $officialSha) {
        echo "  [SKIP] {$arch}: 本地已存在且校验通过\n";
    } else {
        $ok = downloadFile("{$base}/{$file}", $path);
        if ($ok === false || hash_file('sha256', $path) !== $officialSha) {
            echo "  [FAIL] {$arch}: 下载/校验失败\n";
            $failed++;
            continue;
        }
        echo "  [OK]   {$arch}: 下载并校验通过\n";
    }

    $assets[] = [
        'arch' => $arch,
        'file' => $file,
        'sha256' => $officialSha,
        'size' => filesize($path),
    ];
}

if ($failed > 0) {
    fwrite(STDERR, "{$failed} 个架构失败，manifest 未更新。重试或检查网络。\n");
    exit(2);
}

$manifest['versions'][$version] = [
    'assets' => $assets,
    'released_at' => date('Y-m-d'),
];
// 指定 --default 时把该版本设为默认（P1 基线不动 default，P4 升级策略再切）
if (in_array('--default', $argv, true)) {
    $manifest['default'] = $version;
}
file_put_contents($manifestFile, json_encode($manifest, JSON_PRETTY_PRINT | JSON_UNESCAPED_SLASHES) . "\n");
echo "manifest 已更新（v{$version}）。记得 git commit 资产 + manifest。\n";

/** @return int 0=全部 OK，1=有缺失/损坏 */
function verifyAssets(array $manifest): int
{
    $bad = 0;
    foreach (ARCHS as $arch) {
        $asset = collect($manifest['versions'][$manifest['default']]['assets'] ?? [])->firstWhere('arch', $arch);
        if ($asset === null) {
            echo "[MISS] default 版本无 {$arch} 资产定义\n";
            $bad++;
            continue;
        }
        $path = storage_path('app/xray/node-bin/' . $manifest['default'] . '/' . $asset['file']);
        if (! is_file($path)) {
            echo "[MISS] {$arch}: " . $asset['file'] . " 不在部署目录\n";
            $bad++;
            continue;
        }
        $actual = hash_file('sha256', $path);
        if ($actual !== $asset['sha256']) {
            echo "[CORRUPT] {$arch}: sha256 不匹配（期望 {$asset['sha256']}，实际 {$actual}）\n";
            $bad++;
            continue;
        }
        echo "[OK]   {$arch}\n";
    }
    if ($bad === 0) {
        echo "v{$manifest['default']} 资产完整（4/4 校验通过）。\n";
    }
    return $bad === 0 ? 0 : 1;
}

function downloadFile(string $url, string $toPath): bool
{
    $tmp = $toPath . '.part';
    $ctx = stream_context_create(['http' => ['timeout' => 300]]);
    $data = @file_get_contents($url, false, $ctx);
    if ($data === false) {
        return false;
    }
    file_put_contents($tmp, $data);
    rename($tmp, $toPath);
    return true;
}

function parseDgstSha256(string $dgst): ?string
{
    foreach (explode("\n", $dgst) as $line) {
        if (stripos($line, 'SHA2-256') === 0) {
            return strtolower(trim(explode('=', $line)[1]));
        }
    }
    return null;
}
