import { computed, ref, onActivated, onDeactivated } from 'vue'
import { useRoute, useRouter } from 'vue-router'

/**
 * 后台列表页分页状态（纯展示层分页，每页 50 条，见 prompts/pagination-final-2026-10-01）。
 *
 * - 页码走路由 query.page：刷新 / 切页回来不丢页
 * - 翻页前 abort 上一个未完成请求，防止老响应覆盖新页数据
 * - 越界页码（如 ?page=999）由后端夹回有效页，这里把 URL 校正到实际页
 */
export function usePagedList() {
  const route = useRoute()
  const router = useRouter()

  const loading = ref(false)
  const loadError = ref('')
  const pagination = ref({ current_page: 1, last_page: 1, total: 0, per_page: 50 })

  // keep-alive 退场后本实例还活着，而 route.query.page 是全局的：别的列表页翻页，
  // 这里的 watch(page, load) 也会跟着发请求。短列表（nodes 60 条 / plans 100 条 → 只有 2 页）
  // 被问到第 4 页会被后端夹回 last_page=2，再走下面的 URL 校正把地址栏改写成 ?page=2
  // —— 就是「翻到第 4 页自己跳回第 2 页」。退场期间既不拉数据、也不许改 URL。
  const active = ref(true)
  onActivated(() => { active.value = true })
  onDeactivated(() => { active.value = false })

  let controller = null

  const page = computed(() => {
    const p = parseInt(route.query.page, 10)
    return Number.isFinite(p) && p >= 1 ? p : 1
  })

  const hasPrev = computed(() => page.value > 1)
  const hasNext = computed(() => page.value < pagination.value.last_page)

  const goToPage = (next) => {
    if (next < 1 || next === page.value) return
    router.push({ query: { ...route.query, page: String(next) } })
  }

  const prev = () => goToPage(page.value - 1)
  const next = () => goToPage(page.value + 1)

  /**
   * 拉取当前页。fetcher(params, config) 必须把 config（含 signal）透传给 axios。
   * 成功返回列表数组（分页元信息在 res.pagination）；被取消或失败返回 null 并写 loadError。
   *
   * extra 会合并进请求参数（搜索词等）。搜索态要跟页码一起进 URL query，
   * 这样刷新 / 分享链接 / 翻页回来都不丢搜索条件。
   */
  const fetchPage = async (fetcher, extra = null) => {
    if (!active.value) return null
    controller?.abort()
    controller = new AbortController()
    const signal = controller.signal
    const requested = page.value

    loading.value = true
    loadError.value = ''
    try {
      const res = await fetcher({ page: requested, ...(extra || {}) }, { signal })
      // 请求期间可能已被 keep-alive 退场：响应不落地，更不许改 URL
      if (signal.aborted || !active.value) return null

      pagination.value = {
        current_page: res?.pagination?.current_page ?? requested,
        last_page: res?.pagination?.last_page ?? 1,
        total: res?.pagination?.total ?? (Array.isArray(res) ? res.length : 0),
        per_page: res?.pagination?.per_page ?? 50,
      }

      // 后端已把越界页码夹回有效页；URL 跟着校正，别让地址栏停在 ?page=999。
      // 只校正「地址栏还停在本次请求的页码」的情况，慢响应不能把用户已翻到的页改回去。
      const corrected = pagination.value.current_page
      if (corrected !== requested && page.value === requested) {
        router.replace({ query: { ...route.query, page: String(corrected) } })
      }

      return res
    } catch (e) {
      if (signal.aborted || !active.value) return null
      // 失败原因直接给出来，别再只报一句笼统的「加载失败」
      loadError.value = e?.message || '请求失败，请检查网络后重试'
      return null
    } finally {
      if (!signal.aborted) loading.value = false
    }
  }

  return { page, loading, loadError, pagination, hasPrev, hasNext, prev, next, fetchPage }
}
