<?php

use Illuminate\Foundation\Application;
use Illuminate\Foundation\Configuration\Exceptions;
use Illuminate\Foundation\Configuration\Middleware;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;
use Symfony\Component\HttpKernel\Exception\HttpException;
use Symfony\Component\HttpKernel\Exception\NotFoundHttpException;

return Application::configure(basePath: dirname(__DIR__))
    ->withRouting(
        web: __DIR__.'/../routes/web.php',
        api: __DIR__.'/../routes/api.php',
        commands: __DIR__.'/../routes/console.php',
        health: '/up',
    )
    ->withMiddleware(function (Middleware $middleware): void {
        $middleware->alias([
            'api.auth' => \App\Http\Middleware\ApiAuth::class,
            'admin.auth' => \App\Http\Middleware\AdminAuth::class,
        ]);

        // /admin-api/* 为 SPA 消费的 JSON 接口（session 鉴权 + 登录限流已提供保护），
        // 排除 CSRF 以便前端直接 POST；自托管面板可接受此折中。
        // 若后续需 CSRF 加固，改用前端取 XSRF-TOKEN cookie + X-XSRF-TOKEN header 流程。
        $middleware->validateCsrfTokens(except: ['/admin-api/*']);
    })
    ->withExceptions(function (Exceptions $exceptions): void {
        // api/* 与 admin-api/* 均为 JSON 接口，一律返回 {code, msg}，不返回 HTML
        $exceptions->shouldRenderJsonWhen(fn (Request $request) => $request->is('api/*') || $request->is('admin-api/*') || $request->expectsJson());

        $exceptions->render(function (Throwable $e, Request $request): ?JsonResponse {
            if (! ($request->is('api/*') || $request->is('admin-api/*'))) {
                return null;
            }

            $code = $e instanceof HttpException ? $e->getStatusCode() : 500;
            $msg = $e->getMessage() ?: '服务器错误';

            // 路由未命中
            if ($e instanceof NotFoundHttpException) {
                $msg = '接口不存在';
                $code = 404;
            }

            // 业务层抛出的 HttpException 带 message
            return response()->json([
                'code' => $code,
                'msg' => $msg,
                'data' => null,
            ], 200);
        });
    })->create();
