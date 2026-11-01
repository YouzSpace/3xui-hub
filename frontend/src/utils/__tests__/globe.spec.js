import { describe, it, expect } from 'vitest'
import { aggregateRegions, regionCode, regionFlag, regionName } from '@/utils/globe'

/**
 * 地区工具函数的边界。
 *
 * 最要紧的一条：**UK 必须归一到 GB**。
 * 后端（NodeRegion）吐的是 UK（订阅的旗帜映射用这个），而地图数据和旗帜 emoji 都用 ISO 的 GB：
 *   - 不归一：地球找不到 UK 的中心点（节点不上球）；旗帜会拼出 🇺🇰 这种不存在的旗
 */

describe('regionCode', () => {
  it('两位字母原样返回（大写）', () => {
    expect(regionCode('hk')).toBe('HK')
    expect(regionCode('JP')).toBe('JP')
  })

  it('UK 归一到 GB', () => {
    expect(regionCode('UK')).toBe('GB')
    expect(regionCode('uk')).toBe('GB')
  })

  it('旗帜 emoji 反解成两位代码', () => {
    expect(regionCode('🇭🇰')).toBe('HK')
    expect(regionCode('🇬🇧')).toBe('GB')
  })

  it('识别不出返回 null', () => {
    expect(regionCode(null)).toBeNull()
    expect(regionCode('')).toBeNull()
    expect(regionCode('HKG')).toBeNull()
    expect(regionCode('中国香港')).toBeNull()
  })
})

describe('regionFlag', () => {
  it('常规地区拼出正确旗帜', () => {
    expect(regionFlag('HK')).toBe('🇭🇰')
    expect(regionFlag('JP')).toBe('🇯🇵')
  })

  it('UK 拼出英国旗而不是不存在的 🇺🇰', () => {
    expect(regionFlag('UK')).toBe('🇬🇧')
    expect(regionFlag('UK')).not.toBe('🇺🇰')
  })

  it('没有地区时返回空串', () => {
    expect(regionFlag(null)).toBe('')
    expect(regionFlag('')).toBe('')
  })
})

describe('regionName', () => {
  it('港澳用简称，不用 Intl 的全称', () => {
    expect(regionName('HK')).toBe('中国香港')
    expect(regionName('MO')).toBe('中国澳门')
  })

  it('其他地区走浏览器内置名称', () => {
    expect(regionName('JP')).toBe('日本')
    expect(regionName('GB')).toBe('英国')
  })

  it('空值给「未知地区」', () => {
    expect(regionName(null)).toBe('未知地区')
  })
})

describe('aggregateRegions', () => {
  it('UK 节点归到 GB 一组，不会单独成组', () => {
    const regions = aggregateRegions([
      { status: 'online', region: 'UK' },
      { status: 'offline', region: 'GB' },
    ])

    expect(regions).toEqual([{ code: 'GB', total: 2, online: 1 }])
  })

  it('没有地区的节点不计入', () => {
    expect(aggregateRegions([{ status: 'online', region: null }, { status: 'online' }])).toEqual([])
  })

  it('在线多的排前面，同分按总数再按代码（顺序稳定）', () => {
    const regions = aggregateRegions([
      { status: 'online', region: 'JP' },
      { status: 'online', region: 'HK' },
      { status: 'offline', region: 'HK' },
      { status: 'offline', region: 'US' },
      { status: 'offline', region: 'DE' },
    ])

    expect(regions.map((r) => r.code)).toEqual(['HK', 'JP', 'DE', 'US'])
  })
})
