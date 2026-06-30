<?php

use App\Http\Controllers\Admin\AuthController as AdminAuthController;
use App\Http\Controllers\Admin\BackupController as AdminBackupController;
use App\Http\Controllers\Admin\EmailController as AdminEmailController;
use App\Http\Controllers\Admin\NodeController as AdminNodeController;
use App\Http\Controllers\Admin\OrderController as AdminOrderController;
use App\Http\Controllers\Admin\PaymentController as AdminPaymentController;
use App\Http\Controllers\Admin\PlanController as AdminPlanController;
use App\Http\Controllers\Admin\SyncTrafficController as AdminSyncTrafficController;
use App\Http\Controllers\Admin\UserController as AdminUserController;
use App\Http\Controllers\Admin\SiteSettingController as AdminSiteSettingController;
use App\Http\Controllers\Admin\TutorialController as AdminTutorialController;
use Illuminate\Support\Facades\Route;

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
    Route::post('/users/{user}/protocol', [AdminUserController::class, 'switchProtocol']);
    Route::post('/users/{user}/renew', [AdminUserController::class, 'renew']);

    // M4 节点管理
    Route::get('/nodes', [AdminNodeController::class, 'index']);
    Route::post('/nodes', [AdminNodeController::class, 'store']);
    Route::get('/nodes/{node}', [AdminNodeController::class, 'show']);
    Route::put('/nodes/{node}', [AdminNodeController::class, 'update']);
    Route::delete('/nodes/{node}', [AdminNodeController::class, 'destroy']);
    Route::post('/nodes/{node}/test', [AdminNodeController::class, 'test']);
    Route::post('/nodes/probe-inbounds', [AdminNodeController::class, 'probeInbounds']);

    // 套餐管理
    Route::get('/plans', [AdminPlanController::class, 'index']);
    Route::post('/plans', [AdminPlanController::class, 'store']);
    Route::get('/plans/{plan}', [AdminPlanController::class, 'show']);
    Route::put('/plans/{plan}', [AdminPlanController::class, 'update']);
    Route::delete('/plans/{plan}', [AdminPlanController::class, 'destroy']);

    // 订单管理
    Route::get('/orders', [AdminOrderController::class, 'index']);

    // 流量同步
    Route::post('/sync-traffic', [AdminSyncTrafficController::class, 'sync']);

    // 备份管理
    Route::get('/backup/export', [AdminBackupController::class, 'export']);
    Route::post('/backup/preview', [AdminBackupController::class, 'preview']);
    Route::post('/backup/import', [AdminBackupController::class, 'import']);

    // 邮箱配置
    Route::get('/email', [AdminEmailController::class, 'show']);
    Route::put('/email', [AdminEmailController::class, 'save']);
    Route::post('/email/test', [AdminEmailController::class, 'test']);

    // 支付配置管理
    Route::get('/payments', [AdminPaymentController::class, 'index']);
    Route::post('/payments', [AdminPaymentController::class, 'store']);
    Route::get('/payments/{payment}', [AdminPaymentController::class, 'show']);
    Route::put('/payments/{payment}', [AdminPaymentController::class, 'update']);
    Route::delete('/payments/{payment}', [AdminPaymentController::class, 'destroy']);

    // 站点信息配置
    Route::get('/site-settings', [AdminSiteSettingController::class, 'index']);
    Route::put('/site-settings', [AdminSiteSettingController::class, 'update']);

    // 教程管理
    Route::get('/tutorials', [AdminTutorialController::class, 'index']);
    Route::post('/tutorials', [AdminTutorialController::class, 'store']);
    Route::put('/tutorials/{tutorial}', [AdminTutorialController::class, 'update']);
    Route::delete('/tutorials/{tutorial}', [AdminTutorialController::class, 'destroy']);
});
