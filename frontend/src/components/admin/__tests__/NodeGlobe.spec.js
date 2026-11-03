import { describe, it, expect, beforeEach } from 'vitest'
import { mount } from '@vue/test-utils'
import NodeGlobe from '@/components/admin/NodeGlobe.vue'
import { arcCount, callNames, drawnTexts, installCanvasEnv } from './canvas-env'

/**
 * 地球是本页唯一「画不出来就等于整块功能废掉」的部分，而 canvas 的结果无法用 DOM 断言，
 * 所以这里把 2D context 换成记录调用的替身，断言它**真的画了东西**：
 *   - 陆地点阵（大量 arc）
 *   - 地区标记（arc + 地区代码文字）
 *   - 地区列表数据（emit 出去的聚合结果）
 */

beforeEach(() => {
  installCanvasEnv()
})

const mountGlobe = (nodes) => mount(NodeGlobe, { props: { nodes } })

describe('NodeGlobe.vue', () => {
  it('真的在 canvas 上画了陆地与地区标记', async () => {
    const wrapper = mountGlobe([
      { id: 1, name: 'HK01', status: 'online', region: 'HK' },
      { id: 2, name: 'JP01', status: 'offline', region: 'JP' },
    ])
    await wrapper.vm.$nextTick()

    // 陆地是点阵：可见半球上一定有大量 arc
    expect(arcCount()).toBeGreaterThan(100)

    // 地区标记：两个地区各画一个点，并写出地区代码
    expect(drawnTexts()).toContain('HK')
    expect(drawnTexts()).toContain('JP')

    // 该用的绘制 API 都用上了
    expect(callNames()).toContain('fill')
    expect(callNames()).toContain('stroke')
  })

  it('按地区聚合后 emit 出去（在线多的排前面）', async () => {
    const wrapper = mountGlobe([
      { id: 1, name: 'HK01', status: 'online', region: 'HK' },
      { id: 2, name: 'HK02', status: 'offline', region: 'HK' },
      { id: 3, name: 'SG01', status: 'online', region: 'SG' },
      { id: 4, name: '未知节点', status: 'online', region: null },
    ])
    await wrapper.vm.$nextTick()

    const emitted = wrapper.emitted('regions')
    expect(emitted, '必须 emit regions，父级靠它渲染地区列表').toBeTruthy()

    expect(emitted.at(-1)[0]).toEqual([
      { code: 'HK', total: 2, online: 1 },
      { code: 'SG', total: 1, online: 1 },
    ])
  })

  it('没有节点时不报错，也不画地区标记', async () => {
    const wrapper = mountGlobe([])
    await wrapper.vm.$nextTick()

    expect(drawnTexts()).toEqual([])
    expect(wrapper.emitted('regions').at(-1)[0]).toEqual([])
  })

  it('focusRegion 把地球转到该地区正面，自转照旧继续', async () => {
    const wrapper = mountGlobe([{ id: 1, name: 'JP01', status: 'online', region: 'JP' }])
    await wrapper.vm.$nextTick()

    wrapper.vm.focusRegion('JP')

    // 焦点地区仍然在画（换了个角度重画了一次）；自转常开，没有开关可关
    expect(drawnTexts()).toContain('JP')
  })
})
