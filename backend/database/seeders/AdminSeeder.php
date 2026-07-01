<?php

namespace Database\Seeders;

use App\Models\Admin;
use Illuminate\Database\Seeder;

/**
 * 默认管理员：admin / admin123。
 */
class AdminSeeder extends Seeder
{
    public function run(): void
    {
        if (Admin::exists()) {
            return;
        }

        Admin::create([
            'username' => 'admin',
            'password' => 'admin123', // casts hashed → bcrypt
        ]);
    }
}
