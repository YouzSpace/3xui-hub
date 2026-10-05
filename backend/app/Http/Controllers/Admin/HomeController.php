<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\SiteConfig;
use App\Traits\ApiResponse;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;

/**
 * 自定义公开首页：管理员用 HTML + CSS 自己搭首页。
 *
 * 存SiteConfig 两个键（不建表）：
 *   home_custom_html — HTML 正文，上限 1MB
 *   home_custom_css  — 独立样式区，上限 256KB
 *
 * 为空 = 用前端默认首页（见 views/user/Home.vue 的原有内容），
 * 这样老面板升级后行为不变，管理员不填就完全看不到本功能的影响。
 */
class HomeController extends Controller
{
    use ApiResponse;

    /** HTML 上限 1MB；CSS 单独限 256KB，两者分开免得有人拿 CSS 撑爆字段 */
    private const MAX_HTML = 1048576;
    private const MAX_CSS  = 262144;

    private const KEYS = ['home_custom_html', 'home_custom_css'];

    /** 读取当前自定义内容 */
    public function show(): JsonResponse
    {
        $cfg = SiteConfig::getMany(self::KEYS);

        return $this->success([
            'html' => (string) ($cfg['home_custom_html'] ?? ''),
            'css'  => (string) ($cfg['home_custom_css'] ?? ''),
            'max_html' => self::MAX_HTML,
            'max_css'  => self::MAX_CSS,
            // 是否已启用自定义（空 HTML = 未启用）
            'enabled' => trim((string) ($cfg['home_custom_html'] ?? '')) !== '',
        ]);
    }

    /**
     * 保存自定义首页。
     *
     * 这里只做**存储**，不做消毒：消毒放在前端渲染前（DOMPurify），
     * 因为预览和公开首页走同一条清洗路径，放在一处才不会两边规则不一致。
     */
    public function save(Request $request): JsonResponse
    {
        $data = $request->validate([
            'html' => ['nullable', 'string', 'max:' . self::MAX_HTML],
            'css'  => ['nullable', 'string', 'max:' . self::MAX_CSS],
        ]);

        SiteConfig::setMany([
            // 空值也要写：管理员可能想清空自定义回到默认首页
            'home_custom_html' => $data['html'] ?? '',
            'home_custom_css'  => $data['css'] ?? '',
        ]);

        return $this->success(null, '首页已保存，公开首页立即生效');
    }

    /**
     * 恢复默认首页：清空自定义内容。
     * 前端不做二次确认的话，误点一次就丢配置，所以这里只提供接口，
     * 确认弹窗由前端负责。
     */
    public function reset(): JsonResponse
    {
        SiteConfig::setMany([
            'home_custom_html' => '',
            'home_custom_css'  => '',
        ]);

        return $this->success(null, '已恢复默认首页');
    }
}