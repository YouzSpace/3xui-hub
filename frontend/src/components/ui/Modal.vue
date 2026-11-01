<script setup>
/**
 * Modal.vue
 * - overlay + var(--bg-elevated) card
 * - var(--shadow-lg) + var(--radius-xl)
 */
import Icon from '@/components/ui/Icon.vue'

const props = defineProps({
  modelValue: { type: Boolean, default: false },
  title: { type: String, default: '' },
  persistent: { type: Boolean, default: false },
  width: { type: String, default: '480px' },
})

const emit = defineEmits(['update:modelValue', 'close'])

const forceClose = () => {
  emit("update:modelValue", false)
  emit("close")
}

const close = () => {
  if (props.persistent) return
  emit('update:modelValue', false)
  emit('close')
}
</script>

<template>
  <Teleport to="body">
    <Transition name="ch-modal">
      <div v-if="modelValue" class="ch-modal-overlay" @click.self="close">
        <div class="ch-modal-card" :style="{ maxWidth: width }" role="dialog" aria-modal="true">
          <div v-if="title || $slots.header" class="ch-modal-header">
            <slot name="header">
              <h3 class="ch-modal-title">{{ title }}</h3>
            </slot>
            <button class="ch-modal-close" type="button" aria-label="Close" @click="forceClose">
              <Icon name="x" :size="20" />
            </button>
          </div>
          <div class="ch-modal-body">
            <slot />
          </div>
          <div v-if="$slots.footer" class="ch-modal-footer">
            <slot name="footer" />
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>

<style scoped>
.ch-modal-overlay {
  position: fixed;
  inset: 0;
  background: var(--overlay);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 1000;
  padding: var(--space-4);
}
.ch-modal-card {
  width: 100%;
  background: var(--bg-surface);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-xl);
  box-shadow: var(--shadow-lg);
  max-height: calc(100vh - var(--space-8));
  display: flex;
  flex-direction: column;
}
.ch-modal-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: var(--space-4) var(--space-6);
  border-bottom: 1px solid var(--border-subtle);
}
.ch-modal-title {
  margin: 0;
  font-size: var(--text-base);
  font-weight: var(--font-semibold);
  color: var(--text-primary);
}
.ch-modal-close {
  background: transparent;
  border: none;
  color: var(--text-muted);
  padding: var(--space-1);
  border-radius: var(--radius-md);
  display: inline-flex;
  align-items: center;
  transition: color var(--duration-fast) var(--ease-out), background var(--duration-fast) var(--ease-out);
}
.ch-modal-close:hover {
  color: var(--text-primary);
  background: var(--bg-hover);
}
.ch-modal-body {
  padding: var(--space-6);
  overflow-y: auto;
}
.ch-modal-footer {
  padding: var(--space-4) var(--space-6);
  border-top: 1px solid var(--border-subtle);
  display: flex;
  justify-content: flex-end;
  gap: var(--space-2);
}
.ch-modal-enter-active,
.ch-modal-leave-active {
  transition: opacity var(--duration-base) var(--ease-out);
}
.ch-modal-enter-active .ch-modal-card,
.ch-modal-leave-active .ch-modal-card {
  transition: transform var(--duration-slow) var(--ease-spring), opacity var(--duration-base) var(--ease-out);
}
.ch-modal-enter-from,
.ch-modal-leave-to {
  opacity: 0;
}
.ch-modal-enter-from .ch-modal-card,
.ch-modal-leave-to .ch-modal-card {
  transform: translateY(8px) scale(0.98);
  opacity: 0;
}

/*
 * 窄屏（手机）：弹窗必须让 footer 的「保存/取消」始终可达。
 *
 * 桌面用 100vh 没问题；手机浏览器（Edge/Chrome）的地址栏与底栏会占掉 80–150px，
 * 此时 100vh 等于「浏览器 UI 收起时」的大视口高度 —— 卡片按 100vh 撑满后，
 * footer 落在真实可视区之外；而 overlay 是 position:fixed 且 overflow:visible，
 * 用户没有任何办法把它滚进屏幕，只能退出弹窗。
 *
 * 改法：窄屏用 dvh（真实可视高度）限制卡片，body 内部滚动、footer 固定在卡片底部；
 * overlay 额外给一份 overflow-y:auto 兜底（dvh 不支持或软键盘弹出时仍可滚）。
 * 桌面端一行不动。
 */
@media (max-width: 767px) {
  .ch-modal-overlay {
    align-items: flex-start;
    overflow-y: auto;
    padding: var(--space-4) var(--space-3);
    /* iPhone 底部 home indicator：不垫开的话 footer 的按钮会压在系统横条上点不准 */
    padding-bottom: calc(var(--space-4) + env(safe-area-inset-bottom, 0px));
  }
  .ch-modal-card {
    max-height: calc(100vh - var(--space-8)); /* 老浏览器兜底 */
    max-height: calc(100dvh - var(--space-8)); /* 真实可视高度 */
    margin-block: auto; /* 卡片比可视区矮时仍居中 */
  }
}
</style>
