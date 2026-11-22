import { describe, it, expect, afterEach } from 'vitest'
import { defineComponent, h } from 'vue'
import { mount, flushPromises } from '@vue/test-utils'
import { confirm, ConfirmHost } from '@/composables/useConfirm'

/**
 * useConfirm 守三件事：确定 → true、取消 → false、连着弹两次不串台。
 * 第三个是重点：批量操作里两行各弹一次，第一个的点击绝不能把第二个也一起结掉。
 */

const Host = defineComponent({
  name: 'ConfirmHostTestWrapper',
  setup: () => () => h(ConfirmHost),
})

const mounted = []
const mountHost = () => {
  const wrapper = mount(Host, { attachTo: document.body })
  mounted.push(wrapper)
  return wrapper
}

afterEach(() => {
  // 卸载时宿主会把还挂着的调用按「取消」结掉，下一个用例从干净状态开始
  mounted.forEach((w) => w.unmount())
  mounted.length = 0
  document.body.innerHTML = ''
})

/** 弹窗走 Teleport 到 body，按钮只能从 document 上找 */
const footerButton = (label) => {
  const btn = [...document.querySelectorAll('.ch-modal-footer button')].find((b) =>
    b.textContent.trim().includes(label),
  )
  expect(btn, `弹窗底部没找到「${label}」按钮`).toBeTruthy()
  return btn
}

const clickFooter = async (label) => {
  footerButton(label).click()
  await flushPromises()
}

describe('useConfirm', () => {
  it('点确定返回 true，标题 / 正文 / 危险色都按参数渲染', async () => {
    mountHost()

    const answer = confirm({ title: '删除用户', message: '确定删除该用户？', danger: true })
    await flushPromises()

    expect(document.body.textContent).toContain('删除用户')
    expect(document.body.textContent).toContain('确定删除该用户？')
    // danger → 主按钮走红色（Button 的 danger variant）
    expect(footerButton('确定').className).toContain('btn-danger')

    await clickFooter('确定')
    expect(await answer).toBe(true)
  })

  it('点取消返回 false，自定义按钮文案生效', async () => {
    mountHost()

    const answer = confirm({ title: '迁移', message: '计划迁移 12 个用户，确定执行？', confirmText: '开始迁移', cancelText: '再想想' })
    await flushPromises()

    expect(document.body.textContent).toContain('开始迁移')
    expect(document.body.textContent).toContain('再想想')

    await clickFooter('再想想')
    expect(await answer).toBe(false)
  })

  it('连续调用两次不串台：第一次的点击只结第一次，第二次照常等自己的答案', async () => {
    mountHost()

    const first = confirm({ message: '第一条确认' })
    const second = confirm({ message: '第二条确认' })
    await flushPromises()

    // 同一时刻只显示第一条
    expect(document.body.textContent).toContain('第一条确认')
    expect(document.body.textContent).not.toContain('第二条确认')

    await clickFooter('确定')
    expect(await first).toBe(true)

    // 第一条结论出来后，第二条才接上来
    expect(document.body.textContent).toContain('第二条确认')
    await clickFooter('取消')
    expect(await second).toBe(false)
  })

  it('确认框层级高于页面弹窗（1400 > 1000），否则叠在弹窗里点「确认」时会被盖在底层点不到', async () => {
    mountHost()

    const answer = confirm({ message: '嵌套在弹窗里的确认' })
    await flushPromises()

    const overlay = document.querySelector('.ch-modal-overlay')
    expect(overlay).toBeTruthy()
    // 页面弹窗默认 z-index:1000（Modal.css），确认框必须显式抬高，同层级下 DOM 靠后的弹窗会盖住它。
    expect(Number(overlay.style.zIndex)).toBeGreaterThan(1000)

    await clickFooter('确定')
    expect(await answer).toBe(true)
  })
})
