<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\SiteConfig;
use App\Traits\ApiResponse;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;

/**
 * 管理后台配色：管理员自定义管理端的 6 个核心颜色（亮色/暗色各一套）。
 *
 * 存 SiteConfig 两个键（JSON，不建表）：
 *   panel_colors_light / panel_colors_dark
 *   形如 {"--accent":"#18181b","--bg-base":"#ffffff",...}
 *
 * 为什么只放这 6 个变量：覆盖「主色 / 页面底 / 卡片底 / 正文 / 次要文字 / 边框」，
 * 派生变量（hover、focus 等）继续走 tokens.css，面板不会被配到不可用。
 * 空配置 = 不注入，面板保持出厂外观；颜色值严格校验 #RRGGBB —— 这些值会拼进
 * <style> 注入前端，绝不能让白名单外的文本通过。
 */
class PanelColorsController extends Controller
{
    use ApiResponse;

    /** 允许覆盖的变量白名单（与前端「配色」页一一对应） */
    private const ALLOWED_VARS = [
        '--accent', '--bg-base', '--bg-surface',
        '--text-primary', '--text-secondary', '--border-subtle',
    ];

    private const KEY_LIGHT = 'panel_colors_light';
    private const KEY_DARK  = 'panel_colors_dark';

    /** 读取两套配色（未配置 = 空对象） */
    public function show(): JsonResponse
    {
        $cfg = SiteConfig::getMany([self::KEY_LIGHT, self::KEY_DARK]);

        return $this->success([
            'light' => $this->decode((string) ($cfg[self::KEY_LIGHT] ?? '')),
            'dark'  => $this->decode((string) ($cfg[self::KEY_DARK] ?? '')),
        ]);
    }

    /** 保存两套配色（整体覆盖；空对象 = 该模式回到默认） */
    public function save(Request $request): JsonResponse
    {
        $data = $request->validate([
            'light' => ['nullable', 'array'],
            'dark'  => ['nullable', 'array'],
        ]);

        $light = $this->sanitize($data['light'] ?? []);
        $dark  = $this->sanitize($data['dark'] ?? []);

        SiteConfig::setMany([
            self::KEY_LIGHT => $light === [] ? '' : (string) json_encode($light),
            self::KEY_DARK  => $dark === [] ? '' : (string) json_encode($dark),
        ]);

        return $this->success(null, '配色已保存，立即生效');
    }

    /** 恢复默认：清空两套配色，回到出厂外观 */
    public function reset(): JsonResponse
    {
        SiteConfig::setMany([self::KEY_LIGHT => '', self::KEY_DARK => '']);

        return $this->success(null, '已恢复默认配色');
    }

    /** 只保留白名单变量 + 合法 #RRGGBB（非法值静默丢弃，不让坏数据入库） */
    private function sanitize(array $input): array
    {
        $out = [];
        foreach (self::ALLOWED_VARS as $var) {
            $val = $input[$var] ?? null;
            if (is_string($val) && preg_match('/^#[0-9a-fA-F]{6}$/', $val)) {
                $out[$var] = strtolower($val);
            }
        }

        return $out;
    }

    /** 读存储的 JSON；坏数据（非法 JSON / 含非法色值）一律当未配置 */
    private function decode(string $raw): array
    {
        $arr = json_decode($raw, true);

        return is_array($arr) ? $this->sanitize($arr) : [];
    }
}
