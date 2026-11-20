/**
 * 入站表单的「TLS 证书库」部分：列表、引用下拉、域名匹配标注、PEM 回填、远程签发。
 *
 * 为什么单拎出来：证书口径（域名覆盖 / 到期 / 证书+私钥齐全）在 HY2 引用、TLS 内联 PEM、
 * 签发回填三处复用，且都要和后端同口径，散在页面里最容易各改各的。
 */
import { ref, computed, watch } from 'vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'
import {
  bareHost,
  certCoversHost,
  certExpired,
  certSourceLabel,
  latestExpiry,
} from '@/composables/inboundCertUtils'

/**
 * @param { nodeId: import('vue').Ref, form: import('vue').Ref, isHy: import('vue').ComputedRef } deps
 */
export function useInboundCerts({ nodeId, form, isHy }) {
  const toast = useToast()

  // ── TLS 证书库（HY2 的证书引用源）──────────────────────────────
  // HY2 强制 TLS：入站只存证书库 id，私钥留在库里（不进 stream_settings）。
  // 只在这里读取列表；新增/删除走证书库页面，避免把私钥表单塞进入站弹窗。
  const certificates = ref([])

  // 当前节点的主机名：HY2 的证书是按「入站 SNI → 节点域名」匹配的（后端 autoPickHysteriaCert
  // 就是这两级），前端要按同一个域名给证书条目做匹配标注与自动填充。
  // 节点选择器只回 id，这里按 id 再取一次拿 host。
  const nodeHost = ref('')

  const loadNodeHost = async () => {
    nodeHost.value = ''
    if (!nodeId.value) return
    try {
      const res = await adminApi.getNode(nodeId.value)
      nodeHost.value = bareHost(res?.host)
    } catch {
      // 拿不到节点域名时不做域名匹配标注：宁可少标，也不误标成「不匹配」
      nodeHost.value = ''
    }
  }

  // 要匹配的目标域名：优先表单里的 SNI（HY2 握手就按它），其次节点 host。
  const certTargetHost = computed(() => {
    const sni = String(form.value.tls_server_name || '').trim().toLowerCase()
    return sni || nodeHost.value
  })

  // 证书能否覆盖目标域名。目标域名或证书域名任一未知时返回 true：
  // 判断不了就不拦人（后端同样在两边都空时才兜底挑到期最晚的一张）。
  const certFits = (c) => {
    const target = certTargetHost.value
    const dom = String(c?.domain || '').trim()
    if (!target || !dom) return true
    return certCoversHost(dom, target)
  }

  const certOptions = computed(() => [
    { label: '不引用（内核用其它方式提供证书）', value: null },
    ...certificates.value.map((c) => {
      const tags = []
      if (c.domain) tags.push(c.domain)
      const source = certSourceLabel(c.source)
      if (source) tags.push(source)
      if (c.not_after) tags.push(`${String(c.not_after).slice(0, 10)} 到期`)
      if (certExpired(c)) tags.push('已过期')
      // 域名覆盖不到的条目置灰：选了客户端按 SNI 握手必失败
      // （后端自动挑证书同样拒绝拿别的域名凑数，线上真踩过「入站看着正常、实际连不上」）
      const fits = certFits(c)
      if (!fits) tags.push('域名不匹配')
      return {
        label: `${c.name}${tags.length ? `（${tags.join(' · ')}）` : ''}`,
        value: c.id,
        disabled: !fits,
      }
    }),
  ])

  // 证书库里有没有「覆盖当前 serverName」的证书（HY2 下拉用来提示「先签发」）。
  const hasFittingCert = computed(() => certOptions.value.some((o) => o.value && !o.disabled))

  // 选中项自己可能已经不匹配了（先选证书、后改 SNI）：显式点名，别让人以为一切正常
  const selectedCertProblem = computed(() => {
    const id = form.value.tls_cert_id
    if (!id) return ''
    const c = certificates.value.find((x) => Number(x.id) === Number(id))
    if (!c) return ''
    const bad = []
    if (!certFits(c)) bad.push(`证书域名（${c.domain}）覆盖不到 ${certTargetHost.value}`)
    if (certExpired(c)) bad.push('证书已过期')
    return bad.length ? `当前选中的证书用不了：${bad.join('；')}` : ''
  })

  // 「自动填充证书」：① 节点 host 是域名时预填进 SNI（HY2 的 serverName 就是握手用的域名），
  // ② 从证书库挑一张域名覆盖目标域名、未过期、证书+私钥齐全的条目填进下拉。
  // 为什么让前端挑而不是留给后端 autoPick：后端只在请求没带 tls_cert_id 时才自动挑，
  // 表单里显式选中才能让用户当场看见「用的是哪一张」，不会出现选了不引用、面板成功却握不上手。
  const autoFillCert = () => {
    if (!isHy.value) return
    if (!certificates.value.length) {
      toast.error('证书库还是空的：请先到「TLS 证书库」添加一张再回来')
      return
    }
    const host = nodeHost.value
    const hostIsIp = /^\d{1,3}(\.\d{1,3}){3}$/.test(host) || host.includes(':')
    if (!String(form.value.tls_server_name || '').trim() && host && !hostIsIp) {
      form.value.tls_server_name = host
    }
    const target = certTargetHost.value
    const usable = certificates.value.filter((c) => c.has_cert !== false && c.has_key !== false && !certExpired(c))
    if (!usable.length) {
      toast.error('证书库里没有可用的证书：都已过期或缺少证书/私钥，请先到「TLS 证书库」补一张')
      return
    }
    if (!target) {
      // SNI 与节点 host 都空：没得匹配，退到最后到期的一张（与后端兜底一致），并说清原因
      const pick = latestExpiry(usable)
      form.value.tls_cert_id = pick.id
      toast.info(`没填 SNI、节点也没填域名：先按到期最晚选了 ${pick.name}，请把 serverName 补成证书域名`)
      return
    }
    const matched = usable.filter((c) => certCoversHost(c.domain, target))
    if (!matched.length) {
      toast.error(`证书库里没有覆盖 ${target} 的证书：先到「TLS 证书库」为该域名签/导一张，或手动选一张`)
      return
    }
    const pick = latestExpiry(matched)
    form.value.tls_cert_id = pick.id
    toast.success(`已填入证书：${pick.name}${pick.domain ? `（${pick.domain}）` : ''}`)
  }

  const loadCertificates = async () => {
    try {
      const res = await adminApi.listXrayCertificates()
      certificates.value = res?.certificates || []
    } catch (e) {
      certificates.value = []
    }
  }

  // ── 从证书库回填 TLS 入站的 PEM ────────────────────────────────
  // TLS 入站（vless/trojan）读的是内联 PEM，不认证书库 id，所以「引用证书库」对它等于手贴。
  // 这里补一条捷径：选中一张库里的证书 → 请求解密后的 cert_pem/key_pem → 填进两个框。
  // 私钥只在这一个 admin 请求里回传，不进列表载荷，也没落进 stream_settings（保存时按内联 PEM 走）。
  const pemCertId = ref(null)
  const pemFilling = ref(false)

  // 能回填的条目：证书+私钥齐全、没过期、域名覆盖得到目标域名（其余填了也握不上手）
  const pemPickable = computed(() =>
    certificates.value.filter(
      (c) => c.has_cert !== false && c.has_key !== false && !certExpired(c) && certFits(c),
    ),
  )

  // 回填用的下拉：不复用 certOptions（那份首项是「不引用」，回填必须真选一张）
  const pemCertOptions = computed(() =>
    certificates.value.map((c) => {
      const tags = []
      if (c.domain) tags.push(c.domain)
      if (c.not_after) tags.push(`${String(c.not_after).slice(0, 10)} 到期`)
      const complete = c.has_cert !== false && c.has_key !== false
      if (!complete) tags.push('缺证书或私钥')
      if (certExpired(c)) tags.push('已过期')
      if (!certFits(c)) tags.push('域名不匹配')
      return {
        label: `${c.name}${tags.length ? `（${tags.join(' · ')}）` : ''}`,
        value: c.id,
        disabled: !complete || certExpired(c) || !certFits(c),
      }
    }),
  )

  // 没手选时自动挑一张：域名能覆盖目标域名的条目里到期最晚的（与「自动填充证书」同口径）
  const bestPemCert = () => {
    const usable = pemPickable.value
    if (!usable.length) return null
    const target = certTargetHost.value
    const matched = target ? usable.filter((c) => certCoversHost(c.domain, target)) : usable
    return latestExpiry(matched.length ? matched : usable)
  }

  // 当前域名在证书库里有没有能用的证书：没有就留空并提示先签发，
  // 不要让一张域名不匹配的旧证书继续顶在框里（保存后客户端按 SNI 必握手失败）。
  const pemCertPlaceholder = computed(() =>
    pemPickable.value.length ? '选一张证书' : '当前域名没有可用证书，先点上方「签发证书」',
  )

  // 改了 serverName（或换了节点）之后，原来选中的证书可能已经覆盖不到新域名：清掉重选。
  // 覆盖不到时留着它只会误导人 —— 显示框里还是那张证书，实际连不上。
  watch(certTargetHost, () => {
    const stillFits = (id) => {
      if (!id) return true
      const c = certificates.value.find((x) => Number(x.id) === Number(id))
      return !!c && certFits(c)
    }
    if (!stillFits(pemCertId.value)) pemCertId.value = null
    if (!stillFits(form.value.tls_cert_id)) form.value.tls_cert_id = null
  })

  const fillTlsPemFromLibrary = async () => {
    if (pemFilling.value) return
    if (!certificates.value.length) {
      toast.error('证书库还是空的：先到「TLS 证书库」添加一张，或用下面的「签发证书」签一张')
      return
    }
    const id = pemCertId.value || bestPemCert()?.id
    if (!id) {
      toast.error('证书库里没有能填的证书：都缺证书/私钥、已过期，或域名覆盖不到当前 serverName')
      return
    }
    pemFilling.value = true
    try {
      const res = await adminApi.getXrayCertificatePem(id)
      const certPem = String(res?.cert_pem || '').trim()
      const keyPem = String(res?.key_pem || '').trim()
      if (!certPem || !keyPem) {
        toast.error('证书库没回完整 PEM（缺证书或私钥），请先到「TLS 证书库」补齐')
        return
      }
      form.value.tls_certificate = certPem
      form.value.tls_key = keyPem
      pemCertId.value = id
      const picked = certificates.value.find((x) => Number(x.id) === Number(id))
      toast.success(`已从证书库填入证书与私钥${picked?.name ? `（${picked.name}）` : ''}`)
    } catch (e) {
      toast.error(e.message || '读取证书失败')
    } finally {
      pemFilling.value = false
    }
  }

  // ── 远程签发证书（面板下发 cert_issue，节点跑 acme.sh，成功后写回证书库）──
  // 签发域名跟 TLS serverName 走：证书必须覆盖客户端握手用的 SNI，否则签了也握不上手。
  // 只有 serverName 留空时才回落节点 host（老行为）；IP 一律不签（Let's Encrypt 不发 IP 证书）。
  const certSignDomain = computed(() => bareHost(form.value.tls_server_name) || nodeHost.value)
  const certSignDomainIsIp = computed(() => {
    const h = certSignDomain.value
    return /^\d{1,3}(\.\d{1,3}){3}$/.test(h) || (h.includes(':') && !h.includes('.'))
  })
  const issuingCert = ref(false)

  const issueNodeCert = async () => {
    if (issuingCert.value) return
    const domain = certSignDomain.value
    if (!nodeId.value || !domain) {
      toast.error('先选节点，并在 TLS serverName 里填要签的域名（或把节点地址填成域名）')
      return
    }
    if (certSignDomainIsIp.value) {
      toast.error(`签发域名是 IP（${domain}）：Let's Encrypt 不签 IP，请把 TLS serverName 改成解析到本节点的域名`)
      return
    }
    issuingCert.value = true
    try {
      const res = await adminApi.issueXrayCertificate({
        node_id: Number(nodeId.value),
        domain,
        method: 'http-01',
      })
      await loadCertificates()
      // 新条目 id 优先取回执；后端只回路径时，按域名在刷新后的库里取到期最晚的那张
      let newId = res?.certificate?.id
      if (!newId) {
        const hit = certificates.value.filter((c) => certCoversHost(c.domain, domain))
        newId = hit.length ? latestExpiry(hit).id : null
      }
      if (newId) {
        // HY2 认证书 id；TLS 入站只把它选进「证书库条目」（引用），不自动灌 PEM ——
        // 默认保存的是引用；要用内联 PEM 的人自己点「从证书库填充 PEM」。
        if (isHy.value) form.value.tls_cert_id = newId
        else pemCertId.value = newId
      }
      const notAfter = res?.certificate?.not_after || res?.not_after
      toast.success(
        `已签发 ${domain} 的证书${notAfter ? `（到期 ${String(notAfter).slice(0, 10)}）` : ''}，已写入证书库`,
      )
    } catch (e) {
      toast.error(e.message || '签发失败')
    } finally {
      issuingCert.value = false
    }
  }

  return {
    certificates,
    nodeHost,
    loadNodeHost,
    certTargetHost,
    certFits,
    certOptions,
    hasFittingCert,
    selectedCertProblem,
    autoFillCert,
    loadCertificates,
    pemCertId,
    pemFilling,
    pemPickable,
    pemCertOptions,
    bestPemCert,
    pemCertPlaceholder,
    fillTlsPemFromLibrary,
    certSignDomain,
    certSignDomainIsIp,
    issuingCert,
    issueNodeCert,
  }
}
