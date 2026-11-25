<script setup>
/**
 * 仪表盘（新 UI）：流量卡 / 订阅管理 / 使用文档入口 / 节点列表
 * 数据全部来自现有真实接口（/api/me、/api/nodes、/api/site-config）。
 */
import { ref, computed, onMounted, onUnmounted } from "vue";
import { useRouter } from "vue-router";
import { useAuthStore } from "@/stores/auth";
import { userApi } from "@/api/user";
import { useToast } from "@/components/ui/Toast.vue";
import UserIcon from "@/components/user/UserIcon.vue";
import SegControl from "@/components/user/SegControl.vue";
import QrSheet from "@/components/user/QrSheet.vue";
import BottomSheet from "@/components/user/BottomSheet.vue";
import { beginPayWindow, gotoPay, cancelPayWindow } from "@/utils/paywindow";

const auth = useAuthStore();
const toast = useToast();
const router = useRouter();

const user = computed(() => auth.user || {});
const nodes = ref([]);

const fmtBytes = (b) => {
  if (!b || b < 0 || !Number.isFinite(b)) return "0 B";
  const u = ["B", "KB", "MB", "GB", "TB", "PB", "EB"];
  const raw = Math.floor(Math.log(b) / Math.log(1024))
  const i = Math.max(0, Math.min(raw, u.length - 1));
  // 数字与单位之间不空格（用户 2026-11-06 定）：48.82GB 而不是 48.82 GB
  return `${(b / Math.pow(1024, i)).toFixed(i ? 2 : 0)}${u[i]}`;
};

const regionFlags = {
  香港: "🇭🇰",
  HK: "🇭🇰",
  "Hong Kong": "🇭🇰",
  台湾: "🇹🇼",
  TW: "🇹🇼",
  Taiwan: "🇹🇼",
  日本: "🇯🇵",
  JP: "🇯🇵",
  Japan: "🇯🇵",
  韩国: "🇰🇷",
  KR: "🇰🇷",
  Korea: "🇰🇷",
  新加坡: "🇸🇬",
  SG: "🇸🇬",
  Singapore: "🇸🇬",
  美国: "🇺🇸",
  US: "🇺🇸",
  USA: "🇺🇸",
  America: "🇺🇸",
  英国: "🇬🇧",
  UK: "🇬🇧",
  Britain: "🇬🇧",
  德国: "🇩🇪",
  DE: "🇩🇪",
  Germany: "🇩🇪",
  法国: "🇫🇷",
  FR: "🇫🇷",
  France: "🇫🇷",
  加拿大: "🇨🇦",
  CA: "🇨🇦",
  Canada: "🇨🇦",
  澳大利亚: "🇦🇺",
  AU: "🇦🇺",
  Australia: "🇦🇺",
  印度: "🇮🇳",
  IN: "🇮🇳",
  India: "🇮🇳",
  泰国: "🇹🇭",
  TH: "🇹🇭",
  Thailand: "🇹🇭",
  马来西亚: "🇲🇾",
  MY: "🇲🇾",
  Malaysia: "🇲🇾",
  越南: "🇻🇳",
  VN: "🇻🇳",
  Vietnam: "🇻🇳",
  俄罗斯: "🇷🇺",
  RU: "🇷🇺",
  Russia: "🇷🇺",
  巴西: "🇧🇷",
  BR: "🇧🇷",
  Brazil: "🇧🇷",
  土耳其: "🇹🇷",
  TR: "🇹🇷",
  Turkey: "🇹🇷",
  荷兰: "🇳🇱",
  NL: "🇳🇱",
  Netherlands: "🇳🇱",
  中国: "🇨🇳",
  CN: "🇨🇳",
  China: "🇨🇳",
};
const getFlag = (name) => {
  if (!name) return "🌐";
  for (const [key, flag] of Object.entries(regionFlags)) {
    if (name.toUpperCase().includes(key.toUpperCase())) return flag;
  }
  return "🌐";
};

// 流量口径沿用原有 Dashboard：周期套餐看月流量，其余看总流量
const isPeriod = computed(() => user.value.plan_type === "period");
// 纯第三方套餐（套餐不勾本地节点）：第三方流量面板测不到 → 不显示流量，只显示到期日期
const hideTraffic = computed(
  () => !!user.value.plan_id && user.value.plan_include_local === false,
);
const usedBytes = computed(() =>
  isPeriod.value ? user.value.monthly_traffic_used : user.value.traffic_used,
);
const limitBytes = computed(() =>
  isPeriod.value ? user.value.monthly_traffic_limit : user.value.traffic_limit,
);
const trafficPercent = computed(() => {
  const limit = limitBytes.value || 0;
  if (!limit) return 0;
  return Math.min(100, Math.round(((usedBytes.value || 0) / limit) * 100));
});
const trafficLabel = computed(() => (isPeriod.value ? "当月已用" : "已用流量"));
// 进度条辉光（稿中 0 0 8px <barColor>60，即色值 0x60 ≈ 38% 透明度）
const barGlowBlue = "0 0 8px rgba(10, 132, 255, 0.38)";
const barGlowRed = "0 0 8px rgba(255, 69, 58, 0.38)";
const remainingBytes = computed(() => {
  const limit = limitBytes.value || 0;
  if (!limit) return 0;
  return Math.max(0, limit - (usedBytes.value || 0));
});
const expiredLabel = computed(() => {
  if (!user.value.expired_at) return "永不过期";
  return new Date(user.value.expired_at).toLocaleDateString();
});
// 是否已到期（compare against now）。到期后流量被关闭、用量冻结为最后记录。
const isExpired = computed(() => {
  if (!user.value.expired_at) return false;
  return new Date(user.value.expired_at).getTime() <= Date.now();
});
// 距到期天数（未到期且 >0 才显示；到期返回 0）
const daysLeft = computed(() => {
  if (!user.value.expired_at) return null;
  const diff = Math.ceil(
    (new Date(user.value.expired_at).getTime() - Date.now()) / (1000 * 60 * 60 * 24)
  );
  return diff > 0 ? diff : 0;
});
const planLabel = computed(() => user.value.plan_name || "无套餐");

// 重置倒计时
const resetDaysLeft = computed(() => {
  if (!user.value.next_traffic_reset_at) return null;
  const now = new Date();
  const resetAt = new Date(user.value.next_traffic_reset_at);
  const diff = Math.ceil((resetAt - now) / (1000 * 60 * 60 * 24));
  return diff > 0 ? diff : 0;
});
// 是否显示「重置日期」。
// 2026-11-25 修：原条件用 plan_months>1，但 plan_months 是「套餐当前配置的月数」，
// 管理员把某套餐从 3 个月改成 1 个月后，存量按 3 个月买的用户 plan_months 也变成 1，
// 导致这批人的「重置日期」整批消失（流量仍在按 30 天重置，却看不到日期）。
// 改为按用户自身的真实服务期判断：只要「到期日之前还真的会发生一次流量重置」就显示。
const showResetCountdown = computed(() => {
  if (user.value.plan_type !== "period") return false;
  if (!user.value.next_traffic_reset_at) return false;
  // 无到期日（理论上周期套餐不会有）：只要有待重置就显示
  if (!user.value.expired_at) return true;
  const resetAt = new Date(user.value.next_traffic_reset_at).getTime();
  const expiredAt = new Date(user.value.expired_at).getTime();
  // 重置点须在到期日前 3 天以上，才排除「1 个月套餐自身那一次与到期几乎重合的重置」
  return resetAt < expiredAt - 3 * 24 * 60 * 60 * 1000;
});
const resetDateLabel = computed(() => {
  if (!user.value.next_traffic_reset_at) return "";
  return new Date(user.value.next_traffic_reset_at).toLocaleDateString();
});

// 同步流量：提交任务 → 轮询任务态 → 终态后刷新流量 + Toast（沿用原 Dashboard 逻辑）
const syncing = ref(false);
const syncStatus = ref("");
let syncPollTimer = null;
let syncPollStartedAt = 0;

// 任务态文案：排队中… / 同步中…
const syncLabel = computed(() => {
  if (!syncing.value) return "同步流量";
  return syncStatus.value === "pending" ? "排队中…" : "同步中…";
});

// 同步任务到达终态：Toast 提示 + 刷新最新流量并停止轮询
const stopSyncPolling = (refresh) => {
  if (syncPollTimer) {
    window.clearTimeout(syncPollTimer);
    syncPollTimer = null;
  }
  if (refresh) {
    auth.fetchMe().catch(() => {});
  }
  syncing.value = false;
  syncStatus.value = "";
};

const pollSyncStatus = async () => {
  syncPollTimer = null;
  if (!syncing.value) return;
  let status = "running";
  try {
    const data = await userApi.syncTaskStatus();
    status = data?.status ?? "running";
    syncStatus.value = status;
  } catch (e) {
    /* 单次失败继续轮询 */
  }
  const terminal = ["succeeded", "partial", "failed"].includes(status);
  if (terminal) {
    if (status === "failed") toast.error("同步失败");
    else if (status === "partial") toast.success("同步完成（部分节点失败）");
    else toast.success("同步完成，流量已刷新");
    stopSyncPolling(true);
    return;
  }
  // 超时兜底：任务一直非终态时停止轮询并刷新，避免无限轮询
  if (Date.now() - syncPollStartedAt > 300000) {
    stopSyncPolling(true);
    return;
  }
  syncPollTimer = window.setTimeout(pollSyncStatus, 5000);
};

// 提交同步后开始轮询任务状态，完成时自动刷新页面数据
const startSyncPolling = () => {
  window.clearTimeout(syncPollTimer);
  syncPollStartedAt = Date.now();
  syncing.value = true;
  syncStatus.value = "pending";
  pollSyncStatus();
};

const syncTraffic = async () => {
  if (syncing.value) return;
  let submitted = false;
  try {
    const data = await userApi.syncTraffic();
    toast.success(
      data?.already_running ? "已有同步任务正在执行" : "同步任务已提交，完成后自动刷新",
    );
    submitted = true;
    startSyncPolling();
  } catch (e) {
    toast.error(e.message || "同步失败");
  }
  if (!submitted) {
    syncing.value = false;
    syncStatus.value = "";
  }
};

const loadNodes = async () => {
  try {
    nodes.value = await userApi.nodes();
  } catch (e) {
    nodes.value = [];
  }
};

// 节点筛选
const nodeFilter = ref("all");
const filteredNodes = computed(() => {
  if (nodeFilter.value === "online")
    return nodes.value.filter((n) => n.status === "online");
  if (nodeFilter.value === "offline")
    return nodes.value.filter((n) => n.status !== "online");
  return nodes.value;
});
const isOnline = (n) => n.status === "online";
/**
 * 节点倍率标签：与订阅里的 -Nx 后缀同口径。
 * 只在后台「用户端展示倍率」开关打开时接口才返回该字段，1 倍也显示（用户要能对比）。
 */
const multLabel = (m) => {
  const v = Number(m);
  if (!Number.isFinite(v)) return "";
  // 只对小数去尾零：String(10) 是 "10"，直接 replace(/0+$/) 会把 10 变成 1
  const s = String(v);
  const trimmed = s.includes(".") ? s.replace(/0+$/, "").replace(/\.$/, "") : s;
  return `×${trimmed}`;
};
const latencyColor = (ms) => {
  if (ms < 60) return "var(--green)";
  if (ms < 150) return "var(--lbl2)";
  return "var(--lbl3)";
};

// 订阅地址
const subUrl = computed(() => {
  if (!user.value.token) return "";
  return `${location.origin}/api/sub/${user.value.token}`;
});
const subUrlClash = computed(() => {
  if (!user.value.token) return "";
  return `${location.origin}/api/sub/${user.value.token}?clash=1`;
});
const subUrlSingbox = computed(() => {
  if (!user.value.token) return "";
  return `${location.origin}/api/sub/${user.value.token}?singbox=1`;
});

// 订阅格式开关
const subscriptionConfig = ref({
  sub_clash_enabled: "0",
  sub_singbox_enabled: "0",
});
const loadSubscriptionConfig = async () => {
  try {
    const data = await userApi.siteConfig();
    if (data) {
      subscriptionConfig.value = {
        sub_clash_enabled: data.sub_clash_enabled || "0",
        sub_singbox_enabled: data.sub_singbox_enabled || "0",
      };
    }
  } catch (e) {
    // 静默失败
  }
};

/**
 * 复制到剪贴板（三级降级）。
 * navigator.clipboard 只在安全上下文（https / localhost）且未被策略禁用时可用：
 * App WebView、iframe 内嵌、http 页面里它直接不存在或抛 NotAllowedError，
 * 所以失败时退到 textarea + document.execCommand('copy')。
 * 返回是否真的复制成功。
 */
const copyText = async (text) => {
  if (!text) return false;
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch (e) {
      /* 被策略禁用：落到下方兜底 */
    }
  }
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.setAttribute("readonly", "");
  ta.style.position = "fixed";
  ta.style.top = "-9999px";
  document.body.appendChild(ta);
  ta.select();
  ta.setSelectionRange(0, text.length); // iOS Safari 只有 select() 选不中
  let ok = false;
  try {
    ok = document.execCommand("copy");
  } catch (e) {
    ok = false;
  }
  ta.remove();
  return ok;
};

const copied = ref(false);
const copiedClash = ref(false);
const copiedSingbox = ref(false);

const copySub = async () => {
  if (!subUrl.value) return;
  if (!(await copyText(subUrl.value))) {
    toast.error("复制失败，请手动选择复制");
    return;
  }
  copied.value = true;
  toast.success("订阅地址已复制");
  setTimeout(() => {
    copied.value = false;
  }, 1500);
};
const copySubClash = async () => {
  if (!subUrlClash.value) return;
  if (!(await copyText(subUrlClash.value))) {
    toast.error("复制失败，请手动选择复制");
    return;
  }
  copiedClash.value = true;
  toast.success("Clash 订阅地址已复制");
  setTimeout(() => {
    copiedClash.value = false;
  }, 1500);
};
const copySubSingbox = async () => {
  if (!subUrlSingbox.value) return;
  if (!(await copyText(subUrlSingbox.value))) {
    toast.error("复制失败，请手动选择复制");
    return;
  }
  copiedSingbox.value = true;
  toast.success("Sing-box 订阅地址已复制");
  setTimeout(() => {
    copiedSingbox.value = false;
  }, 1500);
};

// 二维码弹层（真实二维码）
const qrOpen = ref(false);
const openQR = () => {
  if (!subUrl.value) return;
  qrOpen.value = true;
};

// 付费重置流量：创建订单 → 跳转支付 → 轮询订单状态
const paymentMethods = ref([]);
const resetOpen = ref(false);
const resetPayment = ref(null);
const creatingReset = ref(false);
const resetPrice = computed(() => user.value.plan_reset_price || 0);

let pollTimer = null;
const stopPolling = () => {
  if (pollTimer) {
    clearInterval(pollTimer);
    pollTimer = null;
  }
};
const startPolling = (orderNo) => {
  stopPolling();
  let count = 0;
  pollTimer = setInterval(async () => {
    count++;
    try {
      const status = await userApi.orderStatus(orderNo);
      if (status.status === "paid") {
        stopPolling();
        toast.success("重置成功");
        await auth.fetchMe();
        return;
      }
    } catch (e) {
      /* 单次失败继续轮询 */
    }
    if (count >= 60) stopPolling();
  }, 5000);
};

const loadPaymentMethods = async () => {
  try {
    paymentMethods.value = await userApi.paymentMethods();
  } catch (e) {
    paymentMethods.value = [];
  }
};

const openReset = async () => {
  resetOpen.value = true;
  resetPayment.value = null;
  if (!paymentMethods.value.length) await loadPaymentMethods();
  if (paymentMethods.value.length)
    resetPayment.value = paymentMethods.value[0].id;
};

const createResetOrder = async () => {
  // iOS Safari 会在「await 之后」拦截 window.open：先同步占住窗口再发请求（非 iOS 不占）
  const payWin = beginPayWindow();
  let opened = false;
  creatingReset.value = true;
  try {
    const result = await userApi.createResetOrder(resetPayment.value);
    if (result.status === "paid") {
      toast.success("重置成功");
      resetOpen.value = false;
      await auth.fetchMe();
      return;
    }
    if (result.pay_url) {
      resetOpen.value = false;
      gotoPay(payWin, result.pay_url);
      opened = true;
      toast.success("订单已创建，请完成支付");
      if (result.order_no) startPolling(result.order_no);
    }
  } catch (e) {
    toast.error(e.message || "创建订单失败");
  } finally {
    if (!opened) cancelPayWindow(payWin);
    creatingReset.value = false;
  }
};

onMounted(() => {
  loadNodes();
  loadSubscriptionConfig();
  // 每次进入/切回仪表盘刷一次最新流量：/api/me 平时只在登录路由守卫里拉一次，
  // 之后不点「同步流量」就一直是旧值。这里补一次轻量刷新（不轮询，避免给所有在线用户
  // 高频打接口）；失败静默，页面沿用 store 里的旧数据即可。
  auth.fetchMe().catch(() => {});
});

onUnmounted(() => {
  stopPolling();
  if (syncPollTimer) window.clearTimeout(syncPollTimer);
});
</script>

<template>
  <div class="db uu-slide-up">
    <!-- 流量卡（纯第三方套餐：不显示流量，只显示到期日期） -->
    <section class="db-card uu-mat-regular">
      <!-- 到期横幅：到期后流量已关闭，引导购买/续费 -->
      <div v-if="isExpired" class="db-expired-banner">
        <span class="db-expired-text">
          <UserIcon name="bell" :size="16" />
          套餐已到期，购买套餐后恢复
        </span>
        <button
          type="button"
          class="db-expired-buy uu-press"
          @click="router.push({ name: 'user-shop' })"
        >
          去购买
        </button>
      </div>
      <div v-if="!hideTraffic" class="db-traffic-head">
        <div>
          <p class="db-traffic-label">{{ trafficLabel }}</p>
          <div class="db-traffic-num">
            <span class="db-traffic-used">{{ fmtBytes(usedBytes) }}</span>
            <span v-if="limitBytes" class="db-traffic-total"
              >/ {{ fmtBytes(limitBytes) }}</span
            >
          </div>
        </div>
        <span v-if="user.plan_name" class="db-plan-badge">{{
          user.plan_name
        }}</span>
      </div>
      <p v-if="hideTraffic" class="db-traffic-label">
        套餐有效期
      </p>
      <div v-if="!hideTraffic" class="db-bar">
        <div
          class="db-bar-fill uu-traffic-fill"
          :style="{
            width: trafficPercent + '%',
            background: trafficPercent > 85 ? 'var(--red)' : 'var(--blue)',
            boxShadow: trafficPercent > 85 ? barGlowRed : barGlowBlue,
          }"
        ></div>
      </div>

      <div v-if="!hideTraffic" class="db-traffic-row">
        <p class="db-remaining">
          <template v-if="limitBytes">
            剩余
            <span class="db-remaining-num">{{ fmtBytes(remainingBytes) }}</span>
          </template>
        </p>
        <div class="db-traffic-actions">
          <!-- 用户端「同步流量」按钮暂时隐藏（用户 2026-11-08 定）：
               页面进入时已自动刷新一次流量，这个按钮多余；且它会给用户反复
               触发 traffic_sync 任务的机会（每个用户都能建一条），容易被用来
               刷任务挤占队列。要恢复就删掉这段注释标记。
          <button
            type="button"
            class="db-sync-btn uu-press"
            :disabled="syncing"
            @click="syncTraffic"
          >
            <UserIcon name="refresh" :size="16" :spin="syncing" />
            {{ syncLabel }}
          </button>
          -->
          <button
            v-if="user.plan_id && isPeriod"
            type="button"
            class="db-reset-btn uu-press"
            :disabled="creatingReset"
            @click="openReset"
          >
            <UserIcon name="refresh" :size="16" :spin="creatingReset" />
            {{ creatingReset ? "重置中…" : "重置流量" }}
          </button>
        </div>
      </div>

      <div class="db-foot">
        <div v-if="showResetCountdown && !hideTraffic" class="db-foot-item">
          <UserIcon name="clock" :size="14" />
          <span>重置日期：<b>{{ resetDateLabel }}</b></span>
        </div>
        <div class="db-foot-item">
          <span class="db-foot-dot" :class="{ 'db-foot-dot--off': !isExpired }"></span>
          <span>
            到期：<b>{{ expiredLabel }}</b>
            <template v-if="isExpired">· 已到期</template>
            <template v-else-if="daysLeft !== null && daysLeft > 0">（剩 {{ daysLeft }} 天）</template>
          </span>
        </div>
      </div>
    </section>

    <!-- 订阅管理 -->
    <section v-if="subUrl" class="db-card uu-mat-regular">
      <div class="db-card-head">
        <p class="db-card-title">订阅管理</p>
        <button type="button" class="db-qr-btn uu-press" @click="openQR">
          <UserIcon name="qr" :size="17" />
          <span>二维码</span>
        </button>
      </div>

      <div class="db-url-box">
        <div class="db-url-icon">
          <UserIcon name="globe" :size="14" />
        </div>
        <div class="db-url-main">
          <p class="db-url-label">订阅地址</p>
          <p class="db-url-text">{{ subUrl }}</p>
        </div>
        <button
          type="button"
          class="db-copy-btn uu-press"
          :class="{ 'db-copy-btn--ok': copied }"
          @click="copySub"
        >
          <UserIcon :name="copied ? 'check' : 'copy'" :size="16" />
          复制
        </button>
      </div>

      <div
        v-if="
          subscriptionConfig.sub_clash_enabled === '1' ||
          subscriptionConfig.sub_singbox_enabled === '1'
        "
        class="db-clients"
      >
        <button
          v-if="subscriptionConfig.sub_clash_enabled === '1'"
          type="button"
          class="db-client uu-press"
          :class="{ 'db-client--ok': copiedClash }"
          @click="copySubClash"
        >
          <span
            class="db-client-icon"
            :class="{ 'db-client-icon--clash': copiedClash }"
          >
            <img v-if="!copiedClash" src="/images/clash.png" alt="Clash" class="db-client-img" />
            <UserIcon v-else name="check" :size="16" style="color: #ff6b35" />
          </span>
          <span class="db-client-text">
            <span class="db-client-name">Clash 导入</span>
            <span class="db-client-sub">点击复制链接</span>
          </span>
        </button>

        <button
          v-if="subscriptionConfig.sub_singbox_enabled === '1'"
          type="button"
          class="db-client uu-press"
          :class="{ 'db-client--ok': copiedSingbox }"
          @click="copySubSingbox"
        >
          <span
            class="db-client-icon"
            :class="{ 'db-client-icon--singbox': copiedSingbox }"
          >
            <img v-if="!copiedSingbox" src="/images/singbox.svg" alt="Sing-box" class="db-client-img" />
            <UserIcon
              v-else
              name="check"
              :size="16"
              style="color: var(--blue)"
            />
          </span>
          <span class="db-client-text">
            <span class="db-client-name">Sing-box 导入</span>
            <span class="db-client-sub">点击复制链接</span>
          </span>
        </button>
      </div>
    </section>

    <!-- 使用文档入口 -->
    <button
      type="button"
      class="db-docs uu-press uu-mat-regular"
      @click="router.push({ name: 'user-tutorials' })"
    >
      <span class="db-docs-icon">
        <UserIcon name="doc" :size="18" />
      </span>
      <span class="db-docs-text">
        <span class="db-docs-name">使用文档</span>
        <span class="db-docs-sub">客户端配置 · 常见问题</span>
      </span>
      <span class="db-docs-chevron"
        ><UserIcon name="chevronRight" :size="16"
      /></span>
    </button>

    <!-- 节点列表 -->
    <section class="db-card uu-mat-regular">
      <div class="db-card-head">
        <p class="db-card-title">节点列表</p>
        <SegControl
          v-model="nodeFilter"
          :options="[
            { label: '全部', value: 'all' },
            { label: '在线', value: 'online' },
            { label: '离线', value: 'offline' },
          ]"
        />
      </div>

      <div v-if="filteredNodes.length" class="db-nodes">
        <div
          v-for="(n, i) in filteredNodes"
          :key="n.id"
          class="db-node uu-slide-up"
          :style="{ animationDelay: i * 30 + 'ms' }"
        >
          <span class="db-node-flag">{{ getFlag(n.name) }}</span>
          <div class="db-node-main">
            <p class="db-node-name">
              {{ n.name }}
              <!-- 倍率：接口只在后台「用户端展示倍率」开关打开时返回该字段（issue #58） -->
              <span
                v-if="n.traffic_multiplier != null"
                class="db-node-mult"
                :class="{ 'db-node-mult--hit': Number(n.traffic_multiplier) !== 1 }"
              >{{ multLabel(n.traffic_multiplier) }}</span>
            </p>
          </div>
          <div class="db-node-side">
            <span
              v-if="isOnline(n) && n.latency != null"
              class="db-node-latency"
              :style="{ color: latencyColor(n.latency) }"
            >
              {{ n.latency }}ms
            </span>
            <span class="db-node-dotwrap">
              <span
                v-if="isOnline(n)"
                class="db-node-pulse uu-pulse-ring"
              ></span>
              <span
                class="db-node-dot"
                :class="{ 'db-node-dot--on': isOnline(n) }"
              ></span>
            </span>
          </div>
        </div>
      </div>
      <p v-else class="db-empty">暂无可用节点</p>
    </section>

    <!-- 订阅二维码弹层 -->
    <QrSheet v-model="qrOpen" :url="subUrl" />

    <!-- 重置流量（付费） -->
    <BottomSheet v-model="resetOpen" title="重置流量" max-width="460px">
      <p class="db-reset-info">
        此操作将重置已使用的流量但不会增加套餐时长，是否继续？
      </p>
      <div v-if="resetPrice > 0 && paymentMethods.length" class="db-reset-pay">
        <p class="db-reset-pay-title">选择支付方式</p>
        <div class="db-reset-pay-list">
          <label
            v-for="m in paymentMethods"
            :key="m.id"
            class="db-reset-pay-item"
            :class="{ 'db-reset-pay-item--on': resetPayment === m.id }"
          >
            <input type="radio" :value="m.id" v-model="resetPayment" />
            <span>{{ m.name }}</span>
          </label>
        </div>
      </div>
      <template #footer>
        <button
          type="button"
          class="db-reset-ok uu-press"
          :disabled="creatingReset"
          @click="createResetOrder"
        >
          {{
            creatingReset
              ? "处理中..."
              : resetPrice > 0
                ? `支付 ¥${resetPrice}`
                : "免费重置"
          }}
        </button>
        <button
          type="button"
          class="db-reset-cancel uu-press"
          @click="resetOpen = false"
        >
          取消
        </button>
      </template>
    </BottomSheet>
  </div>
</template>

<style scoped src="@/assets/styles/pages/user/Dashboard.css"></style>
