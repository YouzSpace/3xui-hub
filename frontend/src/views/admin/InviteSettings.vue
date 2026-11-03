<script setup>
/**
 * Admin · 邀请机制。
 *
 * 从「优惠码」页拆出来：优惠码是发码/核销，邀请机制是拉新规则（折扣、刷新周期、开关），
 * 两件事生命周期不同 —— 之前挤在一页最上面，改邀请规则要滚过一堆发码表单。
 *
 * 同一个接口还带「优惠码校验限流」参数（防穷举，见 App\Services\RateGuardService），
 * 这些参数本质是营销风控，跟着邀请规则一起放这里，不留在发码页。
 */
import { ref, onMounted } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Input from '@/components/ui/Input.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()

const form = ref({
  invite_discount: '0.90',
  invite_refresh_days: '30',
  invite_enabled: '1',
  redeem_discount: '0.90',
  affiliate_rebate_rate: '0.10',
  rate_discount_per_minute: 10,
  rate_discount_fail_per_day: 50,
  rate_discount_ip_fail_hourly: 100,
  rate_discount_max_attempts: 10,
  rate_discount_lock_seconds: 900,
})
const loading = ref(false)
const saving = ref(false)
const saved = ref(false)

// 限流输入框：空串 = 没填 → 传 null，后端回落 config/panel.php 的默认值
const rateValue = (v) =>
  v === '' || v === null || v === undefined || Number.isNaN(Number(v)) ? null : Number(v)

const load = async () => {
  loading.value = true
  try {
    form.value = await adminApi.getInviteSettings()
  } catch (e) {
    toast.error(e.message || '加载失败')
  } finally {
    loading.value = false
  }
}

const save = async () => {
  const f = form.value
  if (!(Number(f.invite_discount) > 0 && Number(f.invite_discount) <= 1)) {
    toast.error('邀请码折扣需在 0.01 ~ 1 之间')
    return
  }
  if (!(Number(f.redeem_discount) > 0 && Number(f.redeem_discount) <= 1)) {
    toast.error('兑换码折扣需在 0.01 ~ 1 之间')
    return
  }
  if (Number(f.affiliate_rebate_rate) < 0 || Number(f.affiliate_rebate_rate) > 1) {
    toast.error('返佣比例需在 0 ~ 1 之间（0 = 暂停返佣）')
    return
  }

  saving.value = true
  try {
    await adminApi.updateInviteSettings({
      invite_discount: Number(f.invite_discount),
      invite_refresh_days: Number(f.invite_refresh_days) || 30,
      invite_enabled: f.invite_enabled === '1' || f.invite_enabled === true,
      redeem_discount: Number(f.redeem_discount),
      affiliate_rebate_rate: Number(f.affiliate_rebate_rate),
      rate_discount_per_minute: rateValue(f.rate_discount_per_minute),
      rate_discount_fail_per_day: rateValue(f.rate_discount_fail_per_day),
      rate_discount_ip_fail_hourly: rateValue(f.rate_discount_ip_fail_hourly),
      rate_discount_max_attempts: rateValue(f.rate_discount_max_attempts),
      rate_discount_lock_seconds: rateValue(f.rate_discount_lock_seconds),
    })
    toast.success('已保存')
    // 成功反馈：按钮短暂变对勾（Submit Morph 成功段）
    saved.value = true
    setTimeout(() => { saved.value = false }, 900)
    await load()
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="iv">
    <div class="iv-head">
      <div>
        <h1 class="iv-title">邀请机制</h1>
        <p class="iv-sub">用户邀请好友后双方拿到的折扣规则，以及优惠码校验的防穷举限流。</p>
      </div>
      <Button variant="primary" size="sm" :loading="saving" :success="saved" @click="save">保存设置</Button>
    </div>

    <Card class="iv-card">
      <h2 class="iv-section">邀请与兑换折扣</h2>
      <div class="iv-grid">
        <label class="iv-field">
          <span class="iv-label">邀请码折扣</span>
          <Input v-model.number="form.invite_discount" type="number" step="0.01" min="0.01" max="1" />
          <span class="iv-tip">0.90 = 9 折，填 1 = 不打折</span>
        </label>
        <label class="iv-field">
          <span class="iv-label">兑换码折扣</span>
          <Input v-model.number="form.redeem_discount" type="number" step="0.01" min="0.01" max="1" />
          <span class="iv-tip">0.90 = 9 折</span>
        </label>
        <label class="iv-field">
          <span class="iv-label">邀请返佣比例</span>
          <Input v-model.number="form.affiliate_rebate_rate" type="number" step="0.01" min="0" max="1" />
          <span class="iv-tip">好友消费后返给邀请人的比例，0.10 = 10%，填 0 = 暂停返佣</span>
        </label>
        <label class="iv-field">
          <span class="iv-label">邀请码刷新周期（天）</span>
          <Input v-model.number="form.invite_refresh_days" type="number" min="1" />
          <span class="iv-tip">每个用户隔多少天刷新一次邀请码</span>
        </label>
        <div class="iv-field">
          <span class="iv-label">邀请机制开关</span>
          <label class="iv-switch">
            <input
              type="checkbox"
              :checked="form.invite_enabled === '1' || form.invite_enabled === true"
              @change="form.invite_enabled = $event.target.checked ? '1' : '0'"
            />
            <span>开启后用户端显示邀请入口，邀请码可被使用</span>
          </label>
        </div>
      </div>
    </Card>

    <Card class="iv-card">
      <h2 class="iv-section">优惠码校验限流</h2>
      <p class="iv-tip iv-tip-block">
        防脚本枚举：同一用户 / 同一 IP 反复试错码会被限流锁定。值为 0 表示该项不限，留空回落到系统默认值。
      </p>
      <div class="iv-grid">
        <label class="iv-field">
          <span class="iv-label">每分钟上限</span>
          <Input v-model.number="form.rate_discount_per_minute" type="number" min="0" placeholder="10" />
          <span class="iv-tip">同一用户每分钟最多校验几次（成功失败都算）</span>
        </label>
        <label class="iv-field">
          <span class="iv-label">每日失败上限</span>
          <Input v-model.number="form.rate_discount_fail_per_day" type="number" min="0" placeholder="50" />
          <span class="iv-tip">同一用户 24 小时内最多错几次（成功即清零）</span>
        </label>
        <label class="iv-field">
          <span class="iv-label">同 IP 每小时失败上限</span>
          <Input v-model.number="form.rate_discount_ip_fail_hourly" type="number" min="0" placeholder="100" />
          <span class="iv-tip">同一 IP 每小时最多错几次（防换号绕过）</span>
        </label>
        <label class="iv-field">
          <span class="iv-label">连续失败锁定阈值</span>
          <Input v-model.number="form.rate_discount_max_attempts" type="number" min="0" placeholder="10" />
          <span class="iv-tip">同一用户连续错几次后被锁定</span>
        </label>
        <label class="iv-field">
          <span class="iv-label">锁定时长（秒）</span>
          <Input v-model.number="form.rate_discount_lock_seconds" type="number" min="0" placeholder="900" />
          <span class="iv-tip">锁定期内不再查库，直接返回限流提示</span>
        </label>
      </div>
    </Card>
  </div>
</template>

<style scoped>
.iv { max-width: 900px; }
.iv-head {
  display: flex; align-items: flex-start; justify-content: space-between;
  gap: var(--space-3); margin-bottom: var(--space-4);
}
.iv-title { margin: 0 0 4px; font-size: var(--text-xl); font-weight: var(--font-bold); }
.iv-sub { margin: 0; color: var(--text-muted); font-size: var(--text-sm); }
.iv-card { margin-bottom: var(--space-4); }
.iv-section { margin: 0 0 var(--space-3); font-size: var(--text-base); font-weight: var(--font-semibold); }
.iv-grid { display: grid; grid-template-columns: 1fr 1fr; gap: var(--space-3) var(--space-4); }
.iv-field { display: flex; flex-direction: column; gap: 6px; }
.iv-label { font-size: var(--text-sm); }
.iv-tip { color: var(--text-muted); font-size: var(--text-xs); line-height: 1.5; }
.iv-tip-block { margin: 0 0 var(--space-3); }
.iv-switch { display: flex; align-items: center; gap: 8px; font-size: var(--text-xs); color: var(--text-muted); }

/* 标签统一两行高度：长说明换行时不把同行输入框挤下去 */
.iv-grid > .iv-field > .iv-label { display: block; min-height: 2.6em; line-height: 1.35; }
</style>
