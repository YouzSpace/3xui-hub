/**
 * 证书库相关的纯函数（无 Vue 依赖）。
 *
 * 三个口径必须与后端一致（错了会让「面板看着正常、客户端握手必失败」）：
 *   bareHost         —— 与后端 nodeHostName 同口径
 *   certCoversHost   —— 与后端 certDomainMatches 同口径
 *   latestExpiry     —— 同域名多张时挑到期最晚的一张
 */

// 与后端 nodeHostName 同口径：去 scheme / 路径 / 端口 / IPv6 方括号，转小写裸主机名。
export const bareHost = (raw) => {
  let h = String(raw || '').trim()
  if (!h) return ''
  const scheme = h.indexOf('//')
  if (scheme >= 0) h = h.slice(scheme + 2)
  const cut = h.search(/[/?]/)
  if (cut >= 0) h = h.slice(0, cut)
  if (h.startsWith('[')) {
    const end = h.indexOf(']')
    if (end > 0) h = h.slice(1, end)
  } else if (h.split(':').length === 2) {
    h = h.slice(0, h.lastIndexOf(':'))
  }
  return h.toLowerCase()
}

// 与后端 certDomainMatches 同口径：完全一致，或 *.example.com 覆盖 a.example.com 与 example.com 本身
export const certCoversHost = (certDomain, host) => {
  const d = String(certDomain || '').trim().toLowerCase()
  const h = String(host || '').trim().toLowerCase()
  if (!d || !h) return false
  if (d === h) return true
  if (d.startsWith('*.')) {
    const base = d.slice(2)
    return h === base || h.endsWith(`.${base}`)
  }
  return false
}

// 到期判定：not_after 缺失或解析不出来按「不知道」处理，不标已过期
export const certExpired = (c) => {
  if (!c?.not_after) return false
  const t = Date.parse(c.not_after)
  return Number.isFinite(t) && t <= Date.now()
}

// source 是后端派生字段：local-dir = 本机证书目录自动刷新，manual = 手工导入/续签后需重导
export const certSourceLabel = (source) => {
  if (source === 'local-dir') return '本机目录'
  if (source === 'manual') return '手工导入'
  return ''
}

// 取到期最晚的一张（同域名多张时：换证后旧证还留在库里）
export const latestExpiry = (list) =>
  list.reduce((best, c) => {
    const t = Date.parse(c?.not_after || '')
    const b = Date.parse(best?.not_after || '')
    return (Number.isFinite(t) ? t : 0) > (Number.isFinite(b) ? b : 0) ? c : best
  })
