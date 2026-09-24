<?php

namespace App\Http\Controllers\Admin;

use App\Http\Controllers\Controller;
use App\Models\SiteConfig;
use App\Traits\ApiResponse;
use Illuminate\Http\Request;

/**
 * 订阅格式配置接口。
 */
class SubscriptionSettingController extends Controller
{
    use ApiResponse;

    /** 可配置的字段 */
    private const FIELDS = [
        'sub_clash_enabled',
        'sub_singbox_enabled',
        'sub_show_traffic',
        'sub_show_expire',
        'sub_show_flag',
        'sub_show_userid',
        'sub_rename_enabled',
        'sub_rename_regex',
        'sub_rename_replacement',
        'sub_custom_info_enabled',
        'sub_custom_info_text',
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
