<?php

use App\Http\Controllers\Admin\AuthController as AdminAuthController;
use App\Http\Controllers\Admin\AsyncTaskController as AdminAsyncTaskController;
use App\Http\Controllers\Admin\BackupController as AdminBackupController;
use App\Http\Controllers\Admin\DiscountCodeController as AdminDiscountCodeController;
use App\Http\Controllers\Admin\DomainController as AdminDomainController;
use App\Http\Controllers\Admin\EmailController as AdminEmailController;
use App\Http\Controllers\Admin\HomeController as AdminHomeController;
use App\Http\Controllers\Admin\NodeController as AdminNodeController;
use App\Http\Controllers\Admin\OrderController as AdminOrderController;
use App\Http\Controllers\Admin\PanelColorsController as AdminPanelColorsController;
use App\Http\Controllers\Admin\PaymentController as AdminPaymentController;
use App\Http\Controllers\Admin\PlanController as AdminPlanController;
use App\Http\Controllers\Admin\SyncTrafficController as AdminSyncTrafficController;
use App\Http\Controllers\Admin\ThirdPartyController as AdminThirdPartyController;
use App\Http\Controllers\Admin\UserController as AdminUserController;
use App\Http\Controllers\Admin\SiteSettingController as AdminSiteSettingController;
use App\Http\Controllers\Admin\SubscriptionSettingController as AdminSubscriptionSettingController;
use App\Http\Controllers\Admin\SystemStatusController as AdminSystemStatusController;
use App\Http\Controllers\Admin\TutorialController as AdminTutorialController;
use App\Http\Controllers\Admin\XrayInboundController as AdminXrayInboundController;
use App\Http\Controllers\Admin\XrayOutboundController as AdminXrayOutboundController;
use App\Http\Controllers\Admin\XrayRoutingController as AdminXrayRoutingController;
use App\Http\Controllers\Admin\NodeWarpController as AdminNodeWarpController;
use Illuminate\Support\Facades\Route;

/*
|--------------------------------------------------------------------------
| 节点资产分发（xray 双通道 M0：资产随面板入库，节点只连面板，GitHub 零依赖）
| 全部公开端点——节点安装/注册前无凭据可鉴权
|--------------------------------------------------------------------------
*/
Route::get('/node-install.sh', [\App\Http\Controllers\Api\NodeAssetController::class, 'nodeInstall']);
Route::get('/node-agent.sh', [\App\Http\Controllers\Api\NodeAssetController::class, 'nodeAgent']);
Route::get('/node-bin/manifest.json', [\App\Http\Controllers\Api\NodeAssetController::class, 'manifest']);
// agent 二进制路由必须排在 {version}/{arch}.zip 之前（虽然 {arch}.zip 后缀模式不会吞它，双保险）
Route::get('/node-bin/agent/{arch}', [\App\Http\Controllers\Api\NodeAssetController::class, 'agentBinary']);
Route::get('/node-bin/{version}/{arch}.zip', [\App\Http\Controllers\Api\NodeAssetController::class, 'binary']);

/*
|--------------------------------------------------------------------------
| Admin API 路由（session 鉴权，JSON；前缀 /admin-api，与 SPA 页面 /admin 分离）
|--------------------------------------------------------------------------
*/

Route::post('/admin-api/login', [AdminAuthController::class, 'login']);

Route::middleware('admin.auth')->prefix('admin-api')->group(function () {
    Route::post('/logout', [AdminAuthController::class, 'logout']);
    Route::get('/settings', [AdminAuthController::class, 'settings']);
    Route::post('/change-password', [AdminAuthController::class, 'changePassword']);
    Route::post('/update-username', [AdminAuthController::class, 'updateUsername']);

    // Google 2FA
    Route::post('/google2fa/generate', [AdminAuthController::class, 'google2faGenerate']);
    Route::post('/google2fa/enable', [AdminAuthController::class, 'google2faEnable']);
    Route::post('/google2fa/disable', [AdminAuthController::class, 'google2faDisable']);

    // M3 用户管理
    Route::get('/users', [AdminUserController::class, 'index']);
    Route::post('/users', [AdminUserController::class, 'store']);
    Route::get('/users/{user}', [AdminUserController::class, 'show']);
    Route::put('/users/{user}', [AdminUserController::class, 'update']);
    Route::delete('/users/{user}', [AdminUserController::class, 'destroy']);
    Route::post('/users/{user}/reset-traffic', [AdminUserController::class, 'resetTraffic']);
    Route::post('/users/{user}/reset-password', [AdminUserController::class, 'resetPassword']);
    Route::post('/users/{user}/protocol', [AdminUserController::class, 'switchProtocol']);
    Route::post('/users/{user}/renew', [AdminUserController::class, 'renew']);

    // M4 节点管理
    Route::get('/nodes', [AdminNodeController::class, 'index']);
    Route::post('/nodes', [AdminNodeController::class, 'store']);
    // 流量倍率（原始倍率 / 用户端展示开关）—— 必须排在 /nodes/{node} 参数路由之前，
    // 否则 multiplier 会被当成 node id 吞掉（同 /payments/protocols 的先例）
    Route::put('/nodes/multiplier/original', [AdminNodeController::class, 'updateOriginalMultiplier']);
    Route::put('/nodes/multiplier/display', [AdminNodeController::class, 'updateMultiplierDisplay']);
    // 第三方订阅（固定路径）—— 必须排在 /nodes/{node} 参数路由之前，否则 third-party 会被当成 node id 吞掉
    Route::get('/nodes/third-party', [AdminThirdPartyController::class, 'index']);
    Route::post('/nodes/third-party', [AdminThirdPartyController::class, 'store']);
    Route::put('/nodes/third-party/switch', [AdminThirdPartyController::class, 'switch']);
    Route::post('/nodes/third-party/{sub}/refresh', [AdminThirdPartyController::class, 'refresh']);
    Route::put('/nodes/third-party/{sub}', [AdminThirdPartyController::class, 'update']);
    Route::delete('/nodes/third-party/{sub}', [AdminThirdPartyController::class, 'destroy']);
    Route::get('/nodes/{node}', [AdminNodeController::class, 'show']);
    Route::put('/nodes/{node}', [AdminNodeController::class, 'update']);
    Route::delete('/nodes/{node}', [AdminNodeController::class, 'destroy']);
    Route::post('/nodes/{node}/test', [AdminNodeController::class, 'test']);
    Route::post('/nodes/{node}/upgrade-agent', [AdminNodeController::class, 'upgradeAgent']);
    Route::post('/nodes/probe-inbounds', [AdminNodeController::class, 'probeInbounds']);

    // xray 节点配置能力（入站 / 出站 / 路由 / WARP；仅 driver_type=xray 的节点可用，控制器内校验）
    Route::get('/nodes/{node}/xray-inbounds', [AdminXrayInboundController::class, 'index']);
    Route::post('/nodes/{node}/xray-inbounds', [AdminXrayInboundController::class, 'store']);
    // 密钥生成必须排在 {inbound} 参数路由之前（同 third-party 的先例）
    Route::post('/nodes/{node}/xray-inbounds/reality-keypair', [AdminXrayInboundController::class, 'realityKeypair']);
    Route::put('/nodes/{node}/xray-inbounds/{inbound}', [AdminXrayInboundController::class, 'update']);
    Route::delete('/nodes/{node}/xray-inbounds/{inbound}', [AdminXrayInboundController::class, 'destroy']);

    Route::get('/nodes/{node}/xray-outbounds', [AdminXrayOutboundController::class, 'index']);
    Route::post('/nodes/{node}/xray-outbounds', [AdminXrayOutboundController::class, 'store']);
    Route::post('/nodes/{node}/xray-outbounds/import', [AdminXrayOutboundController::class, 'import']);
    Route::put('/nodes/{node}/xray-outbounds/{outbound}', [AdminXrayOutboundController::class, 'update']);
    Route::delete('/nodes/{node}/xray-outbounds/{outbound}', [AdminXrayOutboundController::class, 'destroy']);
    Route::post('/nodes/{node}/xray-outbounds/{outbound}/test', [AdminXrayOutboundController::class, 'test']);

    Route::get('/nodes/{node}/xray-routing', [AdminXrayRoutingController::class, 'index']);
    Route::post('/nodes/{node}/xray-routing', [AdminXrayRoutingController::class, 'store']);
    Route::post('/nodes/{node}/xray-routing/test', [AdminXrayRoutingController::class, 'test']);
    Route::put('/nodes/{node}/xray-routing/{rule}', [AdminXrayRoutingController::class, 'update']);
    Route::delete('/nodes/{node}/xray-routing/{rule}', [AdminXrayRoutingController::class, 'destroy']);

    Route::get('/nodes/{node}/warp', [AdminNodeWarpController::class, 'show']);
    Route::post('/nodes/{node}/warp/register', [AdminNodeWarpController::class, 'register']);
    Route::post('/nodes/{node}/warp/apply', [AdminNodeWarpController::class, 'apply']);
    Route::post('/nodes/{node}/warp/rotate', [AdminNodeWarpController::class, 'rotate']);
    Route::put('/nodes/{node}/warp', [AdminNodeWarpController::class, 'update']);
    Route::delete('/nodes/{node}/warp', [AdminNodeWarpController::class, 'destroy']);

    // 套餐管理
    Route::get('/plans', [AdminPlanController::class, 'index']);
    Route::post('/plans', [AdminPlanController::class, 'store']);
    Route::get('/plans/{plan}', [AdminPlanController::class, 'show']);
    Route::put('/plans/{plan}', [AdminPlanController::class, 'update']);
    Route::post('/plans/{plan}/deactivate', [AdminPlanController::class, 'deactivate']);
    Route::post('/plans/{plan}/activate', [AdminPlanController::class, 'activate']);

    // 订单管理
    Route::get('/orders', [AdminOrderController::class, 'index']);

    // 流量同步
    Route::post('/sync-traffic', [AdminSyncTrafficController::class, 'sync']);

    // 异步任务
    Route::get('/async-tasks', [AdminAsyncTaskController::class, 'index']);
    Route::post('/async-tasks/{asyncTask}/retry', [AdminAsyncTaskController::class, 'retry']);

    // 备份管理
    Route::get('/backup/export', [AdminBackupController::class, 'export']);
    Route::post('/backup/preview', [AdminBackupController::class, 'preview']);
    Route::post('/backup/import', [AdminBackupController::class, 'import']);

    // 邮箱配置
    Route::get('/email', [AdminEmailController::class, 'show']);
    Route::put('/email', [AdminEmailController::class, 'save']);
    Route::post('/email/test', [AdminEmailController::class, 'test']);
    // 自动通知配置（开关全部默认关闭，管理员自行开启）
    Route::get('/email/notify', [AdminEmailController::class, 'notifyConfig']);
    Route::put('/email/notify', [AdminEmailController::class, 'saveNotifyConfig']);
    // 批量发信（群发/单发，限速默认关闭由管理员填上限）
    Route::post('/email/batch-send', [AdminEmailController::class, 'batchSend']);
    // 按月定时发信（管理员选每月第 N 天 HH:MM，到点自动发；默认关闭）
    Route::get('/email/schedule-mail', [AdminEmailController::class, 'scheduleMailConfig']);
    Route::put('/email/schedule-mail', [AdminEmailController::class, 'saveScheduleMailConfig']);
    // 发信日志
    Route::get('/email/logs', [AdminEmailController::class, 'mailLogs']);

    // 多域名管理
    Route::get('/domains', [AdminDomainController::class, 'index']);
    Route::post('/domains', [AdminDomainController::class, 'store']);
    Route::post('/domains/{id}/primary', [AdminDomainController::class, 'setPrimary']);
    Route::post('/domains/{id}/apply', [AdminDomainController::class, 'apply']);
    Route::post('/domains/{id}/renew', [AdminDomainController::class, 'renew']);
    Route::delete('/domains/{id}', [AdminDomainController::class, 'destroy']);

    // 支付配置管理
    Route::get('/payments', [AdminPaymentController::class, 'index']);
    Route::post('/payments', [AdminPaymentController::class, 'store']);
    // 协议清单（动态表单元数据）—— 必须排在 /payments/{payment} 之前，否则被参数路由吞掉
    Route::get('/payments/protocols', [AdminPaymentController::class, 'protocols']);
    Route::get('/payments/{payment}', [AdminPaymentController::class, 'show']);
    Route::put('/payments/{payment}', [AdminPaymentController::class, 'update']);
    Route::delete('/payments/{payment}', [AdminPaymentController::class, 'destroy']);

    // 自定义公开首页（HTML + CSS）
    Route::get('/home-custom', [AdminHomeController::class, 'show']);
    Route::put('/home-custom', [AdminHomeController::class, 'save']);
    Route::post('/home-custom/reset', [AdminHomeController::class, 'reset']);

    // 管理后台配色（自定义管理端的核心颜色，亮/暗两套）
    Route::get('/panel-colors', [AdminPanelColorsController::class, 'show']);
    Route::put('/panel-colors', [AdminPanelColorsController::class, 'save']);
    Route::post('/panel-colors/reset', [AdminPanelColorsController::class, 'reset']);

    // 站点信息配置
    Route::get('/site-settings', [AdminSiteSettingController::class, 'index']);
    Route::put('/site-settings', [AdminSiteSettingController::class, 'update']);

    // 订阅格式配置
    Route::get('/subscription-settings', [AdminSubscriptionSettingController::class, 'index']);
    Route::put('/subscription-settings', [AdminSubscriptionSettingController::class, 'update']);

    // 系统状态（操作日志 + 定时任务 + worker）
    Route::get('/system/status', [AdminSystemStatusController::class, 'status']);

    // 教程管理
    Route::get('/tutorials', [AdminTutorialController::class, 'index']);
    Route::post('/tutorials', [AdminTutorialController::class, 'store']);
    Route::put('/tutorials/{tutorial}', [AdminTutorialController::class, 'update']);
    Route::delete('/tutorials/{tutorial}', [AdminTutorialController::class, 'destroy']);

    // 优惠码 / 邀请机制
    Route::get('/discount-codes', [AdminDiscountCodeController::class, 'index']);
    Route::post('/discount-codes', [AdminDiscountCodeController::class, 'store']);
    Route::put('/discount-codes/{code}', [AdminDiscountCodeController::class, 'update']);
    Route::delete('/discount-codes/{code}', [AdminDiscountCodeController::class, 'destroy']);
    Route::get('/invite-settings', [AdminDiscountCodeController::class, 'inviteSettings']);
    Route::put('/invite-settings', [AdminDiscountCodeController::class, 'updateInviteSettings']);
});
