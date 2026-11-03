import { describe, it, expect } from 'vitest'
import { centsToYuan, formatYuan, formatSignedYuan, yuanToCents } from '@/utils/money'

/**
 * 金额换算的口径守卫。
 *
 * 这些断言盯的是「一分钱级别」的正确性，不是格式好看：
 *  - 分/元换算必须整数运算（1.1 元不能变成 109 分）
 *  - 非法输入必须返回 null，绝不能兜底成 0 —— 后端拿 0 会写出「0 元充值单」
 *  - 流水符号：正数 +、负数 -、0 不带符号
 */
describe('centsToYuan', () => {
  it('整数分换算成两位小数的元', () => {
    expect(centsToYuan(0)).toBe('0.00')
    expect(centsToYuan(1)).toBe('0.01')
    expect(centsToYuan(99)).toBe('0.99')
    expect(centsToYuan(100)).toBe('1.00')
    expect(centsToYuan(5000)).toBe('50.00')
    expect(centsToYuan(123456)).toBe('1234.56')
  })

  it('负数保留符号，不把小数的负号丢在整数位上', () => {
    expect(centsToYuan(-1)).toBe('-0.01')
    expect(centsToYuan(-1234)).toBe('-12.34')
  })

  it('非数值输入按 0 处理（展示层兜底，写接口不用它）', () => {
    expect(centsToYuan(null)).toBe('0.00')
    expect(centsToYuan(undefined)).toBe('0.00')
    expect(centsToYuan('abc')).toBe('0.00')
  })
})

describe('formatYuan / formatSignedYuan', () => {
  it('余额统一带 ¥ 前缀', () => {
    expect(formatYuan(5000)).toBe('¥50.00')
    expect(formatYuan(0)).toBe('¥0.00')
  })

  it('流水金额带方向符号，0 不带符号', () => {
    expect(formatSignedYuan(5000)).toBe('+¥50.00')
    expect(formatSignedYuan(-5000)).toBe('-¥50.00')
    expect(formatSignedYuan(0)).toBe('¥0.00')
    expect(formatSignedYuan(-1)).toBe('-¥0.01')
  })
})

describe('yuanToCents', () => {
  it('整数元 / 一位小数 / 两位小数都能换算', () => {
    expect(yuanToCents('10')).toBe(1000)
    expect(yuanToCents('10.5')).toBe(1050)
    expect(yuanToCents('10.05')).toBe(1005)
    expect(yuanToCents('0.01')).toBe(1)
    expect(yuanToCents(' 99.99 ')).toBe(9999)
  })

  it('浮点陷阱：1.1 / 0.29 / 8.7 必须落在整分上', () => {
    expect(yuanToCents('1.1')).toBe(110)
    expect(yuanToCents('0.29')).toBe(29)
    expect(yuanToCents('8.7')).toBe(870)
  })

  it('非法输入返回 null，不兜底成 0', () => {
    expect(yuanToCents('')).toBeNull()
    expect(yuanToCents('0.001')).toBeNull()
    expect(yuanToCents('1.234')).toBeNull()
    expect(yuanToCents('abc')).toBeNull()
    expect(yuanToCents('-5')).toBeNull()
    expect(yuanToCents('1,000')).toBeNull()
    expect(yuanToCents(null)).toBeNull()
    expect(yuanToCents(undefined)).toBeNull()
  })

  it('超出安全整数范围判非法', () => {
    expect(yuanToCents('99999999999999999')).toBeNull()
  })
})
