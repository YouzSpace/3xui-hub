import { describe, it, expect } from 'vitest'
import { formatBytes, formatSpeed } from '@/utils/format'

/**
 * formatBytes / formatSpeed 的边界契约。
 *
 * 核心回归：**0 < 字节值 < 1** 的输入（低流量节点、空闲时段的亚字节速率）
 * 曾因单位索引为负（units[-1] === undefined）拼出「589 undefined/s」——
 * 监控页流量曲线的 Y 轴与图例实测触发。索引必须双向钳制。
 */

describe('formatBytes', () => {
  it('缺失值一律占位「—」，绝不兜底成 0', () => {
    expect(formatBytes(null)).toBe('—')
    expect(formatBytes(undefined)).toBe('—')
    expect(formatBytes(NaN)).toBe('—')
    expect(formatBytes(Infinity)).toBe('—')
    expect(formatBytes(-1)).toBe('—')
  })

  it('常规换算不受影响', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(589)).toBe('589 B')
    expect(formatBytes(1023)).toBe('1023 B')
    expect(formatBytes(1024)).toBe('1.0 KB')
    expect(formatBytes(1024 * 1024 * 1.5)).toBe('1.5 MB')
  })

  it('回归：0 < value < 1 不得出现 undefined（曾输出「589 undefined」）', () => {
    for (const value of [0.575, 0.4, 0.999, 1e-6]) {
      const text = formatBytes(value)
      expect(text).not.toContain('undefined')
      expect(text).toMatch(/^\d+ B$/)
    }

    // 四舍五入到 B 级整数：0.575 → 1 B；0.4 → 0 B
    expect(formatBytes(0.575)).toBe('1 B')
    expect(formatBytes(0.4)).toBe('0 B')
  })
})

describe('formatSpeed', () => {
  it('常规速率', () => {
    expect(formatSpeed(589)).toBe('589 B/s')
    expect(formatSpeed(1024)).toBe('1.0 KB/s')
  })

  it('缺失值占位', () => {
    expect(formatSpeed(null)).toBe('—')
    expect(formatSpeed(-1)).toBe('—')
  })

  it('回归：亚字节速率不得出现 undefined（曾输出「589 undefined/s」）', () => {
    const text = formatSpeed(0.575)
    expect(text).not.toContain('undefined')
    expect(text).toBe('1 B/s')
  })
})
