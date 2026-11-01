<template>
  <div class="tg-page">
    <div class="tg-head">
      <h1 class="tg-title">Telegram 机器人</h1>
      <span class="tg-autosave-hint">修改自动保存</span>
    </div>

    <Card class="tg-card">
      <template #header><h2>基础配置</h2></template>
      <div class="tg-grid">
        <div>
          <label class="tg-label">Bot Token</label>
          <Input v-model="form.tg_bot_token" type="password" placeholder="从 @BotFather 获取" />
        </div>
        <div>
          <label class="tg-label">Chat ID</label>
          <Input v-model="form.tg_bot_chat_id" placeholder="接收通知的 chat_id（数字）" />
        </div>
      </div>
      <p class="tg-help">
        <Badge variant="info">提示</Badge>
        在 Telegram 找 @BotFather 创建机器人拿 Token；给机器人发一条消息后用
        <code class="tg-code">getUpdates</code> 查你的 Chat ID（个人号是纯数字）。
      </p>
    </Card>

    <Card class="tg-card">
      <template #header><h2>通知开关</h2></template>
      <div class="tg-rows">
        <div class="tg-switch-row">
          <span class="tg-switch-label">启用 Telegram 通知</span>
          <label class="tg-switch">
            <input v-model="form.tg_enabled" type="checkbox" class="tg-switch-input" />
            <span class="tg-switch-slider"></span>
          </label>
        </div>
        <div class="tg-switch-row">
          <span class="tg-switch-label">节点离线告警</span>
          <label class="tg-switch">
            <input v-model="form.tg_notify_node_offline" type="checkbox" class="tg-switch-input" />
            <span class="tg-switch-slider"></span>
          </label>
        </div>
        <div class="tg-switch-row">
          <span class="tg-switch-label">支付到账通知</span>
          <label class="tg-switch">
            <input v-model="form.tg_notify_payment" type="checkbox" class="tg-switch-input" />
            <span class="tg-switch-slider"></span>
          </label>
        </div>
        <div class="tg-switch-row">
          <span class="tg-switch-label">每日备份推送</span>
          <label class="tg-switch">
            <input v-model="form.tg_backup_push" type="checkbox" class="tg-switch-input" />
            <span class="tg-switch-slider"></span>
          </label>
        </div>
      </div>
      <p class="tg-help">
        <Badge variant="info">提示</Badge>
        节点离线只在「本轮新掉线」时推一次，不会每 5 分钟重复刷屏。
      </p>
    </Card>

    <div class="tg-actions">
      <Button @click="onTest" :loading="testing" :success="sent">
        {{ sent ? '已发送' : '发送测试消息' }}
      </Button>
    </div>
  </div>
</template>

<script setup>
import { ref, watch, onMounted, onUnmounted } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Input from '@/components/ui/Input.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()
const testing = ref(false)
const sent = ref(false)
let sentTimer = null

const form = ref({
  tg_bot_token: '',
  tg_bot_chat_id: '',
  tg_enabled: false,
  tg_notify_node_offline: false,
  tg_notify_payment: false,
  tg_backup_push: false,
})

// 布尔字段列表：后端存 '1'/'0'，加载时归一到 boolean
const boolKeys = ['tg_enabled', 'tg_notify_node_offline', 'tg_notify_payment', 'tg_backup_push']

const load = async () => {
  try {
    const data = await adminApi.getTelegram()
    if (!data) return
    for (const k of boolKeys) {
      form.value[k] = data[k] === true || data[k] === '1' || data[k] === 1
    }
    form.value.tg_bot_token = data.tg_bot_token ?? ''
    form.value.tg_bot_chat_id = data.tg_bot_chat_id ?? ''
    savedSnap = JSON.stringify(form.value)
  } catch (e) {
    // 读失败静默：保存会整体提交，空值不覆盖后端已有配置
  }
}

// 自动保存：深度监听 + 800ms 防抖，快照比对避免加载回读触发保存
let savedSnap = ''
let timer = null
let lastToastAt = 0
watch(
  form,
  () => {
    sent.value = false
    clearTimeout(timer)
    timer = setTimeout(save, 800)
  },
  { deep: true },
)

const save = async () => {
  const snap = JSON.stringify(form.value)
  if (snap === savedSnap) return
  try {
    await adminApi.saveTelegram(form.value)
    savedSnap = snap
    const now = Date.now()
    if (now - lastToastAt > 1500) {
      toast.success('已自动保存')
      lastToastAt = now
    }
  } catch (e) {
    toast.error(e?.message || '保存失败')
  }
}

const onTest = async () => {
  if (!form.value.tg_bot_token) return toast.error('请先填写 Bot Token')
  if (!form.value.tg_bot_chat_id) return toast.error('请先填写 Chat ID')
  testing.value = true
  sent.value = false
  clearTimeout(sentTimer)
  try {
    await adminApi.saveTelegram(form.value)
    savedSnap = JSON.stringify(form.value)
    await adminApi.testTelegram()
    sent.value = true
    toast.success('测试消息已发送，请查看 Telegram')
    // 2.5 秒后按钮恢复可点击（success 态会 disabled，不恢复就没法再点）
    sentTimer = setTimeout(() => { sent.value = false }, 2500)
  } catch (e) {
    toast.error(e?.message || '发送失败')
  } finally {
    testing.value = false
  }
}

onMounted(load)
onUnmounted(() => clearTimeout(sentTimer))
</script>

<style scoped>
.tg-page { max-width: 1180px; }
.tg-head { display: flex; align-items: baseline; gap: var(--space-3); }
.tg-title { font-size: var(--text-xl); font-weight: var(--font-semibold); margin-bottom: var(--space-5); }
.tg-autosave-hint { font-size: var(--text-xs); color: var(--text-muted); }
.tg-card { margin-bottom: var(--space-4); }
.tg-grid { display: grid; grid-template-columns: 1fr 1fr; gap: var(--space-4); }
.tg-label { display: block; margin-bottom: var(--space-2); font-size: var(--text-xs); color: var(--text-muted); }
.tg-help { margin: var(--space-3) 0 0; font-size: var(--text-xs); color: var(--text-muted); display: flex; align-items: center; gap: var(--space-2); flex-wrap: wrap; }
.tg-code { font-family: monospace; background: var(--bg-hover); padding: 1px 6px; border-radius: var(--radius-sm); font-size: var(--text-xs); color: var(--text-secondary); }

/* 拨动开关（对齐 Email 页 email-switch） */
.tg-rows { display: flex; flex-direction: column; }
.tg-switch-row { display: flex; align-items: center; justify-content: space-between; padding: var(--space-3) 0; border-bottom: 1px solid var(--border-subtle); }
.tg-switch-row:last-child { border-bottom: none; }
.tg-switch-label { font-size: var(--text-sm); }
.tg-switch { position: relative; display: inline-block; width: 48px; height: 27px; flex-shrink: 0; }
.tg-switch-input { opacity: 0; width: 0; height: 0; position: absolute; }
.tg-switch-slider { position: absolute; cursor: pointer; inset: 0; background: var(--switch-off, #d1d5db); border-radius: 14px; transition: 0.3s; box-shadow: inset 0 1px 3px rgba(0,0,0,0.1); }
.tg-switch-slider::before { content: ''; position: absolute; height: 21px; width: 21px; left: 3px; bottom: 3px; background: var(--switch-knob, #fff); border-radius: 50%; transition: 0.3s; box-shadow: 0 2px 4px rgba(0,0,0,0.2); }
.tg-switch-input:checked + .tg-switch-slider { background: var(--switch-on, #2563eb); box-shadow: inset 0 0 0 0 rgba(0,0,0,0); }
.tg-switch-input:checked + .tg-switch-slider::before { transform: translateX(21px); }

.tg-actions { margin-top: var(--space-4); }
@media (max-width: 640px) { .tg-grid { grid-template-columns: 1fr; } }
</style>
