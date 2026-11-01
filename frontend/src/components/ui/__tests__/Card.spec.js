import { describe, it, expect } from 'vitest'
import { mount } from '@vue/test-utils'
import Card from '@/components/ui/Card.vue'

describe('Card.vue', () => {
  it('renders slot content', () => {
    const wrapper = mount(Card, { slots: { default: '<p class="x">hi</p>' } })
    expect(wrapper.find('.x').exists()).toBe(true)
    expect(wrapper.text()).toContain('hi')
  })

  it('has surface card class', () => {
    const wrapper = mount(Card)
    expect(wrapper.classes()).toContain('ch-card')
  })

  it('supports flush mode (no padding)', () => {
    const wrapper = mount(Card, { props: { padding: false } })
    expect(wrapper.classes()).toContain('ch-card--flush')
  })
})
