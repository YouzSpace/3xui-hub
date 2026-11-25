<script setup>
/**
 * Invite — 邀请好友（二级页面）。
 * 我的邀请码（大字 + 复制 + 下次刷新倒计时）/ 累计返佣 + 已邀请 / 规则说明 / 我的兑换码列表。
 * 数据：userApi.myInvite()（GET /api/invite/me）+ userApi.myAffiliate()（GET /api/affiliate）。
 * 两个接口各自加载、各自失败：返佣挂了不能把邀请码一起拖成「加载失败」。
 */
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { userApi } from '@/api/user'
import UserIcon from '@/components/user/UserIcon.vue'
import ListGroup from '@/components/user/ListGroup.vue'
import { useToast } from '@/components/ui/Toast.vue'
import { formatYuan } from '@/utils/money'

const toast = useToast()

const loading = ref(true)
// loadError 非空表示真的失败（网络/500）。与「没开邀请功能」要分开显示 ——
// 原来不论哪种情况都显示「邀请码加载失败」，把真实原因盖住了。
const loadError = ref('')
const invite = ref(null)
const redeemCodes = ref([])
const copied = ref(false)

// 返佣（GET /api/affiliate）：与邀请码接口分开存，任一失败只影响自己那块
const aff = ref(null)
const affLoading = ref(true)
const affError = ref('')

// 倒计时每秒走一格
const now = ref(Date.now())
let ticker = null

const countdown = computed(() => {
  const at = invite.value?.next_refresh_at
  if (!at) return ''
  const diff = new Date(at).getTime() - now.value
  if (diff <= 0) return '即将刷新'
  const d = Math.floor(diff / 86400000)
  const h = Math.floor((diff % 86400000) / 3600000)
  const m = Math.floor((diff % 3600000) / 60000)
  const s = Math.floor((diff % 60000) / 1000)
  if (d > 0) return `${d} 天 ${h} 小时后刷新`
  if (h > 0) return `${h} 小时 ${m} 分后刷新`
  return `${m} 分 ${s} 秒后刷新`
})

const discountLabel = computed(() => {
  const d = Number(invite.value?.discount)
  if (!d) return ''
  return `${(d * 10).toFixed(1)} 折`
})

// 返佣比例：后端给 0~1 的小数（0.1 = 10%），展示成百分比；多余的小数位去掉
const ratePercent = computed(() => {
  const r = Number(aff.value?.rate)
  if (!Number.isFinite(r)) return '—'
  return `${Number((r * 100).toFixed(2))}%`
})

const refreshDate = computed(() => {
  const at = invite.value?.next_refresh_at
  if (!at) return '—'
  const d = new Date(at)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleDateString('zh-CN', { year: 'numeric', month: 'long', day: 'numeric' })
})

const codeDiscount = (d) => `${(Number(d) * 10).toFixed(1)} 折`

// 被邀请人加入日期（只到天；解析不了给占位符，不显示 Invalid Date）
const joinedDate = (iso) => {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleDateString('zh-CN')
}

const statusLabel = (s) => ({
  active: '未使用',
  used_up: '已使用',
  expired: '已过期',
  refreshed: '已失效',
}[s] || s)

// 我的兑换码：可用的正常展示；已使用/已过期/已失效的收进可折叠的历史区，
// 避免码一多把页面越拉越长（兑换码是拿来用的，用完的不该一直占屏）。
const showUsedGifts = ref(false)
const activeRedeemCodes = computed(() => redeemCodes.value.filter((c) => c.status === 'active'))
const usedRedeemCodes = computed(() => redeemCodes.value.filter((c) => c.status !== 'active'))

/**
 * 复制到剪贴板（三级降级）。
 * navigator.clipboard 只在安全上下文（https / localhost）且未被策略禁用时可用：
 * App WebView、iframe 内嵌、http 页面里它直接不存在或抛 NotAllowedError，
 * 所以失败时退到 textarea + document.execCommand('copy')。
 * 返回是否真的复制成功。
 */
const copyText = async (text) => {
  if (!text) return false
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text)
      return true
    } catch (e) {
      /* 被策略禁用：落到下方兜底 */
    }
  }
  const ta = document.createElement('textarea')
  ta.value = text
  ta.setAttribute('readonly', '')
  ta.style.position = 'fixed'
  ta.style.top = '-9999px'
  document.body.appendChild(ta)
  ta.select()
  ta.setSelectionRange(0, text.length) // iOS Safari 只有 select() 选不中
  let ok = false
  try {
    ok = document.execCommand('copy')
  } catch (e) {
    ok = false
  }
  ta.remove()
  return ok
}

const copyCode = async () => {
  const code = invite.value?.code
  if (!code) return
  if (!(await copyText(code))) {
    toast.error('复制失败，请手动选择复制')
    return
  }
  copied.value = true
  toast.success('邀请码已复制')
  setTimeout(() => { copied.value = false }, 1500)
}

const load = async () => {
  loading.value = true
  loadError.value = ''
  try {
    // 后端契约与老 PHP 一致：{ invite: {...}, redeem_codes: [...] }。
    // 2026-11-02 的教训：这个接口曾把 invite 对象拍平，前端拿不到 → 永远「加载失败」。
    const data = await userApi.myInvite()
    invite.value = data?.invite || null
    redeemCodes.value = data?.redeem_codes || []
  } catch (e) {
    // 不静默吞掉：把真实原因带给用户（能看出是网络还是服务端）
    invite.value = null
    redeemCodes.value = []
    loadError.value = e?.message || '请求失败'
  } finally {
    loading.value = false
  }
}

// 返佣数据单独拉：接口挂了只让返佣那块显示失败原因 + 重试，邀请码照常可用
const loadAffiliate = async () => {
  affLoading.value = true
  affError.value = ''
  try {
    aff.value = (await userApi.myAffiliate()) || null
  } catch (e) {
    aff.value = null
    affError.value = e?.message || '请求失败'
  } finally {
    affLoading.value = false
  }
}

onMounted(() => {
  load()
  loadAffiliate()
  ticker = setInterval(() => { now.value = Date.now() }, 1000)
})
onUnmounted(() => { if (ticker) clearInterval(ticker) })
</script>

<template>
  <div class="iv uu-slide-up">
    <!-- 我的邀请码 -->
    <section v-if="loading" class="iv-hero iv-hero--skeleton">
      <div class="iv-skel iv-skel--sm"></div>
      <div class="iv-skel iv-skel--lg"></div>
      <div class="iv-skel iv-skel--sm"></div>
    </section>

    <section v-else-if="invite" class="iv-hero">
      <div class="iv-hero-top">
        <p class="iv-hero-label">我的邀请码</p>
        <span v-if="discountLabel" class="iv-hero-badge">{{ discountLabel }}</span>
      </div>

      <!-- 邀请码可能还没生成（新用户/刚关再开）：此时不能显示一个空框 -->
      <!-- 后端 ensureInviteCode 保证首次访问就生成，正常不会走到空码；留兜底防万一 -->
      <button v-if="invite.code" type="button" class="iv-code uu-press" @click="copyCode">
        <span class="iv-code-text">{{ invite.code }}</span>
        <span class="iv-code-copy" :class="{ 'iv-code-copy--done': copied }">
          <UserIcon :name="copied ? 'check' : 'copy'" :size="16" />
          <span>{{ copied ? '已复制' : '复制' }}</span>
        </span>
      </button>
      <p v-else class="iv-hero-generating">邀请码还没有生成，稍后刷新本页即可看到</p>

      <p v-if="!invite.enabled" class="iv-hero-off">邀请机制当前已关闭，邀请码暂不可用</p>

      <div class="iv-hero-foot">
        <div class="iv-hero-stat">
          <p class="iv-hero-stat-k">下次刷新</p>
          <p class="iv-hero-stat-v">{{ refreshDate }}</p>
        </div>
        <div class="iv-hero-stat">
          <p class="iv-hero-stat-k">剩余</p>
          <p class="iv-hero-stat-v">{{ countdown || '—' }}</p>
        </div>
        <div class="iv-hero-stat">
          <p class="iv-hero-stat-k">已邀请成功</p>
          <p class="iv-hero-stat-v">{{ invite.invited_count ?? 0 }} 人</p>
        </div>
      </div>
    </section>

    <!-- 真失败：显示真实原因 + 重试按钮（不再只给一句「请稍后重试」） -->
    <section v-else class="iv-hero iv-hero--empty">
      <p class="iv-empty">邀请信息加载失败</p>
      <p v-if="loadError" class="iv-empty-reason">{{ loadError }}</p>
      <button type="button" class="iv-retry uu-press" @click="load">重新加载</button>
    </section>

    <!-- 累计返佣（GET /api/affiliate；与邀请码接口各自失败，互不影响） -->
    <section class="iv-rebate">
      <div class="iv-rebate-top">
        <p class="iv-rebate-label">累计返佣</p>
        <span class="iv-rebate-rate">返佣 {{ ratePercent }}</span>
      </div>

      <template v-if="affLoading">
        <div class="iv-skel iv-skel--lg"></div>
        <div class="iv-skel iv-skel--sm"></div>
      </template>
      <template v-else-if="affError">
        <p class="iv-rebate-amount iv-rebate-amount--dim">—</p>
        <p class="iv-empty-reason iv-empty-reason--left">返佣数据加载失败：{{ affError }}</p>
        <button type="button" class="iv-retry iv-retry--left uu-press" @click="loadAffiliate">重新加载</button>
      </template>
      <template v-else>
        <p class="iv-rebate-amount">{{ formatYuan(aff?.total_rebate ?? 0) }}</p>
        <p class="iv-rebate-hint">好友购买套餐后，按实付金额的 {{ ratePercent }} 返到你的账户余额</p>
      </template>
    </section>

    <!-- 已邀请（每行 = 打码邮箱 + 贡献返佣 + 加入时间）。列表只显示最近 30 天加入的人，
         「已邀请成功 N 人」是上方全量统计，不受影响。说明放标题右侧，不占列表空间。 -->
    <ListGroup v-if="!affError" label="已邀请">
      <template #label-extra>仅显示最近 30 天</template>
      <template v-if="affLoading">
        <div class="iv-invitee iv-invitee--skeleton">
          <div class="iv-skel iv-skel--sm"></div>
          <div class="iv-skel iv-skel--sm"></div>
        </div>
      </template>
      <template v-else-if="aff?.invitees?.length">
        <div v-for="it in aff.invitees" :key="it.user_id" class="iv-invitee">
          <div class="iv-invitee-row">
            <span class="iv-invitee-email">{{ it.email || '—' }}</span>
            <span class="iv-invitee-rebate">{{ formatYuan(it.rebate ?? 0) }}</span>
          </div>
          <p class="iv-invitee-time">{{ joinedDate(it.invited_at) }} 加入</p>
        </div>
      </template>
      <p v-else class="iv-empty iv-empty--inset">还没有邀请好友，把邀请码发给朋友试试</p>
    </ListGroup>

    <!-- 规则说明（精简版：只留用户用得上的，并把「兑换码怎么用」讲清楚） -->
    <ListGroup label="规则说明">
      <ul class="iv-rules">
        <li>把邀请码发给好友，好友下单可享 {{ discountLabel || '折扣' }} 优惠</li>
        <li>好友首次下单结算时，在「邀请码 / 兑换码」框填入邀请码即可；每人限用一次，用过永久生效，不能用自己或别人已用过的码</li>
        <li>好友付款成功后，你会获得一张你的专属兑换码，可在「商店」下单时填入抵扣</li>
        <li>好友每次购买套餐，你会按 {{ ratePercent }} 获得返佣，直接到账余额</li>
      </ul>
    </ListGroup>

    <!-- 我的兑换码：可用的正常展示；已使用/已过期折叠进历史，避免列表越拉越长 -->
    <ListGroup label="我的兑换码">
      <template v-if="activeRedeemCodes.length">
        <div v-for="c in activeRedeemCodes" :key="c.code" class="iv-gift">
          <div class="iv-gift-head">
            <span class="iv-gift-code">{{ c.code }}</span>
            <span class="iv-gift-status iv-gift-status--active">{{ statusLabel(c.status) }}</span>
          </div>
          <div class="iv-gift-meta">
            <span class="iv-gift-discount">{{ codeDiscount(c.discount) }}</span>
            <span class="iv-gift-exp">{{ c.expires_at ? `有效期至 ${new Date(c.expires_at).toLocaleDateString('zh-CN')}` : '永久有效' }}</span>
          </div>
          <p v-if="c.note" class="iv-gift-note">{{ c.note }}</p>
        </div>
      </template>
      <p v-else-if="!redeemCodes.length" class="iv-empty iv-empty--inset">还没有兑换码，邀请好友下单即可获得</p>
      <p v-else class="iv-empty iv-empty--inset">暂无可用兑换码（已使用 / 已过期的见下方）</p>

      <!-- 历史：已使用 / 已过期，默认收起 -->
      <template v-if="usedRedeemCodes.length">
        <button type="button" class="iv-gift-toggle uu-press" @click="showUsedGifts = !showUsedGifts">
          <span>已使用 / 已过期（{{ usedRedeemCodes.length }}）</span>
          <UserIcon :name="showUsedGifts ? 'chevronDown' : 'chevronRight'" :size="16" />
        </button>
        <div v-show="showUsedGifts" class="iv-gift-history">
          <div v-for="c in usedRedeemCodes" :key="c.code" class="iv-gift iv-gift--used">
            <div class="iv-gift-head">
              <span class="iv-gift-code iv-gift-code--dead">{{ c.code }}</span>
              <span class="iv-gift-status" :class="`iv-gift-status--${c.status}`">{{ statusLabel(c.status) }}</span>
            </div>
            <div class="iv-gift-meta">
              <span class="iv-gift-discount">{{ codeDiscount(c.discount) }}</span>
              <span class="iv-gift-exp">{{ c.expires_at ? `有效期至 ${new Date(c.expires_at).toLocaleDateString('zh-CN')}` : '永久有效' }}</span>
            </div>
          </div>
        </div>
      </template>
    </ListGroup>
  </div>
</template>

<style scoped src="@/assets/styles/pages/user/Invite.css"></style>
