import { describe, it, expect } from 'vitest'
import { mount } from '@vue/test-utils'
import Button from '@/components/ui/Button.vue'

describe('Button.vue', () => {
  it('renders default primary variant', () => {
    const wrapper = mount(Button, { slots: { default: 'Save' } })
    expect(wrapper.classes()).toContain('btn-primary')
    expect(wrapper.text()).toContain('Save')
  })

  it('applies variant class', () => {
    const wrapper = mount(Button, { props: { variant: 'danger' } })
    expect(wrapper.classes()).toContain('btn-danger')
  })

  it('applies size class', () => {
    const wrapper = mount(Button, { props: { size: 'lg' } })
    expect(wrapper.classes()).toContain('btn-lg')
  })

  it('disables when loading', () => {
    const wrapper = mount(Button, { props: { loading: true } })
    expect(wrapper.attributes('disabled')).toBeDefined()
    expect(wrapper.classes()).toContain('is-loading')
  })

  it('disables when disabled prop true', () => {
    const wrapper = mount(Button, { props: { disabled: true } })
    expect(wrapper.attributes('disabled')).toBeDefined()
  })

  it('emits click', async () => {
    const wrapper = mount(Button)
    await wrapper.trigger('click')
    expect(wrapper.emitted('click')).toBeTruthy()
  })
})
