<script setup>
/**
 * Select.vue — 自定义下拉选择器。
 *
 * 为什么不用原生 <select>：展开后的选项面板由浏览器绘制，CSS 只能改颜色，
 * 圆角/间距/阴影都控制不了，深色模式下还会出现白底。本组件用 button + 浮层
 * 自己渲染，面板完全可控。
 *
 * 用法（v-model 与原生一致）：
 *   <Select v-model="form.type" :options="plans" />
 *   <Select v-model="x" :options="opts" placeholder="请选择" />
 *   <Select v-model="x" :options="opts" size="sm" />
 *   <Select v-model="x" :options="opts" @change="fn" />
 *
 * options 元素支持 { label, value, disabled }，也接受裸字符串/数字。
 */
import { ref, computed, onBeforeUnmount, watch, nextTick } from 'vue'
import { Teleport } from 'vue'
import Icon from './Icon.vue'

const props = defineProps({
  modelValue: { type: [String, Number, null], default: null },
  options: { type: Array, default: () => [] },
  placeholder: { type: String, default: '请选择' },
  size: { type: String, default: 'md', validator: (v) => ['sm', 'md'].includes(v) },
  disabled: { type: Boolean, default: false },
  // 触发器额外类名（对齐原页面布局）。不能用 class 作 prop 名：JS 保留字，
  // 模板里 :class="[... class ...]" 会直接语法报错。
  wrapperClass: { type: String, default: '' },
})

const emit = defineEmits(['update:modelValue', 'change'])

const open = ref(false)
const trigger = ref(null)
const panel = ref(null)
const activeIdx = ref(-1)
// 面板用 fixed 定位 + Teleport 到 body：Modal 的 .ch-modal-body 是
// overflow-y:auto，绝对定位的浮层会被它裁掉（选项只露一半）。
const panelStyle = ref({})

const normalized = computed(() =>
  props.options.map((o) => {
    if (o !== null && typeof o === 'object') {
      return { label: o.label ?? String(o.value ?? ''), value: o.value, disabled: !!o.disabled }
    }
    return { label: String(o), value: o, disabled: false }
  }),
)

const selected = computed(() => normalized.value.find((o) => o.value === props.modelValue) || null)
const displayText = computed(() => selected.value?.label ?? props.placeholder)

const toggle = () => {
  if (props.disabled) return
  open.value ? close() : show()
}

const show = async () => {
  open.value = true
  const i = normalized.value.findIndex((o) => o.value === props.modelValue)
  activeIdx.value = i
  await nextTick()
  measure()
  document.addEventListener('mousedown', onOutside, true)
  document.addEventListener('keydown', onKeydown, true)
  window.addEventListener('resize', reposition)
  // 面板是 fixed 定位，页面滚动后坐标会跟触发器脱节 —— 重新测量而不是直接关闭
  window.addEventListener('scroll', reposition, true)
}

const close = () => {
  if (!open.value) return
  open.value = false
  document.removeEventListener('mousedown', onOutside, true)
  document.removeEventListener('keydown', onKeydown, true)
  window.removeEventListener('resize', reposition)
  window.removeEventListener('scroll', reposition, true)
}

const reposition = () => { if (open.value) measure() }

/**
 * 面板用 fixed 坐标，并贴合触发器宽度。
 * 向上还是向下：优先下方空间够就往下，不够再看上方，都不够则选空间大的一边。
 */
const measure = () => {
  const el = trigger.value
  if (!el) return
  const r = el.getBoundingClientRect()
  const margin = 8
  const below = window.innerHeight - r.bottom - margin
  const above = r.top - margin
  const maxH = Math.min(280, Math.max(normalized.value.length * 34 + 8, 120))
  const gap = 4

  let top, maxHeight
  if (below >= Math.min(maxH, needed()) || below >= above) {
    top = r.bottom + gap
    maxHeight = Math.max(Math.min(below, maxH), 80)
  } else {
    top = Math.max(margin, r.top - gap - Math.min(above, maxH))
    maxHeight = Math.max(Math.min(above, maxH), 80)
  }

  const left = Math.max(margin, Math.min(r.left, window.innerWidth - r.width - margin))
  panelStyle.value = {
    top: `${Math.round(top)}px`,
    left: `${Math.round(left)}px`,
    width: `${Math.round(r.width)}px`,
    maxHeight: `${Math.round(maxHeight)}px`,
  }
}

const needed = () => Math.min(normalized.value.length * 34 + 8, 280)

const onOutside = (e) => {
  if (trigger.value?.contains(e.target) || panel.value?.contains(e.target)) return
  close()
}

const choose = (opt) => {
  if (opt.disabled) return
  emit('update:modelValue', opt.value)
  emit('change', opt.value)
  close()
}

/** 键盘操作：↑↓ 移动、Enter 选中、Esc 关闭 */
const onKeydown = (e) => {
  if (e.key === 'Escape') { close(); trigger.value?.focus(); return }
  if (e.key === 'Tab') { close(); return }
  if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp' && e.key !== 'Enter' && e.key !== ' ') return

  if (!open.value) {
    if (e.key !== 'Enter' && e.key !== ' ') return
    e.preventDefault()
    show()
    return
  }
  e.preventDefault()

  if (e.key === 'Enter' || e.key === ' ') {
    const opt = normalized.value[activeIdx.value]
    if (opt) choose(opt)
    return
  }
  const dir = e.key === 'ArrowDown' ? 1 : -1
  let i = activeIdx.value
  // 跳过 disabled 项
  for (let n = 0; n < normalized.value.length; n++) {
    i = (i + dir + normalized.value.length) % normalized.value.length
    if (!normalized.value[i].disabled) break
  }
  activeIdx.value = i
}

const onTriggerKeydown = (e) => {
  if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
    e.preventDefault()
    if (!open.value) show()
  }
}

// 外部把 modelValue 改了，展开态的高亮要跟着走
watch(() => props.modelValue, () => {
  if (!open.value) return
  activeIdx.value = normalized.value.findIndex((o) => o.value === props.modelValue)
})

onBeforeUnmount(close)
</script>

<template>
  <div class="ch-select" :class="[`ch-select--${size}`, wrapperClass, { 'is-open': open, 'is-disabled': disabled, 'is-placeholder': !selected }]">
    <button
      ref="trigger"
      type="button"
      class="ch-select-trigger"
      :disabled="disabled"
      :aria-expanded="open"
      aria-haspopup="listbox"
      @click="toggle"
      @keydown="onTriggerKeydown"
    >
      <span class="ch-select-value">{{ displayText }}</span>
      <Icon name="chevron-down" :size="16" class="ch-select-caret" />
    </button>

    <!-- Teleport 到 body：Modal 内容区 overflow-y:auto 会裁掉绝对定位的浮层 -->
    <Teleport to="body">
      <Transition name="ch-select-pop">
        <div
          v-if="open"
          ref="panel"
          class="ch-select-panel"
          :style="panelStyle"
          role="listbox"
        >
        <button
          v-for="(opt, i) in normalized"
          :key="String(opt.value)"
          type="button"
          class="ch-select-option"
          :class="{ 'is-selected': opt.value === modelValue, 'is-active': i === activeIdx, 'is-disabled': opt.disabled }"
          role="option"
          :aria-selected="opt.value === modelValue"
          :disabled="opt.disabled"
          @click="choose(opt)"
          @mouseenter="activeIdx = i"
        >
          <span>{{ opt.label }}</span>
          <Icon v-if="opt.value === modelValue" name="check" :size="16" class="ch-select-tick" />
          </button>
        </div>
      </Transition>
    </Teleport>
  </div>
</template>

<style scoped>
.ch-select { position: relative; width: 100%; }

.ch-select-trigger {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-2);
  width: 100%;
  min-height: 36px;
  padding: 8px 10px 8px 12px;
  border: 1px solid transparent;
  border-radius: var(--radius-md);
  background-color: var(--bg-input);
  color: var(--text-primary);
  font-family: inherit;
  font-size: var(--text-base);
  line-height: 1.5;
  text-align: left;
  cursor: pointer;
  transition: border-color 0.15s var(--ease-out), box-shadow 0.15s var(--ease-out), background-color 0.15s var(--ease-out);
}
.ch-select-trigger:hover:not(:disabled) { filter: brightness(0.97); }
.ch-select-trigger:focus-visible,
.ch-select.is-open .ch-select-trigger {
  outline: none;
  border-color: var(--accent);
  background-color: var(--bg-base);
  box-shadow: 0 0 0 2px var(--accent-muted);
}
.ch-select-trigger:disabled { opacity: 0.55; cursor: not-allowed; }

.ch-select--sm .ch-select-trigger {
  min-height: 32px;
  padding: 5px 8px 5px 10px;
  font-size: var(--text-sm);
}

/* 无选中值时用弱化色，提示「还没选」 */
.ch-select.is-placeholder .ch-select-value { color: var(--text-muted); }

.ch-select-value {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.ch-select-caret { flex-shrink: 0; color: var(--text-muted); transition: transform 0.18s var(--ease-out); }
.ch-select.is-open .ch-select-caret { transform: rotate(180deg); }

/* 面板：这是原生 select 做不到的部分 —— 完全自己画。
   fixed + Teleport 到 body：脱离任何祖先的 overflow 裁剪（Modal 尤其明显），
   坐标由 JS 按触发器位置算出，所以 width 也跟着触发器走。 */
.ch-select-panel {
  position: fixed;
  z-index: 1100;              /* 高于 Modal 的 1000 */
  overflow-y: auto;
  padding: var(--space-1);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
  background-color: var(--bg-elevated);
  box-shadow: 0 8px 24px -4px hsl(0 0% 0% / 0.18), 0 2px 6px -2px hsl(0 0% 0% / 0.10);
  overscroll-behavior: contain;
  /* 隐藏滚动条但保留滚动：面板高度受限时仍能滚，只是不显示滚动条轨道 */
  scrollbar-width: none;
  -ms-overflow-style: none;
}
.ch-select-panel::-webkit-scrollbar {
  display: none;
}
html.dark .ch-select-panel {
  box-shadow: 0 8px 24px -4px hsl(0 0% 0% / 0.55), 0 2px 6px -2px hsl(0 0% 0% / 0.4);
}

.ch-select-option {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-2);
  width: 100%;
  padding: 7px 10px;
  border: none;
  border-radius: var(--radius-sm, 4px);
  background: transparent;
  color: var(--text-primary);
  font-family: inherit;
  font-size: var(--text-base);
  line-height: 1.4;
  text-align: left;
  cursor: pointer;
  transition: background-color 0.12s var(--ease-out);
}
.ch-select--sm .ch-select-option { font-size: var(--text-sm); padding: 5px 10px; }

/* hover 与键盘高亮用同一套观感 */
.ch-select-option.is-active:not(.is-disabled) { background-color: var(--bg-hover); }
.ch-select-option.is-selected { color: var(--accent); font-weight: var(--font-medium); }
.ch-select-option.is-disabled { opacity: 0.45; cursor: not-allowed; }
.ch-select-tick { flex-shrink: 0; color: var(--accent); }

.ch-select-pop-enter-active,
.ch-select-pop-leave-active { transition: opacity 0.14s var(--ease-out), transform 0.14s var(--ease-out); }
.ch-select-pop-enter-from,
.ch-select-pop-leave-to { opacity: 0; transform: translateY(-4px); }

</style>