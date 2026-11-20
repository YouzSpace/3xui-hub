/**
 * 入站表单的全部状态：默认值、协议/传输/安全层联动、编辑回填、校验与提交。
 *
 * 只负责「表单」这一件事：列表加载、勾选、弹窗开关在页面容器，
 * 证书库在 useInboundCerts，Reality 目标检测在 useInboundScan。
 */
import { ref, computed, watch } from 'vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'
import {
  NETWORK_ALL,
  REALITY_NETWORKS,
  emptyForm,
  formFromInbound,
} from '@/composables/inboundFormModel'
import { buildInboundPayload, validateInboundForm } from '@/composables/inboundPayload'
import { useInboundCerts } from '@/composables/useInboundCerts'

/**
 * @param {{
 *   nodeId: import('vue').Ref,
 *   onSaved: () => Promise<void>,
 * }} deps  onSaved 在提交成功后调用（页面据此刷新列表）
 */
export function useInboundForm({ nodeId, onSaved }) {
  const toast = useToast()
  const modalOpen = ref(false)
  const editing = ref(null)
  const saving = ref(false)
  const saved = ref(false)
  const generating = ref(false)

  const form = ref(emptyForm())
  // 高级参数折叠面板（新建/编辑时不打扰）
  const advOpen = ref(false)

  const isReality = computed(() => form.value.protocol === 'vless' && form.value.security === 'reality')

  // ── 地址族（issue #88）─────────────────────────────────────────
  // listen 一直是字段（后端 schema 里就有），但以前前端固定传 0.0.0.0，用户无从选择：
  // 一台机器有 v6 时入站只能绑 v4，v6 用户拿不到入口。这里把它暴露成「地址族」下拉。
  //
  // 老数据兼容：listen 是这两个值之外的内容（历史手工值 / 具体 IP / unix socket）
  // 时，额外给一个「保留原值」选项 —— 未动的老入站提交后仍是原值，不会被悄悄改写成 0.0.0.0。
  const ADDRESS_FAMILIES = [
    { label: 'IPv4（0.0.0.0，默认）', value: '0.0.0.0' },
    { label: 'IPv6（::，同时接受 IPv4-mapped）', value: '::' },
  ]
  const listenOptions = computed(() => {
    const cur = form.value.listen
    const known = ADDRESS_FAMILIES.some((o) => o.value === cur)
    const base = known || !cur ? ADDRESS_FAMILIES : [...ADDRESS_FAMILIES, { label: `保留原值（${cur}）`, value: cur }]
    // 「自定义」= 绑定地址不动，单独指定订阅链接里给客户端用的地址（CDN 域名 / 另一个 IP）。
    return [...base, { label: '自定义（客户端连接地址，如 CDN 域名）', value: 'custom' }]
  })
  // 「自定义客户端连接地址」是一个独立的编辑态：选了就显示输入框，值存在 form.client_host。
  // 不能用 client_host 是否为空反推 —— 选了但还没填时会立刻弹回原选项，输入框根本出不来。
  const customAddress = ref(false)
  const listenFamily = computed({
    get: () => (customAddress.value ? 'custom' : form.value.listen),
    set: (v) => {
      if (v === 'custom') {
        customAddress.value = true // 绑定地址保持不变，只打开下面的自定义地址输入
        return
      }
      customAddress.value = false
      form.value.client_host = ''
      form.value.listen = v
    },
  })

  const isTls = computed(() => form.value.security === 'tls')
  const isWs = computed(() => form.value.network === 'ws')
  const isXhttp = computed(() => form.value.network === 'xhttp')
  const isHttpupgrade = computed(() => form.value.network === 'httpupgrade')
  const isSs = computed(() => form.value.protocol === 'shadowsocks')
  // SS-2022（2022-blake3-aes-*）：服务端密钥必须是 base64 固定长度（aes-256 32 字节 /
  // aes-128 16 字节），留空或长度不对后端会自动生成一把合法的。所以这一档用明文展示：
  // 它本来就明文出现在每个用户的订阅链接里（ss://method:服务端PSK:用户PSK），
  // 生成后的值管理员要能看到/复制，否则没法核对。
  const ssIs2022 = computed(() => String(form.value.ss_method || '').startsWith('2022-blake3-'))
  // 传统代理（socks / http）：单组账号密码，没有用户概念
  const isSocks = computed(() => form.value.protocol === 'socks')
  const isHttp = computed(() => form.value.protocol === 'http')
  const isProxyProto = computed(() => isSocks.value || isHttp.value)
  // tunnel（dokodemo-door）：把流量原样转给固定目标，L4 网络在 settings.network
  const isTunnel = computed(() => form.value.protocol === 'tunnel')
  // wireguard：UDP 监听、自带加密，服务端私钥由面板生成
  const isWg = computed(() => form.value.protocol === 'wireguard')
  // 传输下拉：wireguard / tunnel 没有可选的传输层（wireguard 固定 UDP，tunnel 走 L4 直转）
  const hideTransport = computed(() => isWg.value || isTunnel.value)
  // 安全层下拉：上面两种 + 传统代理都是「无附加安全层」
  const hideSecurity = computed(() => hideTransport.value || isProxyProto.value)
  // hysteria2：QUIC 传输，强制 TLS（跳过了 TLS 就没有加密层），证书走证书库引用
  const isHy = computed(() => form.value.protocol === 'hysteria')

  // brutal 带宽只在这两种拥塞算法下有意义（后端同样只在这两种下写入 quicParams）
  const isHyBrutal = computed(
    () => isHy.value && ['brutal', 'force-brutal'].includes(form.value.hy_congestion),
  )

  // 传输选项：Reality 下仅保留受支持项（tcp/xhttp/grpc）；hy2 固定 hysteria（QUIC）
  const networkOptions = computed(() => {
    if (isHy.value) return [{ label: 'Hysteria（QUIC）', value: 'hysteria' }]
    if (isReality.value) return NETWORK_ALL.filter((o) => REALITY_NETWORKS.includes(o.value))
    return NETWORK_ALL
  })

  // 安全层选项随协议变化（reality 仅 vless；ss 固定无；hy2 固定 tls）
  const securityOptions = computed(() => {
    if (isHy.value) return [{ label: 'TLS（HY2 强制）', value: 'tls' }]
    if (isSs.value) return [{ label: '无（SS 自带加密）', value: 'none' }]
    if (form.value.protocol === 'vless') {
      return [
        { label: 'Reality（推荐）', value: 'reality' },
        { label: 'TLS', value: 'tls' },
        { label: '无', value: 'none' },
      ]
    }
    return [
      { label: 'TLS', value: 'tls' },
      { label: '无', value: 'none' },
    ]
  })

  // Reality 仅 tcp/xhttp/grpc；ss 固定 tcp+none；hy2 固定 hysteria+tls；
  // socks/http/tunnel/wireguard 没有安全层也没有用户 —— 切换协议/安全时自动收敛，
  // 避免残留非法组合被后端打回
  watch(
    () => [form.value.protocol, form.value.security],
    () => {
      if (isReality.value && !REALITY_NETWORKS.includes(form.value.network)) form.value.network = 'tcp'
      if (isSs.value) {
        form.value.network = 'tcp'
        form.value.security = 'none'
      }
      if (isHy.value) {
        form.value.network = 'hysteria'
        form.value.security = 'tls'
      }
      if (isWg.value || isTunnel.value) {
        form.value.network = 'tcp'
        form.value.security = 'none'
      }
      if (isProxyProto.value) form.value.security = 'none'
    },
  )

  // 证书库（引用下拉 / PEM 回填 / 远程签发）
  const certs = useInboundCerts({ nodeId, form, isHy })
  const {
    loadCertificates,
    pemCertId,
    bestPemCert,
  } = certs

  const openCreate = () => {
    editing.value = null
    form.value = emptyForm()
    customAddress.value = false
    advOpen.value = false
    loadCertificates().then(() => {
      pemCertId.value = bestPemCert()?.id ?? null
    })
    modalOpen.value = true
  }

  const openEdit = (ib) => {
    editing.value = ib
    customAddress.value = !!String(ib.client_host || '').trim()
    form.value = formFromInbound(ib)
    advOpen.value = false
    loadCertificates().then(() => {
      // 编辑已有入站：优先回显它引用的那张（续签就是按这个 id 找节点的）；
      // 没引用（老的内联 PEM 用法）才按域名挑一张。
      pemCertId.value = form.value.tls_cert_id ?? bestPemCert()?.id ?? null
    })
    modalOpen.value = true
  }

  const generateReality = async () => {
    generating.value = true
    try {
      const res = await adminApi.generateRealityKeypair(nodeId.value)
      form.value.private_key = res.private_key
      form.value.public_key = res.public_key
      form.value.short_ids = res.short_id
      form.value.dest = res.dest
      form.value.server_names = res.server_name
      toast.success('已生成新的 Reality 密钥对')
    } catch (e) {
      toast.error(e.message || '生成失败')
    } finally {
      generating.value = false
    }
  }

  const save = async (forceServerNames = false) => {
    if (!nodeId.value) return
    const invalid = validateInboundForm(form, {
      customAddress: customAddress.value,
      isTls: isTls.value,
      isHy: isHy.value,
      isReality: isReality.value,
      pemCertId: pemCertId.value,
    })
    if (invalid) {
      toast.error(invalid.error)
      if (invalid.openAdvanced) advOpen.value = true
      return
    }
    saving.value = true
    try {
      const payload = buildInboundPayload(
        form,
        // 这里传的是 computed 本身（不是 .value）：buildInboundPayload 内部按 ref 读
        // （flags.isHy.value ...），传布尔会让所有分层字段整块消失（只剩基础字段）。
        {
          isReality,
          isWs,
          isXhttp,
          isHttpupgrade,
          isTls,
          isSs,
          isProxyProto,
          isTunnel,
          isWg,
          isHy,
        },
        { forceServerNames, pemCertId },
      )

      if (editing.value) {
        await adminApi.updateXrayInbound(nodeId.value, editing.value.id, payload)
        toast.success('入站已更新（节点将秒级生效）')
      } else {
        await adminApi.createXrayInbound(nodeId.value, payload)
        toast.success('入站已创建（节点将秒级生效）')
      }
      // 成功反馈：按钮短暂变对勾（Submit Morph 成功段），再关弹窗
      saved.value = true
      setTimeout(() => {
        saved.value = false
        modalOpen.value = false
      }, 900)
      await onSaved()
    } catch (e) {
      // issue #57：后端拦下「serverNames 与 dest 不是同一站点」的配置。
      // 少数场景是合理的（dest 是 CDN，一张证书覆盖多个域名），所以给确认通道，不写死拒绝。
      if (e.code === 422 && /serverNames/i.test(String(e.message || ''))) {
        if (confirm(`${e.message}\n\n确认这些域名确实由 dest 的同一张证书覆盖，继续保存？`)) {
          return save(true)
        }
        // 取消：把原因留在屏幕上，别静默什么都不发生
        toast.error(e.message)
        return
      }
      toast.error(e.message || '保存失败')
    } finally {
      saving.value = false
    }
  }

  return {
    ...certs,
    form,
    advOpen,
    customAddress,
    modalOpen,
    editing,
    saving,
    saved,
    generating,
    isReality,
    listenOptions,
    listenFamily,
    isTls,
    isWs,
    isXhttp,
    isHttpupgrade,
    isSs,
    ssIs2022,
    isSocks,
    isHttp,
    isProxyProto,
    isTunnel,
    isWg,
    hideTransport,
    hideSecurity,
    isHy,
    isHyBrutal,
    networkOptions,
    securityOptions,
    openCreate,
    openEdit,
    generateReality,
    save,
  }
}
