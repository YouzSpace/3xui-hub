import { ref } from 'vue'

/**
 * 统一「表单 + 校验 + 提交 + toast」。
 *
 * 页面上这段逻辑的重复度最高，而且每次抄都会丢一样东西：漏掉 submitting 就能连点三下
 * 建出三条记录；校验写在提交函数里 return 前一句，前端提示和实际拦下的条件对不上。
 *
 *   const { form, errors, submitting, submit, reset, setForm } = useCrudForm({
 *     initial: () => ({ name: '', price: 0 }),
 *     validate: (f) => ({ name: !f.name ? '必填' : '', price: f.price < 0 ? '不能为负' : '' }),
 *     onSubmit: async (f) => adminApi.createPlan(f),
 *     onSuccess: () => { toast.success('已保存'); close() },
 *     onError: (e) => toast.error(e?.message || '保存失败'),
 *     onInvalid: (errs) => toast.error(Object.values(errs)[0]),
 *   })
 *
 * 约定：
 *  - validate 返回「字段名 → 错误消息」，空字符串 / null 表示这一项没问题（方便三元写），
 *    真正的 errors 只保留有消息的字段；
 *  - submit() 先 validate，有错就调 onInvalid 并返回 false，绝不调 onSubmit；
 *  - submitting 期间再点直接返回 false，不会重复提交；
 *  - onSubmit 抛错时交给 onError（页面自己决定 toast 措辞），返回 false；
 *    没给 onError 就把错误抛出去 —— 提交失败绝不允许被静默吞掉。
 *
 * 为什么 initial 是函数而不是对象：编辑弹窗要「回填 → 关掉 → 再开新建」时拿回干净的一份，
 * 共享一个对象的话上一次输的内容会残留在下一次新建里。
 */
export function useCrudForm(options = {}) {
  const { initial = () => ({}), validate, onSubmit, onSuccess, onError, onInvalid } = options

  const seed = () => {
    const v = initial()
    return v && typeof v === 'object' ? { ...v } : {}
  }

  const form = ref(seed())
  const errors = ref({})
  const submitting = ref(false)

  const reset = () => {
    form.value = seed()
    errors.value = {}
  }

  /** 编辑回填：以 initial() 为底再盖上一层，缺字段的用默认值，不会留 undefined */
  const setForm = (obj) => {
    form.value = { ...seed(), ...(obj || {}) }
    errors.value = {}
  }

  const submit = async () => {
    if (submitting.value) return false

    const found = (validate ? validate(form.value) : null) || {}
    const next = {}
    for (const [field, message] of Object.entries(found)) {
      if (message) next[field] = message
    }
    errors.value = next
    if (Object.keys(next).length) {
      onInvalid?.(next)
      return false
    }

    submitting.value = true
    try {
      const res = await onSubmit?.(form.value)
      onSuccess?.(res)
      return true
    } catch (e) {
      if (!onError) throw e
      onError(e)
      return false
    } finally {
      submitting.value = false
    }
  }

  return { form, errors, submitting, submit, reset, setForm }
}
