<?php

use App\Http\Controllers\Api\AuthController;
use App\Http\Controllers\Api\NodeController as ApiNodeController;
use App\Http\Controllers\Api\PaymentController as ApiPaymentController;
use App\Http\Controllers\Api\PlanController as ApiPlanController;
use App\Http\Controllers\Api\ProtocolController;
use App\Http\Controllers\Api\SubscriptionController;
use App\Http\Controllers\Api\UserController as ApiUserController;
use Illuminate\Support\Facades\Route;

/*
|--------------------------------------------------------------------------
| 用户端 API 路由（前缀 /api）
|--------------------------------------------------------------------------
*/

Route::get('/ping', function (): \Illuminate\Http\JsonResponse {
    return response()->json(['code' => 0, 'msg' => 'ok', 'data' => ['pong' => true]]);
});

// 公开：站点配置（无需鉴权）
Route::get('/site-config', function (): \Illuminate\Http\JsonResponse {
    $keys = ['site_title', 'site_subtitle', 'site_description', 'site_keywords', 'site_logo', 'site_favicon', 'register_email_verify'];
    $data = \App\Models\SiteConfig::getMany($keys);
    return response()->json(['code' => 0, 'msg' => 'ok', 'data' => $data]);
});

// 认证（Token 登录，无状态）
Route::post('/login', [AuthController::class, 'login']);

// 需要 session 的接口（验证码、注册、邮箱登录）
Route::middleware('web')->group(function () {
    Route::get('/captcha', [AuthController::class, 'captcha']);
    Route::post('/register', [AuthController::class, 'register']);
    Route::post('/login-email', [AuthController::class, 'loginEmail']);
    Route::post('/email-verify/send', [\App\Http\Controllers\Api\EmailVerifyController::class, 'sendCode']);
    Route::post('/email-verify/check', [\App\Http\Controllers\Api\EmailVerifyController::class, 'verifyCode']);
});

// 订阅：仅凭 token，无需 Bearer
Route::get('/sub/{token}', [SubscriptionController::class, 'show']);

// 公开接口
Route::get('/plans', [ApiPlanController::class, 'index']);
Route::get('/payment/methods', [ApiPaymentController::class, 'methods']);

// 支付回调（无需鉴权，由支付网关调用）
Route::post('/payment/notify', [ApiPaymentController::class, 'notify']);

// 受 api.auth 保护的端点
Route::middleware('api.auth')->group(function () {
    Route::get('/me', [ApiUserController::class, 'me']);
    Route::post('/sync-traffic', [ApiUserController::class, 'syncTraffic']);
    Route::get('/nodes', [ApiNodeController::class, 'index']);
    Route::post('/protocol', [ProtocolController::class, 'switch']);

    // 支付相关
    Route::post('/payment/create', [ApiPaymentController::class, 'create']);
    Route::post('/payment/create-reset', [ApiPaymentController::class, 'createReset']);
    Route::get('/payment/status', [ApiPaymentController::class, 'status']);
    Route::get('/payment/orders', [ApiPaymentController::class, 'orders']);
    Route::post('/payment/retry', [ApiPaymentController::class, 'retry']);

    Route::get('/_auth_check', function (\Illuminate\Http\Request $request): \Illuminate\Http\JsonResponse {
        return response()->json([
            'code' => 0, 'msg' => 'ok',
            'data' => ['user_id' => $request->user()?->id],
        ]);
    });
});
