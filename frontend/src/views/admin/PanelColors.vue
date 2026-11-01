<script setup>
/**
 * 配色 — 管理员自定义管理后台的颜色。
 *
 * 默认「整体」模式：一排预设一键套用 + 一个色环拖动调整体色调
 * （选一个色相，背景/文字/边框等整套自动生成）。六个颜色的逐项微调
 * 收在「高级」开关里，默认收起。
 *
 * 只作用于管理端：注入的变量挂在 body.admin-app 作用域（AdminLayout 挂类），
 * 用户端与公开首页天然隔离。修改自动保存（防抖 800ms），改坏可一键恢复默认。
 */
import { ref, computed, onMounted, onBeforeUnmount } from 'vue'
import { useToast } from '@/components/ui/Toast.vue'
import { adminApi } from '@/api/admin'
import { applyPanelColors, buildPalette, hexToHue, hslToHex } from '@/utils/panelColors'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'

const toast = useToast()

/** 可配置的 6 个变量；light/dark 为 tokens.css 出厂值（hsl 换算的 hex） */
const COLOR_ITEMS = [
  { var: '--accent', label: '主色', desc: '按钮、链接、选中态', light: '#18181b', dark: '#fafafa' },
  { var: '--bg-base', label: '页面背景', desc: '整个后台的底色', light: '#ffffff', dark: '#09090b' },
  { var: '--bg-surface', label: '卡片背景', desc: '卡片、表格、面板的底色', light: '#ffffff', dark: '#09090b' },
  { var: '--text-primary', label: '正文文字', desc: '标题与主要文字', light: '#09090b', dark: '#fafafa' },
  { var: '--text-secondary', label: '次要文字', desc: '说明、辅助信息', light: '#71717a', dark: '#a1a1aa' },
  { var: '--border-subtle', label: '边框', desc: '卡片、输入框、分隔线', light: '#e4e4e7', dark: '#27272a' },
]
const ACCENT_ITEM = COLOR_ITEMS[0]

/** 预设配色：hue=null 表示出厂中性黑白；其余色相走 buildPalette 生成整套 */
const PRESETS = [
  { id: 'default', name: '默认', hue: null },
  { id: 'blue', name: '蓝', hue: 221 },
  { id: 'cyan', name: '青', hue: 190 },
  { id: 'green', name: '绿', hue: 145 },
  { id: 'purple', name: '紫', hue: 262 },
  { id: 'pink', name: '粉', hue: 330 },
  { id: 'red', name: '红', hue: 355 },
  { id: 'orange', name: '橙', hue: 25 },
]

const HEX_RE = /^#[0-9a-fA-F]{6}$/

// 只存「已自定义」的变量；未存的显示出厂默认值
const light = ref({})
const dark = ref({})
const loading = ref(false)
const isDark = ref(false)
const advanced = ref(false)
const saveState = ref('') // '' | 'saving' | 'saved'

const current = computed(() => (isDark.value ? dark.value : light.value))

const valueOf = (c) => current.value[c.var] || (isDark.value ? c.dark : c.light)
const isCustom = (c) => Object.prototype.hasOwnProperty.call(current.value, c.var)

const applyNow = () => applyPanelColors({ light: light.value, dark: dark.value })

// ---- 保存（防抖；成功只亮小字提示，失败才弹错误）----
let saveTimer = null

const doSave = async () => {
  saveTimer = null
  saveState.value = 'saving'
  try {
    await adminApi.savePanelColors({ light: light.value, dark: dark.value })
    saveState.value = 'saved'
  } catch (e) {
    saveState.value = ''
    toast.error(e?.message || '保存失败')
  }
}

const scheduleSave = () => {
  clearTimeout(saveTimer)
  saveState.value = ''
  saveTimer = setTimeout(doSave, 800)
}

// ---- 整体：预设 + 色环（都是「选一个色相 → 整套生成」）----

/** 套用预设：默认=清空回出厂；其余=按色相生成亮暗两套 */
const applyPreset = (p) => {
  if (p.hue == null) {
    light.value = {}
    dark.value = {}
  } else {
    const built = buildPalette(p.hue)
    light.value = built.light
    dark.value = built.dark
  }
  applyNow()
  scheduleSave()
}

/** 色环拖动：按该色相生成整套并实时应用（松开手指/鼠标才保存） */
const applyHue = (deg) => {
  const built = buildPalette(deg)
  light.value = built.light
  dark.value = built.dark
  applyNow()
}

const sameColors = (a, b) => {
  const ka = Object.keys(a)
  const kb = Object.keys(b)
  if (ka.length !== kb.length) return false
  return ka.every((k) => a[k] === b[k])
}

/** 当前配色与哪个预设完全一致（一致才高亮；手动微调过就不选任何预设） */
const activePresetId = computed(() => {
  const isDefault = Object.keys(light.value).length === 0 && Object.keys(dark.value).length === 0
  if (isDefault) return 'default'
  for (const p of PRESETS) {
    if (p.hue == null) continue
    const built = buildPalette(p.hue)
    if (sameColors(light.value, built.light) && sameColors(dark.value, built.dark)) return p.id
  }
  return ''
})

/** 预设圆点色：默认=黑白对半，其余=该色相主色 */
const presetDot = (p) =>
  p.hue == null ? 'linear-gradient(135deg, #18181b 50%, #f4f4f5 50%)' : hslToHex(p.hue, 72, 42)

// ---- 色环（CSS conic-gradient 环 + 指针拖动）----
const ringEl = ref(null)
let dragging = false

const ringGradient = computed(() => {
  const stops = []
  for (let d = 0; d <= 360; d += 30) stops.push(`hsl(${d} 80% 55%)`)
  return `conic-gradient(from 0deg, ${stops.join(', ')})`
})

/** 当前主色对应的色相（灰/黑白没有色相 → 不画标记） */
const ringHue = computed(() => hexToHue(valueOf(ACCENT_ITEM)))

/** 标记位置：与 conic 起点（12 点）对齐，半径取环带中径 61px */
const ringMark = computed(() => {
  const h = ringHue.value
  if (h == null) return null
  const rad = (h * Math.PI) / 180
  return {
    left: `calc(50% + ${(Math.sin(rad) * 61).toFixed(1)}px)`,
    top: `calc(50% - ${(Math.cos(rad) * 61).toFixed(1)}px)`,
  }
})

const ringPointer = (e) => {
  const el = ringEl.value
  if (!el) return
  const rect = el.getBoundingClientRect()
  const dx = e.clientX - (rect.left + rect.width / 2)
  const dy = e.clientY - (rect.top + rect.height / 2)
  // 指针滑到圆心附近时角度会跳变，半径 < 20px 不采样
  if (Math.hypot(dx, dy) < 20) return
  // atan2(右, -下)：0° = 12 点、顺时针，与 conic-gradient 对齐
  const deg = (Math.atan2(dx, -dy) * 180) / Math.PI
  applyHue(Math.round(((deg % 360) + 360) % 360))
}

const onRingMove = (e) => {
  if (dragging) ringPointer(e)
}

const onRingUp = () => {
  if (!dragging) return
  dragging = false
  window.removeEventListener('pointermove', onRingMove)
  window.removeEventListener('pointerup', onRingUp)
  scheduleSave()
}

const onRingDown = (e) => {
  dragging = true
  e.preventDefault()
  ringPointer(e)
  window.addEventListener('pointermove', onRingMove)
  window.addEventListener('pointerup', onRingUp)
}

const stopRingDrag = () => {
  if (!dragging) return
  dragging = false
  window.removeEventListener('pointermove', onRingMove)
  window.removeEventListener('pointerup', onRingUp)
}

// ---- 高级：逐项微调 ----
const setColor = (c, hex) => {
  const target = isDark.value ? dark : light
  target.value = { ...target.value, [c.var]: hex }
  applyNow()
  scheduleSave()
}

/** 单项恢复默认：从当前模式的配置里删掉该变量 */
const restoreOne = (c) => {
  if (!isCustom(c)) return
  const target = isDark.value ? dark : light
  const next = { ...target.value }
  delete next[c.var]
  target.value = next
  applyNow()
  scheduleSave()
}

// ---- 原生色卡（点击色块拉起系统取色器，拖动实时预览）----
const pickerEl = ref(null)
const pickerFor = ref(null)

const openPicker = (c) => {
  pickerFor.value = c
  if (pickerEl.value) {
    pickerEl.value.value = valueOf(c)
    pickerEl.value.click()
  }
}

const onPick = (e) => {
  const v = e?.target?.value
  if (pickerFor.value && HEX_RE.test(v)) setColor(pickerFor.value, v.toLowerCase())
}

// ---- hex 输入框（手输/粘贴颜色码）----
const onHexInput = (c, e) => {
  const m = String(e.target.value || '')
    .trim()
    .match(/^#?([0-9a-fA-F]{6})$/)
  if (!m) {
    toast.error('颜色格式应形如 #3366ff')
    e.target.value = valueOf(c)
    return
  }
  setColor(c, '#' + m[1].toLowerCase())
}

// ---- 恢复默认（两套一起清）----
const onReset = async () => {
  if (!confirm('恢复默认配色？将清空亮色与暗色两套自定义颜色。')) return
  try {
    await adminApi.resetPanelColors()
    light.value = {}
    dark.value = {}
    applyNow()
    toast.success('已恢复默认配色')
  } catch (e) {
    toast.error(e?.message || '恢复失败')
  }
}

// ---- 加载 ----
const load = async () => {
  loading.value = true
  try {
    const data = await adminApi.getPanelColors()
    light.value = data && typeof data.light === 'object' && data.light ? { ...data.light } : {}
    dark.value = data && typeof data.dark === 'object' && data.dark ? { ...data.dark } : {}
    applyNow()
  } catch (e) {
    toast.error(e?.message || '加载失败')
  } finally {
    loading.value = false
  }
}

// ---- 主题跟随（html 上 dark 类的增删）----
const syncTheme = () => {
  isDark.value = document.documentElement.classList.contains('dark')
}

let themeObserver = null

onMounted(() => {
  syncTheme()
  themeObserver = new MutationObserver(syncTheme)
  themeObserver.observe(document.documentElement, { attributes: true, attributeFilter: ['class'] })
  load()
})

onBeforeUnmount(() => {
  themeObserver?.disconnect()
  stopRingDrag()
  // 防抖窗口内离开页面 → 立即保存，别丢改动
  if (saveTimer) {
    clearTimeout(saveTimer)
    doSave()
  }
})
</script>

<template>
  <div class="pc-page">
    <div class="pc-head">
      <h1 class="pc-title">配色</h1>
      <span class="pc-hint">只作用于管理后台 · 修改自动保存</span>
      <span v-if="saveState === 'saving'" class="pc-state">保存中…</span>
      <span v-else-if="saveState === 'saved'" class="pc-state pc-state--ok">已保存</span>
    </div>

    <Card>
      <template #header>
        <div class="pc-card-head">
          <span>管理后台配色</span>
          <span class="pc-mode">当前为{{ isDark ? '暗色' : '亮色' }}模式（整套同时对亮色/暗色生效）</span>
        </div>
      </template>

      <!-- 预设配色 -->
      <div class="pc-block">
        <div class="pc-block-title">预设配色</div>
        <div class="pc-presets">
          <button
            v-for="p in PRESETS"
            :key="p.id"
            class="pc-preset"
            :class="{ 'pc-preset--active': activePresetId === p.id }"
            :title="`应用「${p.name}」配色`"
            @click="applyPreset(p)"
          >
            <span class="pc-preset-dot" :style="{ background: presetDot(p) }"></span>
            <span class="pc-preset-name">{{ p.name }}</span>
          </button>
        </div>
      </div>

      <!-- 整体色调（色环） -->
      <div class="pc-block">
        <div class="pc-block-title">整体色调</div>
        <div class="pc-ring-wrap">
          <div
            ref="ringEl"
            class="pc-ring"
            :style="{ background: ringGradient }"
            @pointerdown="onRingDown"
          >
            <span class="pc-ring-hole"></span>
            <span
              v-if="ringMark"
              class="pc-ring-mark"
              :style="{ left: ringMark.left, top: ringMark.top, background: valueOf(ACCENT_ITEM) }"
            ></span>
            <span class="pc-ring-hex">{{ valueOf(ACCENT_ITEM) }}</span>
          </div>
          <p class="pc-ring-note">
            拖动色环选一个色调，背景、文字、边框等整套配色自动跟着生成；<br />
            想逐个颜色微调，打开下面的「高级」。
          </p>
        </div>
      </div>

      <!-- 高级开关 -->
      <div class="pc-adv-row">
        <label class="pc-adv">
          <input v-model="advanced" type="checkbox" class="pc-adv-input" />
          <span class="pc-adv-track"><span class="pc-adv-knob"></span></span>
          <span class="pc-adv-label">高级（逐个颜色微调）</span>
        </label>
      </div>

      <!-- 逐项微调（默认收起） -->
      <div v-show="advanced" class="pc-list">
        <div v-for="c in COLOR_ITEMS" :key="c.var" class="pc-row">
          <div class="pc-meta">
            <span class="pc-name">{{ c.label }}</span>
            <span class="pc-desc">{{ c.desc }}</span>
          </div>
          <button
            class="pc-swatch"
            :style="{ background: valueOf(c) }"
            title="点击选择颜色"
            @click="openPicker(c)"
          ></button>
          <input
            class="pc-hex"
            :value="valueOf(c)"
            spellcheck="false"
            @change="onHexInput(c, $event)"
            @keyup.enter="onHexInput(c, $event)"
          />
          <button
            class="pc-badge"
            :class="{ 'pc-badge--custom': isCustom(c) }"
            :disabled="!isCustom(c)"
            :title="isCustom(c) ? '点击恢复默认色' : '当前为默认色'"
            @click="restoreOne(c)"
          >
            {{ isCustom(c) ? '已自定义' : '默认' }}
          </button>
        </div>
      </div>

      <div class="pc-actions">
        <Button variant="secondary" :loading="loading" @click="onReset">恢复默认配色</Button>
      </div>
    </Card>

    <input ref="pickerEl" type="color" class="pc-native" tabindex="-1" aria-hidden="true" @input="onPick" />
  </div>
</template>

<style scoped>
.pc-page {
  max-width: 760px;
}
.pc-head {
  display: flex;
  align-items: baseline;
  gap: var(--space-3);
}
.pc-title {
  font-size: var(--text-xl);
  font-weight: var(--font-semibold);
  margin-bottom: var(--space-5);
}
.pc-hint {
  font-size: var(--text-xs);
  color: var(--text-muted);
}
.pc-state {
  font-size: var(--text-xs);
  color: var(--text-muted);
  margin-left: auto;
}
.pc-state--ok {
  color: var(--success);
}

.pc-card-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: var(--space-3);
}
.pc-mode {
  font-size: var(--text-xs);
  font-weight: normal;
  color: var(--text-muted);
}

/* ===== 区块 ===== */
.pc-block {
  padding: var(--space-4) 0;
  border-bottom: 1px solid var(--border-subtle);
}
.pc-block:first-of-type {
  padding-top: var(--space-2);
}
.pc-block-title {
  font-size: var(--text-sm);
  font-weight: var(--font-medium);
  color: var(--text-primary);
  margin-bottom: var(--space-3);
}

/* ===== 预设 ===== */
.pc-presets {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-3);
}
.pc-preset {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 6px;
  padding: 8px 10px;
  border: 1px solid transparent;
  border-radius: var(--radius-md);
  background: none;
  cursor: pointer;
  transition:
    border-color 0.15s ease,
    background 0.15s ease;
}
.pc-preset:hover {
  background: var(--bg-hover);
}
.pc-preset--active {
  border-color: var(--accent);
}
.pc-preset-dot {
  width: 26px;
  height: 26px;
  border-radius: 50%;
  border: 1px solid var(--border-subtle);
}
.pc-preset-name {
  font-size: var(--text-xs);
  color: var(--text-secondary);
}

/* ===== 色环 ===== */
.pc-ring-wrap {
  display: flex;
  align-items: center;
  gap: var(--space-6);
}
.pc-ring {
  position: relative;
  width: 148px;
  height: 148px;
  flex: none;
  border-radius: 50%;
  cursor: crosshair;
  touch-action: none;
}
.pc-ring-hole {
  position: absolute;
  inset: 26px;
  border-radius: 50%;
  background: var(--bg-surface);
  box-shadow: inset 0 0 0 1px var(--border-subtle);
}
.pc-ring-hex {
  position: absolute;
  inset: 26px;
  display: flex;
  align-items: center;
  justify-content: center;
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  color: var(--text-secondary);
  pointer-events: none;
}
.pc-ring-mark {
  position: absolute;
  width: 18px;
  height: 18px;
  border-radius: 50%;
  border: 3px solid #fff;
  box-shadow: 0 1px 4px rgba(0, 0, 0, 0.4);
  transform: translate(-50%, -50%);
  pointer-events: none;
}
.pc-ring-note {
  font-size: var(--text-xs);
  color: var(--text-muted);
  line-height: 1.7;
}

/* ===== 高级开关 ===== */
.pc-adv-row {
  padding: var(--space-4) 0 0;
}
.pc-adv {
  display: inline-flex;
  align-items: center;
  gap: var(--space-2);
  cursor: pointer;
  user-select: none;
}
.pc-adv-input {
  position: absolute;
  opacity: 0;
  pointer-events: none;
}
.pc-adv-track {
  position: relative;
  width: 36px;
  height: 20px;
  flex: none;
  border-radius: 999px;
  background: var(--switch-off);
  transition: background 0.2s ease;
}
.pc-adv-input:checked + .pc-adv-track {
  background: var(--switch-on);
}
.pc-adv-knob {
  position: absolute;
  top: 2px;
  left: 2px;
  width: 16px;
  height: 16px;
  border-radius: 50%;
  background: var(--switch-knob);
  transition: transform 0.2s ease;
}
.pc-adv-input:checked + .pc-adv-track .pc-adv-knob {
  transform: translateX(16px);
}
.pc-adv-label {
  font-size: var(--text-sm);
  color: var(--text-primary);
}

/* ===== 逐项微调 ===== */
.pc-list {
  display: flex;
  flex-direction: column;
  margin-top: var(--space-2);
}
.pc-row {
  display: flex;
  align-items: center;
  gap: var(--space-4);
  padding: var(--space-3) 0;
  border-bottom: 1px solid var(--border-subtle);
}
.pc-row:last-child {
  border-bottom: none;
}
.pc-meta {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.pc-name {
  font-size: var(--text-sm);
  font-weight: var(--font-medium);
  color: var(--text-primary);
}
.pc-desc {
  font-size: var(--text-xs);
  color: var(--text-muted);
}

.pc-swatch {
  width: 36px;
  height: 36px;
  flex: none;
  border: 1px solid var(--border-strong);
  border-radius: var(--radius-md);
  cursor: pointer;
  transition: box-shadow 0.15s ease;
}
.pc-swatch:hover {
  box-shadow:
    0 0 0 2px var(--bg-base),
    0 0 0 4px var(--accent);
}

.pc-hex {
  width: 104px;
  flex: none;
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  padding: 7px 9px;
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
  background: var(--bg-input);
  color: var(--text-primary);
}
.pc-hex:focus {
  outline: none;
  border-color: var(--border-focus);
}

.pc-badge {
  flex: none;
  width: 64px;
  padding: 0;
  border: none;
  background: none;
  font-size: var(--text-xs);
  color: var(--text-muted);
  cursor: default;
  text-align: right;
}
.pc-badge--custom {
  color: var(--accent);
  cursor: pointer;
}
.pc-badge--custom:hover {
  text-decoration: underline;
}

.pc-actions {
  display: flex;
  justify-content: flex-end;
  margin-top: var(--space-4);
}

/* 不 display:none —— 藏在视口外，保证 .click() 能拉起系统色卡 */
.pc-native {
  position: absolute;
  left: -9999px;
  width: 1px;
  height: 1px;
}
</style>
