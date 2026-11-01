<script setup>
/**
 * BottomSheet — 通用底部弹层外壳（稿中 mat-sheet + modal-rise）。
 * v-model 控制显隐；默认插槽放内容，footer 插槽放底部按钮。
 */
defineProps({
  modelValue: { type: Boolean, default: false },
  title: { type: String, default: '' },
  maxWidth: { type: String, default: '512px' },
})
const emit = defineEmits(['update:modelValue', 'close'])

const close = () => {
  emit('update:modelValue', false)
  emit('close')
}
</script>

<template>
  <Teleport to="body">
    <Transition name="uu-sheet">
      <!-- Teleport 到 body 后脱离 .uu-root 子树，这里补挂 uu-root 以继承主题变量 -->
      <div v-if="modelValue" class="uu-root uu-sheet-overlay" @click.self="close">
        <div
          class="uu-sheet uu-mat-sheet uu-modal-rise"
          :style="{ maxWidth: maxWidth }"
          role="dialog"
          aria-modal="true"
        >
          <p v-if="title" class="uu-sheet-title">{{ title }}</p>
          <div class="uu-sheet-body">
            <slot />
          </div>
          <div v-if="$slots.footer" class="uu-sheet-footer">
            <slot name="footer" />
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>

<style scoped>
.uu-sheet-overlay {
  position: fixed;
  inset: 0;
  z-index: 50;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 16px;
  background: rgba(0, 0, 0, 0.6);
  backdrop-filter: blur(8px);
  -webkit-backdrop-filter: blur(8px);
}
.uu-sheet {
  width: 100%;
  max-height: calc(100dvh - 32px);
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: 20px;
  padding: 24px;
  border-radius: 24px;
}
.uu-sheet-title {
  margin: 0;
  font-size: 17px;
  font-weight: 600;
  color: var(--lbl1);
}
.uu-sheet-body {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.uu-sheet-footer {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.uu-sheet-enter-active,
.uu-sheet-leave-active {
  transition: opacity 0.28s ease;
}
.uu-sheet-enter-from,
.uu-sheet-leave-to {
  opacity: 0;
}
@media (min-width: 768px) {
  .uu-sheet-overlay {
    padding: 24px;
  }
  .uu-sheet {
    max-height: calc(100dvh - 48px);
  }
}
</style>
