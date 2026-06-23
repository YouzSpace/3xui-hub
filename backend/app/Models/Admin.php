<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Attributes\Fillable;
use Illuminate\Database\Eloquent\Attributes\Hidden;
use Illuminate\Foundation\Auth\User as Authenticatable;

/**
 * ControlHub 管理员。
 * session 登录，密码 bcrypt，支持 Google 2FA。
 */
#[Fillable(['username', 'password', 'google2fa_secret', 'google2fa_enabled'])]
#[Hidden(['password', 'google2fa_secret'])]
class Admin extends Authenticatable
{
    protected $guard = 'admin';

    protected function casts(): array
    {
        return [
            'password' => 'hashed',
            'google2fa_enabled' => 'boolean',
        ];
    }
}
