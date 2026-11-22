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
  // 叠放层级：默认 1000（普通页面弹窗）。嵌套场景（如「确认框叠在某个弹窗之上」）
  // 需要显式抬高，否则同层级下 DOM 靠后的弹窗会把确认框盖住。
  zIndex: { type: Number, default: 1000 },
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
      <div v-if="modelValue" class="ch-modal-overlay" :style="{ zIndex }" @click.self="close">
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

<style scoped src="@/assets/styles/components/ui/Modal.css"></style>
