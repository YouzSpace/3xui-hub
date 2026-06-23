<?php

namespace Database\Factories;

use App\Models\User;
use Illuminate\Database\Eloquent\Factories\Factory;
use Illuminate\Support\Str;

/**
 * ControlHub 用户工厂（新 schema：token、uuid、protocol、traffic、expired_at、enabled）。
 *
 * @extends Factory<User>
 */
class UserFactory extends Factory
{
    public function definition(): array
    {
        return [
            'email' => fake()->unique()->safeEmail(),
            'token' => 'sub_' . Str::random(32),
            'uuid' => Str::uuid()->toString(),
            'protocol' => 'vless',
            'traffic_limit' => 0,
            'traffic_used' => 0,
            'expired_at' => null,
            'enabled' => true,
        ];
    }
}
