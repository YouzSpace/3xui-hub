<?php

namespace App\Services;

use App\Models\SiteConfig;
use Illuminate\Support\Facades\RateLimiter;

/**
 * 用户端接口限流（发码 / 验证码试错 / 邮箱登录 / 注册 / 折扣码校验）。
 *
 * 规则可由管理员在后台自己调：值存在 SiteConfig 的同名 rate_* 键里，
 * **SiteConfig 有非空值就覆盖 config/panel.php 的 ratelimit 默认值**；任何一项 <= 0 表示该项不限。
 * 入口按业务归口分两处：发码/验证码/登录/注册在「邮箱配置 → 安全限制」页，
 * 折扣码校验（rate_discount_*）在「优惠码管理 → 邀请机制设置」卡片。
 *
 * 五个用户端接口共用本服务（别在控制器里各写一份）；键前缀统一 rate:，
 * 计数走 RateLimiter facade，与 Admin\AuthController 的管理员登录限流同构。
 *
 * 防枚举前提（改这里前先读 PasswordResetController 的类注释）：
 *  - 发码接口必须「先问 codeSendBlocked、再判邮箱是否注册」；
 *  - 且只要放行就必须 recordCodeSend —— 无论邮箱是否注册、SMTP 是否配置、信有没有真发出去。
 * 只要有一处分支漏记或早退，注册 / 未注册邮箱在限流状态上就会分叉，等于把注册状态漏出去。
 */
class RateGuardService
{
    /** 被限流时的统一文案：与 Admin\AuthController 的管理员登录限流逐字一致。 */
    public const TOO_OFTEN_MSG = '操作过于频繁，请稍后再试';

    /** 所有限流键的统一前缀，避免与业务缓存键（pwd_reset_ / email_verify_ 那些）撞车。 */
    private const PREFIX = 'rate:';

    /** 可被 SiteConfig 覆盖的键，同时也是 config/panel.php ratelimit 段的键名。 */
    private const CONFIG_KEYS = [
        'rate_code_interval',
        'rate_code_per_day',
        'rate_code_ip_hourly',
        'rate_verify_max_attempts',
        'rate_verify_lock_seconds',
        'rate_login_max_attempts',
        'rate_login_lock_seconds',
        'rate_register_ip_hourly',
        'rate_discount_per_minute',
        'rate_discount_fail_per_day',
        'rate_discount_ip_fail_hourly',
        'rate_discount_max_attempts',
        'rate_discount_lock_seconds',
    ];

    /** 滚动窗口长度（秒）。 */
    private const MINUTE = 60;
    private const HOUR   = 3600;
    private const DAY    = 86400;

    // ===== 发码：/api/password-reset/send 与 /api/email-verify/send =====

    /**
     * 发码前置检查（间隔 / 每日 / 同 IP 每小时三条规则）。
     * 返回 null = 放行；非 null = 提示文案，调用方原样返回 429 即可。
     */
    public function codeSendBlocked(string $email, string $ip): ?string
    {
        $limits = $this->limits();

        // 间隔：这个键最多容 1 次命中，命中即「距上次发码还不到 interval 秒」
        if ($limits['rate_code_interval'] > 0
            && RateLimiter::tooManyAttempts($this->codeIntervalKey($email), 1)) {
            return self::TOO_OFTEN_MSG;
        }

        if ($this->exceeded($this->codeDailyKey($email), $limits['rate_code_per_day'])) {
            return self::TOO_OFTEN_MSG;
        }

        if ($this->exceeded($this->codeIpKey($ip), $limits['rate_code_ip_hourly'])) {
            return self::TOO_OFTEN_MSG;
        }

        return null;
    }

    /**
     * 记一次发码请求。
     *
     * **只要 codeSendBlocked 放行就必须调用**，且要在「邮箱是否注册」判断之前调：
     * 未注册邮箱、SMTP 未配置、发信失败这些分支全都得记，否则限流状态本身就泄露注册状态。
     */
    public function recordCodeSend(string $email, string $ip): void
    {
        $limits = $this->limits();

        if ($limits['rate_code_interval'] > 0) {
            RateLimiter::hit($this->codeIntervalKey($email), $limits['rate_code_interval']);
        }

        if ($limits['rate_code_per_day'] > 0) {
            RateLimiter::hit($this->codeDailyKey($email), self::DAY);
        }

        if ($limits['rate_code_ip_hourly'] > 0) {
            RateLimiter::hit($this->codeIpKey($ip), self::HOUR);
        }
    }

    // ===== 验证码试错：/api/password-reset/reset 与 /api/email-verify/check =====

    /** 返回 null = 放行；非 null = 提示文案（试错次数已打满，锁定期内连正确验证码也拒）。 */
    public function verifyBlocked(string $email, string $ip): ?string
    {
        return $this->exceeded($this->verifyKey($email, $ip), $this->limits()['rate_verify_max_attempts'])
            ? self::TOO_OFTEN_MSG
            : null;
    }

    /** 记一次「验证码不对」。 */
    public function recordVerifyFailure(string $email, string $ip): void
    {
        $lock = $this->limits()['rate_verify_lock_seconds'];
        if ($lock > 0) {
            RateLimiter::hit($this->verifyKey($email, $ip), $lock);
        }
    }

    /** 验证通过后清零，别让之前几次手误拖累后面的正常操作。 */
    public function clearVerifyFailures(string $email, string $ip): void
    {
        RateLimiter::clear($this->verifyKey($email, $ip));
    }

    // ===== 邮箱密码登录：/api/login-email =====

    public function loginBlocked(string $email, string $ip): ?string
    {
        return $this->exceeded($this->loginKey($email, $ip), $this->limits()['rate_login_max_attempts'])
            ? self::TOO_OFTEN_MSG
            : null;
    }

    /** 记一次登录失败（含邮箱不存在 —— 对外文案本来就与密码错统一）。 */
    public function recordLoginFailure(string $email, string $ip): void
    {
        $lock = $this->limits()['rate_login_lock_seconds'];
        if ($lock > 0) {
            RateLimiter::hit($this->loginKey($email, $ip), $lock);
        }
    }

    /** 登录成功后清零。 */
    public function clearLoginFailures(string $email, string $ip): void
    {
        RateLimiter::clear($this->loginKey($email, $ip));
    }

    // ===== 注册：/api/register（按 IP，无邮箱维度）=====

    public function registerBlocked(string $ip): ?string
    {
        return $this->exceeded($this->registerKey($ip), $this->limits()['rate_register_ip_hourly'])
            ? self::TOO_OFTEN_MSG
            : null;
    }

    /** 记一次注册请求（入口处调，验证码错 / 邮箱已注册这些分支同样占额度）。 */
    public function recordRegister(string $ip): void
    {
        if ($this->limits()['rate_register_ip_hourly'] > 0) {
            RateLimiter::hit($this->registerKey($ip), self::HOUR);
        }
    }

    // ===== 折扣码校验：/api/discount/check =====

    /**
     * 折扣码校验前置检查（每分钟请求 / 连续失败锁定 / 每日失败 / 同 IP 每小时失败）。
     * 返回 null = 放行；非 null = 提示文案，调用方原样返回 429 即可。
     *
     * 锁定期内在这里就拦掉，**调用方不得再查库**：不查库就没有任何枚举反馈可给。
     */
    public function discountCheckBlocked(int $userId, string $ip): ?string
    {
        $limits = $this->limits();

        // 每分钟请求：不分成功失败，所有请求都计
        if ($this->exceeded($this->discountMinuteKey($userId), $limits['rate_discount_per_minute'])) {
            return self::TOO_OFTEN_MSG;
        }

        // 连续失败锁定：窗口就是锁定时长，打满 max_attempts 次后整段窗口内一律拒
        if ($this->exceeded($this->discountAttemptKey($userId), $limits['rate_discount_max_attempts'])) {
            return self::TOO_OFTEN_MSG;
        }

        if ($this->exceeded($this->discountFailKey($userId), $limits['rate_discount_fail_per_day'])) {
            return self::TOO_OFTEN_MSG;
        }

        if ($this->exceeded($this->discountIpFailKey($ip), $limits['rate_discount_ip_fail_hourly'])) {
            return self::TOO_OFTEN_MSG;
        }

        return null;
    }

    /** 记一次校验请求：这条规则口径是「所有请求都计」，成功失败都算。 */
    public function recordDiscountRequest(int $userId): void
    {
        if ($this->limits()['rate_discount_per_minute'] > 0) {
            RateLimiter::hit($this->discountMinuteKey($userId), self::MINUTE);
        }
    }

    /** 记一次校验失败：用户维度（连续失败 + 每日）与 IP 维度各记一笔。 */
    public function recordDiscountFailure(int $userId, string $ip): void
    {
        $limits = $this->limits();

        // 连续失败计数：TTL 取锁定时长，连着失败满 max_attempts 次即进入锁定
        if ($limits['rate_discount_max_attempts'] > 0 && $limits['rate_discount_lock_seconds'] > 0) {
            RateLimiter::hit($this->discountAttemptKey($userId), $limits['rate_discount_lock_seconds']);
        }

        if ($limits['rate_discount_fail_per_day'] > 0) {
            RateLimiter::hit($this->discountFailKey($userId), self::DAY);
        }

        if ($limits['rate_discount_ip_fail_hourly'] > 0) {
            RateLimiter::hit($this->discountIpFailKey($ip), self::HOUR);
        }
    }

    /**
     * 校验成功后清零失败计数（用户维度）。
     *
     * 刻意**不清** IP 维度：那是同 IP 上所有人共享的额度，一次成功就抹掉，
     * 等于给攻击者一把「成功校验一次即可重置全局限额」的钥匙。
     * 每分钟的请求计数同理，让它自然过期（否则成功一次也能洗掉自己的频率痕迹）。
     */
    public function clearDiscountFailures(int $userId): void
    {
        RateLimiter::clear($this->discountAttemptKey($userId));
        RateLimiter::clear($this->discountFailKey($userId));
    }

    // ===== 内部 =====

    /** 阈值 <= 0 一律视为不限。 */
    private function exceeded(string $key, int $limit): bool
    {
        return $limit > 0 && RateLimiter::tooManyAttempts($key, $limit);
    }

    /**
     * 取全部阈值：SiteConfig 有非空值就用它，否则回退 config/panel.php 的 ratelimit 默认值。
     * 存进来的值一律是字符串（SiteConfig 是 key-value 文本表），空串 = 没配过。
     *
     * 这里刻意**不缓存**：控制器实例会被路由复用（长驻进程下同一个实例可能服务很多请求），
     * 缓存住阈值就会让后台刚改的规则对已实例化的控制器不生效。
     */
    private function limits(): array
    {
        $overrides = SiteConfig::getMany(self::CONFIG_KEYS);

        $limits = [];
        foreach (self::CONFIG_KEYS as $key) {
            $value = $overrides[$key] ?? '';
            $limits[$key] = ($value === '' || $value === null)
                ? (int) config('panel.ratelimit.' . $key, 0)
                : (int) $value;
        }

        return $limits;
    }

    /** 邮箱统一小写去空白，避免同一邮箱换大小写绕开计数。 */
    private function normalize(string $value): string
    {
        return strtolower(trim($value));
    }

    private function codeIntervalKey(string $email): string
    {
        return self::PREFIX . 'code_interval:' . $this->normalize($email);
    }

    private function codeDailyKey(string $email): string
    {
        return self::PREFIX . 'code_daily:' . $this->normalize($email);
    }

    private function codeIpKey(string $ip): string
    {
        return self::PREFIX . 'code_ip:' . $ip;
    }

    private function verifyKey(string $email, string $ip): string
    {
        return self::PREFIX . 'verify:' . $this->normalize($email) . '|' . $ip;
    }

    private function loginKey(string $email, string $ip): string
    {
        return self::PREFIX . 'login:' . $this->normalize($email) . '|' . $ip;
    }

    private function registerKey(string $ip): string
    {
        return self::PREFIX . 'register:' . $ip;
    }

    private function discountMinuteKey(int $userId): string
    {
        return self::PREFIX . 'discount_min:' . $userId;
    }

    private function discountAttemptKey(int $userId): string
    {
        return self::PREFIX . 'discount_attempt:' . $userId;
    }

    private function discountFailKey(int $userId): string
    {
        return self::PREFIX . 'discount_fail:' . $userId;
    }

    private function discountIpFailKey(string $ip): string
    {
        return self::PREFIX . 'discount_ip_fail:' . $ip;
    }
}
