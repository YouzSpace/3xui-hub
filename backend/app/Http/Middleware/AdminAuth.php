<?php

namespace App\Http\Middleware;

use App\Models\Admin;
use App\Traits\ApiResponse;
use Closure;
use Illuminate\Http\Request;
use Symfony\Component\HttpFoundation\Response;

/**
 * Admin session 鉴权（M2.7）。
 * session('admin_id') → 加载 Admin → 注入 $request->user()。
 */
class AdminAuth
{
    use ApiResponse;

    public function handle(Request $request, Closure $next): Response
    {
        $adminId = $request->session()->get('admin_id');

        if (!$adminId) {
            return $this->error('未登录', 401);
        }

        /** @var Admin|null $admin */
        $admin = Admin::find($adminId);

        if (!$admin) {
            $request->session()->forget('admin_id');
            return $this->error('未登录', 401);
        }

        $request->setUserResolver(fn () => $admin);

        return $next($request);
    }
}
