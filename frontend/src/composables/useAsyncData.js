import { ref, onMounted, onActivated, onDeactivated, onUnmounted, getCurrentInstance } from 'vue'

/**
 * 统一「拉数据 + loading + error + 重试」（不用 vueuse，保持零新依赖）。
 *
 * 91 个页面里 36 个各自抄了一遍 `const loading = ref(false)` + `const load = async () => {...}`，
 * 抄的过程丢了三件容易只在前端炸的事，这里一次解决：
 *
 *  1. 翻/切页时新请求 abort 旧请求 —— 否则老响应后到，把新数据盖回去（列表页错行、数字倒退）；
 *  2. keep-alive 退场（onDeactivated）后不再发请求、迟到响应直接丢弃 —— 布局是 keep-alive，
 *     页面退场后实例还活着，照样会往 ref 里写，用户切回来看到的是别的页面的数据；
 *  3. 卸载后不再写 ref —— 卸载后写 ref 在 Vue 里不会报错但会静默泄漏，也不好排查。
 *
 * 用法（fetcher 需要取消能力时，把多出来的最后一个参数接到 axios 的 config 上）：
 *
 *   const { data, loading, error, run, reset } = useAsyncData(
 *     (params, { signal }) => adminApi.listUsers(params, { signal }),
 *     { initial: [], immediate: true },
 *   )
 *   await run({ page: 2 })
 *
 * run(...args) 会把 args 原样透传给 fetcher，并在最后追加 { signal }；
 * 成功返回 fetcher 的结果，失败 / 被取消返回 null，失败原因同时写进 error.value（字符串）
 * 和 error.raw（原始错误对象，需要看 status / response 时用）。
 *
 * 不做轮询、不做缓存、不做乐观更新 —— 那些是别的 composable 的事。
 */
export function useAsyncData(fetcher, options = {}) {
  const {
    initial = null,
    immediate = false,
    abortable = true,
    // 错误对象没有 message 时的兜底文案（不同页面的措辞不一样，所以可配）
    fallbackMessage = '请求失败，请检查网络后重试',
  } = options

  // 初始值按「值」复制一份：initial 是数组/对象时，reset() 必须给回新的一份，
  // 否则两个页面实例会共用同一个数组（改一个另一个跟着变）。
  const seed = () => (Array.isArray(initial) ? [...initial] : initial && typeof initial === 'object' ? { ...initial } : initial)

  const data = ref(seed())
  const loading = ref(false)
  const error = ref('')
  // 原始错误对象挂在 ref 上（不是 .value 上），避免和「字符串」这个约定打架
  error.raw = null

  let controller = null
  // 请求流水号：只有最新一次请求的响应允许落地
  let seq = 0
  // keep-alive 退场后本实例还活着，但已经不该再拉数据、也不该再写 ref
  let active = true
  let unmounted = false

  // composable 也可能在组件外被直接 new 出来用（比如单测），有实例才注册生命周期
  if (getCurrentInstance()) {
    onActivated(() => { active = true })
    onDeactivated(() => { active = false })
    onUnmounted(() => {
      unmounted = true
      controller?.abort()
      controller = null
    })
  }

  const reset = () => {
    // 作废在飞请求：reset 之后迟到的响应不许再写 data
    seq++
    controller?.abort()
    controller = null
    data.value = seed()
    error.value = ''
    error.raw = null
    loading.value = false
  }

  const run = async (...args) => {
    if (unmounted || !active) return null

    const ticket = ++seq
    if (abortable) {
      controller?.abort()
      controller = new AbortController()
    }
    const signal = controller?.signal ?? null

    loading.value = true
    error.value = ''
    error.raw = null

    // 响应是否还有资格落地：被更新的一次请求顶掉、被 abort、页面退场或卸载，都不算
    const stale = () => ticket !== seq || !!signal?.aborted || unmounted || !active

    try {
      const res = await fetcher(...args, { signal })
      if (stale()) return null
      data.value = res
      return res
    } catch (e) {
      if (stale()) return null
      error.value = e?.message || fallbackMessage
      error.raw = e
      return null
    } finally {
      // 只有最新一次请求有权把 loading 关掉（旧请求 abort 后也会走到这里）
      if (ticket === seq && !unmounted) loading.value = false
    }
  }

  if (immediate) {
    if (getCurrentInstance()) onMounted(() => { run() })
    else run()
  }

  return { data, loading, error, run, reset }
}
