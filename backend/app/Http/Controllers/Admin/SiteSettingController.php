<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\SiteConfig;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;

/**
 * 站点信息配置接口。
 */
class SiteSettingController extends Controller
{
    use ApiResponse;

    /** 可配置的字段 */
    private const FIELDS = [
        'site_title',
        'site_subtitle',
        'site_description',
        'site_keywords',
        'site_logo',
        'site_favicon',
        'announcement',
        'feedback_link',
    ];

    /**
     * 获取当前配置。
     */
    public function index(): \Illuminate\Http\JsonResponse
    {
        $data = SiteConfig::getMany(self::FIELDS);
        return $this->success($data);
    }

    /**
     * 更新配置。
     */
    public function update(Request $request): \Illuminate\Http\JsonResponse
    {
        $data = $request->only(self::FIELDS);
        SiteConfig::setMany($data);
        return $this->success(null, '保存成功');
    }
}
