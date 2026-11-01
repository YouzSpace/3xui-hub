import { describe, it, expect, beforeEach } from 'vitest'
import { mount } from '@vue/test-utils'
import { defineComponent, h, getCurrentInstance } from 'vue'
import Toast, { useToast } from '@/components/ui/Toast.vue'

// Mount <Toast/> so the render is active (toasts teleport to body).
function mountToast() {
  return mount(Toast, { attachTo: document.body })
}

// Call useToast() inside a component setup context.
function getToastApi() {
  let api
  const Probe = defineComponent({
    setup() {
      api = useToast()
      return () => h('div')
    },
  })
  mount(Probe)
  return api
}

describe('Toast.vue', () => {
  beforeEach(() => {
    // Clean body between tests
    document.body.innerHTML = ''
  })

  it('useToast is a named export with success/error/info', () => {
    const api = getToastApi()
    expect(api).toBeTruthy()
    expect(typeof api.success).toBe('function')
    expect(typeof api.error).toBe('function')
    expect(typeof api.info).toBe('function')
  })

  it('success() renders a success toast in the DOM', async () => {
    const wrapper = mountToast()
    const api = getToastApi()
    api.success('copied')
    await wrapper.vm.$nextTick()
    expect(document.body.textContent).toContain('copied')
    expect(document.body.querySelectorAll('.ch-toast--success').length).toBe(1)
    wrapper.unmount()
  })

  it('error() renders an error toast', async () => {
    const wrapper = mountToast()
    const api = getToastApi()
    api.error('boom')
    await wrapper.vm.$nextTick()
    expect(document.body.querySelectorAll('.ch-toast--error').length).toBe(1)
    wrapper.unmount()
  })
})
