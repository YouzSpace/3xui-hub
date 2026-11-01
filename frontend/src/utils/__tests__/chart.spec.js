import { describe, it, expect } from 'vitest'
import { chartGeometry } from '@/utils/chart'

/**
 * SVG 折线几何的连续性契约。
 *
 * 最要紧的一条：**曲线永远视觉连续**。
 *   - 采样间隔再大（页面隐藏 / 切页 / 轮询失败）也不断开
 *   - 缺失值（null / 负数）只跳过该点，前后有效点直连，不留断口
 * 断口在普通用户眼里像故障，产品明确取舍为「无采样区间画插值直线」。
 */

const opts = { width: 100, height: 100, padTop: 0, padBottom: 0, padLeft: 0, padRight: 0 }

const countOf = (d, command) => (d.match(new RegExp(command, 'g')) || []).length

describe('chartGeometry', () => {
  it('基本行为：首点有效 → M 起头，后续点用 L', () => {
    const { paths } = chartGeometry(
      [
        { t: 0, v: 5 },
        { t: 1000, v: 10 },
        { t: 2000, v: 0 },
      ],
      [{ key: 'v' }],
      { ...opts, max: 10 },
    )

    const d = paths[0].d
    expect(d.startsWith('M')).toBe(true)
    expect(countOf(d, 'M')).toBe(1)
    expect(countOf(d, 'L')).toBe(2)
    // 坐标：t=0→x0，v=5→y50；t=1000→x50，v=10→y0；t=2000→x100，v=0→y100
    expect(d).toBe('M0.00,50.00L50.00,0.00L100.00,100.00')
  })

  it('相邻点间隔很大（远超原 gap）→ 仍是一条连续 path，整段只有一个 M', () => {
    const { paths } = chartGeometry(
      [
        { t: 0, v: 1 },
        { t: 100_000, v: 2 }, // 100 秒，远超旧的 15 秒 gap
      ],
      [{ key: 'v' }],
      { ...opts, max: 2 },
    )

    const d = paths[0].d
    expect(countOf(d, 'M')).toBe(1)
    expect(countOf(d, 'L')).toBe(1)
    expect(d).toBe('M0.00,50.00L100.00,0.00')
  })

  it('中间点值为 null → 跳过该点、前后有效点直连（无断点）', () => {
    const { paths } = chartGeometry(
      [
        { t: 0, v: 1 },
        { t: 1000, v: null },
        { t: 2000, v: 3 },
      ],
      [{ key: 'v' }],
      { ...opts, max: 3 },
    )

    const d = paths[0].d
    // 跳过 null 后，t=0 与 t=2000 之间是一条直线，没有第二个 M
    expect(countOf(d, 'M')).toBe(1)
    expect(countOf(d, 'L')).toBe(1)
    expect(d).toBe('M0.00,66.67L100.00,0.00')
  })

  it('负值同样按缺失处理：跳过该点、保留连线状态', () => {
    const { paths } = chartGeometry(
      [
        { t: 0, v: 2 },
        { t: 1000, v: -1 }, // 非法：负数
        { t: 2000, v: 4 },
      ],
      [{ key: 'v' }],
      { ...opts, max: 4 },
    )

    const d = paths[0].d
    expect(countOf(d, 'M')).toBe(1)
    expect(countOf(d, 'L')).toBe(1)
  })

  it('开头的无效点不产生多余 M：首个有效点才用 M', () => {
    const { paths } = chartGeometry(
      [
        { t: 0, v: null },
        { t: 1000, v: 5 },
        { t: 2000, v: 10 },
      ],
      [{ key: 'v' }],
      { ...opts, max: 10 },
    )

    const d = paths[0].d
    expect(d.startsWith('M')).toBe(true)
    expect(countOf(d, 'M')).toBe(1)
    expect(countOf(d, 'L')).toBe(1)
    expect(d).toBe('M50.00,50.00L100.00,0.00')
  })

  it('全无效值 → d 为空字符串', () => {
    const { paths } = chartGeometry(
      [
        { t: 0, v: null },
        { t: 1000, v: -5 },
        { t: 2000, v: undefined },
      ],
      [{ key: 'v' }],
      { ...opts, max: 10 },
    )

    expect(paths[0].d).toBe('')
  })

  it('双轴：axis=2 的系列走右轴量程，与左轴互不影响', () => {
    const { paths, ceiling, ceiling2, y2 } = chartGeometry(
      [
        { t: 0, rate: 100, cum: 1000 },
        { t: 1000, rate: 200, cum: 2000 },
      ],
      [{ key: 'rate' }, { key: 'cum', axis: 2 }],
      { ...opts, max: 200, max2: 2000 },
    )

    expect(ceiling).toBe(200)
    expect(ceiling2).toBe(2000)

    // 左轴系列：100 → y50，200 → y0
    const rate = paths.find((p) => p.key === 'rate')
    expect(rate.d).toBe('M0.00,50.00L100.00,0.00')

    // 右轴系列：1000 → y50（用右轴量程 2000 算），2000 → y0
    const cum = paths.find((p) => p.key === 'cum')
    expect(cum.d).toBe('M0.00,50.00L100.00,0.00')
    expect(y2(1000)).toBeCloseTo(50, 6)
  })

  it('双轴：某条轴的系列全是无效值时，该轴量程退化为 1（不出现 NaN/除零）', () => {
    const { ceiling2, paths } = chartGeometry(
      [{ t: 0, v: 5 }],
      [{ key: 'v' }, { key: 'cum', axis: 2 }],
      { ...opts },
    )

    // 量程退化为 1（再乘 1.15 的头部留白）—— 关键是有限且 > 0，y2 不会除零出 NaN
    expect(Number.isFinite(ceiling2)).toBe(true)
    expect(ceiling2).toBeGreaterThan(0)
    expect(paths.find((p) => p.key === 'cum').d).toBe('')
  })

  it('单轴调用（不传 axis）行为不变：仍是一条量程、一个 ceiling2 默认值', () => {
    const single = chartGeometry(
      [
        { t: 0, v: 1 },
        { t: 1000, v: 2 },
      ],
      [{ key: 'v' }],
      { ...opts, max: 2 },
    )

    expect(single.ceiling).toBe(2)
    expect(single.paths[0].d).toBe('M0.00,50.00L100.00,0.00')
  })

  it('多系列各自独立：一条全无效不影响另一条的连续性', () => {
    const { paths } = chartGeometry(
      [
        { t: 0, up: 1, down: null },
        { t: 1000, up: 2, down: null },
        { t: 2000, up: 3, down: null },
      ],
      [{ key: 'up' }, { key: 'down' }],
      { ...opts, max: 3 },
    )

    const up = paths.find((p) => p.key === 'up')
    const down = paths.find((p) => p.key === 'down')
    expect(countOf(up.d, 'M')).toBe(1)
    expect(countOf(up.d, 'L')).toBe(2)
    expect(down.d).toBe('')
  })
})
