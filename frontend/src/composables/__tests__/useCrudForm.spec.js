import { describe, it, expect, vi } from 'vitest'
import { useCrudForm } from '@/composables/useCrudForm'

/**
 * useCrudForm 守四件事：校验没过不提交 / 校验通过才调 onSubmit / 提交中再点不重复发 /
 * 编辑回填（setForm）拿到的是干净的一份。
 */

const planForm = (overrides = {}) =>
  useCrudForm({
    initial: () => ({ name: '', price: 0 }),
    validate: (f) => ({ name: !f.name ? '必填' : '', price: f.price < 0 ? '不能为负' : '' }),
    ...overrides,
  })

describe('useCrudForm', () => {
  it('校验失败：errors 只留出错的字段，onSubmit 一次都不调', async () => {
    const onSubmit = vi.fn()
    const onInvalid = vi.fn()
    const { form, errors, submit } = planForm({ onSubmit, onInvalid })

    form.value.name = ''
    form.value.price = -1

    expect(await submit()).toBe(false)
    expect(errors.value).toEqual({ name: '必填', price: '不能为负' })
    expect(onInvalid).toHaveBeenCalledWith({ name: '必填', price: '不能为负' })
    expect(onSubmit).not.toHaveBeenCalled()
  })

  it('校验通过：把表单值交给 onSubmit，errors 清空', async () => {
    const onSubmit = vi.fn(() => Promise.resolve({ id: 7 }))
    const onSuccess = vi.fn()
    const { form, errors, submit } = planForm({ onSubmit, onSuccess })

    form.value.name = '香港-100G'
    form.value.price = 19

    expect(await submit()).toBe(true)
    expect(onSubmit).toHaveBeenCalledTimes(1)
    expect(onSubmit.mock.calls[0][0]).toMatchObject({ name: '香港-100G', price: 19 })
    expect(onSuccess).toHaveBeenCalledWith({ id: 7 })
    expect(errors.value).toEqual({})
  })

  it('提交中再点不重复发请求，submitting 期间返回 false', async () => {
    let release
    const onSubmit = vi.fn(
      () =>
        new Promise((resolve) => {
          release = resolve
        }),
    )
    const { form, submitting, submit } = planForm({ onSubmit })
    form.value.name = '日本-50G'

    const first = submit()
    expect(submitting.value).toBe(true)

    // 连点第二下：直接短路，不会进 onSubmit
    expect(await submit()).toBe(false)
    expect(onSubmit).toHaveBeenCalledTimes(1)

    release({})
    expect(await first).toBe(true)
    expect(submitting.value).toBe(false)
  })

  it('setForm 回填：缺的字段用初始值补齐，并清掉上一次的校验错误', async () => {
    const { form, errors, setForm, reset } = planForm()

    form.value.name = '旧内容'
    form.value.price = -5
    await Promise.resolve()
    errors.value = { price: '不能为负' }

    setForm({ name: '美国-100G', price: 29 })

    expect(form.value).toEqual({ name: '美国-100G', price: 29 })
    expect(errors.value).toEqual({})

    // 关掉弹窗再开新建：回到干净的一份
    reset()
    expect(form.value).toEqual({ name: '', price: 0 })
  })
})
