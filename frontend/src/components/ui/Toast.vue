<script>
/**
 * Toast.vue + named export { useToast, createToastStore }
 *
 * Architecture (per F0 spec):
 *   - Module-level singleton store holds `toasts` ref + addToast/removeToast/success/error/info.
 *   - Toast.vue renders the store's toasts (Teleport to body).
 *   - useToast() returns the singleton (works anywhere, no provider required).
 *   - Layouts (M12 AdminLayout / M11 UserLayout) MAY additionally `provide('toast', store)`
 *     so descendants can `inject('toast')`. That wiring is the layout's job, not Toast's.
 *
 * No emoji. All colors via CSS variables.
 */
import { ref, inject } from 'vue'

let seq = 0

export function createToastStore() {
  const toasts = ref([])

  function addToast(message, type = 'info', duration = 2600) {
    const id = ++seq
    toasts.value.push({ id, message, type })
    if (duration > 0) {
      setTimeout(() => removeToast(id), duration)
    }
    return id
  }

  function removeToast(id) {
    const idx = toasts.value.findIndex((t) => t.id === id)
    if (idx !== -1) toasts.value.splice(idx, 1)
  }

  function success(message, duration, undoFn) {
    const id = addToast(message, 'success', duration)
    if (undoFn) {
      const t = toasts.value.find((x) => x.id === id)
      if (t) t.undo = undoFn
    }
    return id
  }
  function error(message, duration) {
    return addToast(message, 'error', duration)
  }
  function info(message, duration) {
    return addToast(message, 'info', duration)
  }

  return { toasts, addToast, removeToast, success, error, info }
}

// Module-level singleton — shared by Toast.vue render + useToast().
const store = createToastStore()

/**
 * Named export. Returns the shared toast API. If a layout has provided its own
 * 'toast' via provide(), that wins; otherwise the module singleton is used.
 */
export function useToast() {
  return inject('toast', store)
}
</script>

<script setup>
import Icon from '@/components/ui/Icon.vue'

const ICON = {
  success: 'check',
  error: 'x',
  info: 'activity',
}

/** 点撤销：执行回调后关掉本条 toast（删除操作的反悔入口） */
function doUndo(t) {
  if (typeof t.undo === 'function') t.undo()
  store.removeToast(t.id)
}
</script>

<template>
  <Teleport to="body">
    <TransitionGroup name="ch-toast" tag="div" class="ch-toast-wrap">
      <div v-for="t in store.toasts.value" :key="t.id" class="ch-toast" :class="`ch-toast--${t.type}`">
        <Icon :name="ICON[t.type] || 'activity'" :size="16" />
        <span class="ch-toast-msg">{{ t.message }}</span>
        <button v-if="t.undo" class="ch-toast-undo" type="button" @click="doUndo(t)">撤销</button>
        <button class="ch-toast-close" type="button" aria-label="Close" @click="store.removeToast(t.id)">
          <Icon name="x" :size="14" />
        </button>
      </div>
    </TransitionGroup>
  </Teleport>
</template>

<style scoped>
.ch-toast-wrap {
  position: fixed;
  top: var(--space-5);
  right: var(--space-5);
  z-index: 2000;
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  pointer-events: none;
}
.ch-toast {
  pointer-events: auto;
  display: flex;
  align-items: center;
  gap: var(--space-2);
  padding: 10px 14px;
  background: var(--bg-elevated);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
  box-shadow: var(--shadow-md);
  color: var(--text-primary);
  font-size: var(--text-sm);
  min-width: 220px;
  max-width: 360px;
}
.ch-toast--success {
  border-left: 3px solid var(--success);
  color: var(--success);
}
.ch-toast--error {
  border-left: 3px solid var(--danger);
  color: var(--danger);
}
.ch-toast--info {
  border-left: 3px solid var(--accent);
  color: var(--accent);
}
.ch-toast-msg {
  flex: 1;
  color: var(--text-primary);
  font-family: var(--font-sans);
}
.ch-toast-close {
  background: transparent;
  border: none;
  color: var(--text-muted);
  padding: 2px;
  display: inline-flex;
  border-radius: var(--radius-sm);
}
.ch-toast-close:hover {
  color: var(--text-primary);
  background: var(--bg-hover);
}
/* 撤销按钮：强调色，点一下反悔刚做的删除操作 */
.ch-toast-undo {
  background: transparent;
  border: none;
  color: var(--accent);
  font-weight: var(--font-semibold);
  font-size: var(--text-sm);
  padding: 2px 6px;
  border-radius: var(--radius-sm);
  cursor: pointer;
  white-space: nowrap;
}
.ch-toast-undo:hover {
  background: var(--accent-muted);
}
.ch-toast-enter-active,
.ch-toast-leave-active {
  transition: transform var(--duration-base) var(--ease-spring), opacity var(--duration-base) var(--ease-out);
}
.ch-toast-enter-from,
.ch-toast-leave-to {
  transform: translateX(16px);
  opacity: 0;
}
</style>
