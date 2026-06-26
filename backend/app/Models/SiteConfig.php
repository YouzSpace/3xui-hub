<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;

/**
 * 站点配置（key-value）。
 */
class SiteConfig extends Model
{
    protected $fillable = ['key', 'value'];

    /**
     * 获取单个配置值。
     */
    public static function getValue(string $key, string $default = ''): string
    {
        $config = static::where('key', $key)->first();
        return $config ? ($config->value ?? $default) : $default;
    }

    /**
     * 设置单个配置值。
     */
    public static function setValue(string $key, ?string $value): void
    {
        static::updateOrCreate(['key' => $key], ['value' => $value]);
    }

    /**
     * 批量获取配置。
     */
    public static function getMany(array $keys): array
    {
        $configs = static::whereIn('key', $keys)->pluck('value', 'key')->all();
        $result = [];
        foreach ($keys as $key) {
            $result[$key] = $configs[$key] ?? '';
        }
        return $result;
    }

    /**
     * 批量设置配置。
     */
    public static function setMany(array $data): void
    {
        foreach ($data as $key => $value) {
            static::setValue($key, $value);
        }
    }
}
