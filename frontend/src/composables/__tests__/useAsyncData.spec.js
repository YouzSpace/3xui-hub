import { describe, it, expect, vi, afterEach } from 'vitest'
import { defineComponent, h } from 'vue'
import { mount, flushPromises } from '@vue/test-utils'
import { useAsyncData } from '@/composables/useAsyncData'

/**
 * useAsyncData 守五件事：成功落地 / 失败留证 / 新请求顶掉旧请求 / 卸载后不写 ref / 重试能恢复。
 * 这四条都是「写错了也不会报错、只会在真实网络下悄悄出错」的地方。
 */

const mounted = []
afterEach(() => {
  mounted.forEach((w) => w.unmount())
  mounted.length = 0
})

/** 手动控制何时 resolve 的 fetcher：用来制造「新请求已发出、旧响应才回来」的时序 */
const deferredFetcher = () => {
  const signals = []
  const resolvers = []
  // 注意第二个参数是 useAsyncData 追加的 { signal }，run() 不带参数时它照样在末尾
  const fetcher = (params, config) => {
    signals.push(config?.signal)
    return new Promise((resolve) => resolvers.push(resolve))
  }
  return { fetcher, signals, resolvers }
}

describe('useAsyncData', () => {
  it('成功：data 落地、loading 关掉、error 清空，参数和 signal 透传给 fetcher', async () => {
    const fetcher = vi.fn(() => Promise.resolve([{ id: 1 }]))
    const { data, loading, error, run } = useAsyncData(fetcher, { initial: [] })

    const p = run({ page: 1 })
    expect(loading.value).toBe(true)

    await p

    expect(fetcher).toHaveBeenCalledTimes(1)
    const [params, config] = fetcher.mock.calls[0]
    expect(params).toEqual({ page: 1 })
    expect(config.signal).toBeInstanceOf(AbortSignal)
    expect(data.value).toEqual([{ id: 1 }])
    expect(loading.value).toBe(false)
    expect(error.value).toBe('')
  })

  it('失败：写 error 字符串 + error.raw 原始对象，data 保持上一次的值', async () => {
    const boom = new Error('列表加载失败')
    const fetcher = vi.fn(() => Promise.reject(boom))
    const { data, loading, error, run } = useAsyncData(fetcher, { initial: [{ id: 9 }] })

    const res = await run()

    expect(res).toBeNull()
    expect(error.value).toBe('列表加载失败')
    expect(error.raw).toBe(boom)
    // 失败不清空已有数据：页面上还能看到上一次拉到的内容，只在旁边报错
    expect(data.value).toEqual([{ id: 9 }])
    expect(loading.value).toBe(false)
  })

  it('abort 旧请求：第二次 run 会 abort 第一次，迟到的旧响应不覆盖新数据', async () => {
    const { fetcher, signals, resolvers } = deferredFetcher()
    const { data, run } = useAsyncData(fetcher)

    const first = run(1)
    const second = run(2)

    expect(signals).toHaveLength(2)
    expect(signals[0].aborted).toBe(true)
    expect(signals[1].aborted).toBe(false)

    // 故意让旧请求后返回
    resolvers[0](['旧数据'])
    resolvers[1](['新数据'])
    await Promise.all([first, second])

    expect(data.value).toEqual(['新数据'])
  })

  it('卸载后不写 ref：组件卸载前发出的请求，响应回来也不落 data', async () => {
    const { fetcher, resolvers } = deferredFetcher()
    let api = null

    const Page = defineComponent({
      setup() {
        api = useAsyncData(fetcher, { initial: '初始值', immediate: true })
        return () => h('div')
      },
    })

    const wrapper = mount(Page)
    mounted.push(wrapper)
    await flushPromises()
    expect(api.loading.value).toBe(true)

    wrapper.unmount()
    mounted.length = 0

    resolvers[0]('迟到的响应')
    await flushPromises()

    expect(api.data.value).toBe('初始值')
  })

  it('重试：失败后再 run 一次能恢复，error 清空', async () => {
    let call = 0
    const fetcher = vi.fn(() => (++call === 1 ? Promise.reject(new Error('网络断了')) : Promise.resolve([1, 2])))
    const { data, error, run } = useAsyncData(fetcher, { initial: [] })

    await run()
    expect(error.value).toBe('网络断了')

    await run()
    expect(error.value).toBe('')
    expect(data.value).toEqual([1, 2])
    expect(fetcher).toHaveBeenCalledTimes(2)
  })
})
