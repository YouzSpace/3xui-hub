<?php

namespace App\Drivers\Xray;

/**
 * Reality / X25519 密钥工具。
 *
 * 生成格式与 `xray x25519` 输出完全兼容（base64url 无填充）：
 * 用 sodium_compat（无 sodium 扩展时纯 PHP 兜底、有扩展自动加速）生成
 * X25519 keypair；私钥按 xray 惯例 clamp 后存储。
 *
 * 兼容性已与 xray v26.3.27 对拍验证：PHP 生成的私钥经 `xray x25519 -i`
 * 推出的公钥与 PHP 侧计算完全一致。
 */
class RealityKeyService
{
    /**
     * 生成 X25519 密钥对。
     *
     * @return array{0:string,1:string} [privateKey, publicKey]，均为 base64url 无填充
     */
    public static function generateKeypair(): array
    {
        $keypair = \ParagonIE_Sodium_Compat::crypto_box_keypair();
        $sk = \ParagonIE_Sodium_Compat::crypto_box_secretkey($keypair);
        $pk = \ParagonIE_Sodium_Compat::crypto_box_publickey($keypair);

        // 对齐 xray x25519 输出形态：私钥 clamp（X25519 标量钳位，幂等；公钥配对不变）
        $sk[0] = chr(ord($sk[0]) & 248);
        $sk[31] = chr((ord($sk[31]) & 127) | 64);

        return [self::base64url($sk), self::base64url($pk)];
    }

    /** 随机 shortId（hex 串，xray reality shortIds 格式）。 */
    public static function shortId(int $bytes = 4): string
    {
        return bin2hex(random_bytes($bytes));
    }

    /** base64url 无填充（xray 全系字符串格式）。 */
    public static function base64url(string $bin): string
    {
        return rtrim(strtr(base64_encode($bin), '+/', '-_'), '=');
    }
}
