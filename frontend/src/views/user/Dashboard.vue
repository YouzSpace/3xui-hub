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
const planLabel = computed(() => user.value.plan_name || "无套餐");

// 重置倒计时（沿用原显示条件：周期套餐且 >1 月才显示）
const resetDaysLeft = computed(() => {
  if (!user.value.next_traffic_reset_at) return null;
  const now = new Date();
  const resetAt = new Date(user.value.next_traffic_reset_at);
  const diff = Math.ceil((resetAt - now) / (1000 * 60 * 60 * 24));
  return diff > 0 ? diff : 0;
});
const showResetCountdown = computed(() => {
  return (
    user.value.plan_type === "period" &&
    user.value.plan_months > 1 &&
    resetDaysLeft.value !== null
  );
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
      window.open(result.pay_url, "_blank");
      toast.success("订单已创建，请完成支付");
      if (result.order_no) startPolling(result.order_no);
    }
  } catch (e) {
    toast.error(e.message || "创建订单失败");
  } finally {
    creatingReset.value = false;
  }
};

onMounted(() => {
  loadNodes();
  loadSubscriptionConfig();
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
          <button
            type="button"
            class="db-sync-btn uu-press"
            :disabled="syncing"
            @click="syncTraffic"
          >
            <UserIcon name="refresh" :size="16" :spin="syncing" />
            {{ syncLabel }}
          </button>
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
          <span class="db-foot-dot"></span>
          <span>到期：<b>{{ expiredLabel }}</b></span>
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

<style scoped>
.db {
  display: flex;
  flex-direction: column;
  gap: 20px;
  padding-bottom: 8px;
}

/* ===== 卡片 ===== */
.db-card {
  border-radius: 24px;
  padding: 20px;
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.db-card-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.db-card-title {
  margin: 0;
  font-size: 17px;
  font-weight: 600;
  color: var(--lbl1);
}

/* ===== 流量卡 ===== */
.db-traffic-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
}
.db-traffic-label {
  margin: 0;
  font-size: 13px;
  font-weight: 500;
  color: var(--lbl2);
}
.db-traffic-num {
  display: flex;
  align-items: baseline;
  gap: 6px;
  margin-top: 4px;
  /* 窄屏（≤320px）下数字与单位不折行：整体撑到宽度上限也不让内部断开 */
  flex-wrap: nowrap;
  min-width: 0;
}
.db-traffic-used {
  font-size: 34px;
  font-weight: 800;
  letter-spacing: -1.5px;
  color: var(--lbl1);
  font-family: var(--uu-font-mono);
  white-space: nowrap;
}
.db-traffic-total {
  font-size: 15px;
  color: var(--lbl3);
  white-space: nowrap;
}
.db-plan-badge {
  margin-top: 2px;
  padding: 4px 10px;
  border-radius: 12px;
  background: var(--blue);
  color: #fff;
  font-size: 13px;
  font-weight: 700;
  white-space: nowrap;
}

.db-bar {
  height: 8px;
  border-radius: 999px;
  background: var(--fill3);
  overflow: hidden;
}
.db-bar-fill {
  height: 100%;
  border-radius: 999px;
}

.db-traffic-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: 8px;
}
.db-traffic-actions {
  display: flex;
  align-items: center;
  gap: 8px;
}
.db-remaining {
  margin: 0;
  font-size: 13px;
  color: var(--lbl1); /* 「剩余」和数字都用主文字色（用户 2026-11-06 定）：lbl3 太浅看不清 */
  font-weight: 600;
}
.db-remaining-num {
  color: var(--lbl1);
  font-weight: 600;
}
.db-reset-btn {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 6px 12px;
  border-radius: 12px;
  background: var(--orange-t);
  color: var(--orange);
  border: 1px solid rgba(255, 159, 10, 0.3);
  font-size: 13px;
  font-weight: 600;
  font-family: inherit;
}
.db-reset-btn:disabled {
  background: var(--fill3);
  color: var(--lbl3);
  border-color: var(--sep);
  cursor: not-allowed;
}

/* 同步流量：与重置按钮同级，中性配色 */
.db-sync-btn {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 6px 12px;
  border-radius: 12px;
  background: var(--fill3);
  color: var(--lbl2);
  border: 1px solid var(--sep);
  font-size: 13px;
  font-weight: 600;
  font-family: inherit;
}
.db-sync-btn:disabled {
  background: var(--fill3);
  color: var(--lbl3);
  border-color: var(--sep);
  cursor: not-allowed;
}

.db-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding-top: 12px;
  border-top: 1px solid var(--sep);
}
.db-foot-item {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 12px;
  color: var(--lbl3);
  /* 窄屏两项同行不折行：放不下时文字收缩省略，图标/圆点不缩 */
  white-space: nowrap;
  min-width: 0;
}
.db-foot-item svg {
  flex-shrink: 0;
}
.db-foot-item span {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
}
.db-foot-item b {
  color: var(--lbl2);
  font-weight: 500;
}
.db-foot-dot {
  width: 6px;
  height: 6px;
  border-radius: 999px;
  background: var(--red);
  flex-shrink: 0;
}

/* ===== 订阅管理 ===== */
.db-qr-btn {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 4px 12px;
  border: none;
  border-radius: 12px;
  background: var(--fill3);
  color: var(--lbl3);
  font-size: 12px;
  font-family: inherit;
}

.db-url-box {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 12px 14px;
  border-radius: 16px;
  background: var(--fill4);
  border: 1px solid var(--sep);
}
.db-url-icon {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 28px;
  height: 28px;
  border-radius: 8px;
  background: var(--blue-t);
  color: var(--blue);
  flex-shrink: 0;
}
.db-url-main {
  flex: 1;
  min-width: 0;
}
.db-url-label {
  margin: 0 0 2px;
  font-size: 10px;
  font-weight: 600;
  color: var(--lbl4);
  letter-spacing: 0.06em;
  text-transform: uppercase;
}
.db-url-text {
  margin: 0;
  font-size: 11.5px;
  color: var(--lbl3);
  font-family: var(--uu-font-mono);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.db-copy-btn {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 6px 12px;
  border: none;
  border-radius: 12px;
  background: var(--fill2);
  color: var(--lbl2);
  font-size: 13px;
  font-weight: 600;
  font-family: inherit;
  flex-shrink: 0;
}
.db-copy-btn--ok {
  background: var(--green-t);
  color: var(--green);
}

.db-clients {
  display: grid;
  grid-template-columns: repeat(2, 1fr);
  gap: 8px;
}
.db-client {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 12px 14px;
  border-radius: 16px;
  background: var(--fill3);
  border: 1px solid var(--sep);
  text-align: left;
  font-family: inherit;
}
.db-client--ok {
  background: var(--green-t);
  border-color: rgba(48, 209, 88, 0.3);
}
.db-client-icon {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 32px;
  height: 32px;
  border-radius: 12px;
  background: rgba(255, 107, 53, 0.15);
  flex-shrink: 0;
}
.db-client-icon--singbox {
  background: rgba(10, 132, 255, 0.15);
}
.db-client-icon--clash {
  background: rgba(255, 107, 53, 0.15);
}
.db-client-img {
  display: block;
  width: 20px;
  height: 20px;
  object-fit: contain;
}
.db-client-text {
  display: flex;
  flex-direction: column;
  min-width: 0;
}
.db-client-name {
  font-size: 13px;
  font-weight: 600;
  color: var(--lbl1);
}
.db-client-sub {
  margin-top: 1px;
  font-size: 11px;
  color: var(--lbl4);
}

/* ===== 使用文档 ===== */
.db-docs {
  display: flex;
  align-items: center;
  gap: 12px;
  width: 100%;
  padding: 14px 16px;
  border-radius: 16px;
  text-align: left;
  font-family: inherit;
}
.db-docs-icon {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 36px;
  height: 36px;
  border-radius: 12px;
  background: var(--indigo-t);
  color: var(--indigo);
  flex-shrink: 0;
}
.db-docs-text {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
}
.db-docs-name {
  font-size: 15px;
  font-weight: 500;
  color: var(--lbl1);
}
.db-docs-sub {
  margin-top: 1px;
  font-size: 12px;
  color: var(--lbl3);
}
.db-docs-chevron {
  color: var(--lbl4);
  display: inline-flex;
}

/* ===== 节点列表 ===== */
.db-nodes {
  display: flex;
  flex-direction: column;
}
.db-node {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 12px 0;
  border-bottom: 1px solid var(--sep);
}
.db-node:last-child {
  border-bottom: none;
}
.db-node-flag {
  font-size: 26px;
  line-height: 1;
}
.db-node-main {
  flex: 1;
  min-width: 0;
}
.db-node-name {
  margin: 0;
  font-size: 15px;
  font-weight: 500;
  color: var(--lbl1);
}
/* 倍率标签：1 倍用中性灰，非 1 倍用强调色（一眼分出计费更贵的线路） */
.db-node-mult {
  display: inline-block;
  margin-left: 6px;
  padding: 1px 6px;
  border-radius: 999px;
  font-size: 11px;
  font-weight: 700;
  font-family: var(--uu-font-mono);
  vertical-align: middle;
  background: rgba(120, 120, 128, 0.14);
  color: var(--lbl2);
}
.db-node-mult--hit {
  background: rgba(13, 148, 136, 0.16);
  color: var(--accent, #0d9488);
}
.db-node-side {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-shrink: 0;
}
.db-node-latency {
  font-size: 13px;
  font-weight: 600;
  font-family: var(--uu-font-mono);
}
.db-node-dotwrap {
  position: relative;
  display: flex;
  align-items: center;
  justify-content: center;
  width: 16px;
  height: 16px;
}
.db-node-pulse {
  position: absolute;
  inset: 0;
  border-radius: 999px;
  background: var(--green);
}
.db-node-dot {
  width: 10px;
  height: 10px;
  border-radius: 999px;
  background: var(--lbl4);
}
.db-node-dot--on {
  background: var(--green);
  box-shadow: 0 0 6px var(--green);
}
.db-empty {
  margin: 0;
  padding: 24px;
  text-align: center;
  font-size: 13px;
  color: var(--lbl3);
}

/* ===== 重置弹层 ===== */
.db-reset-info {
  margin: 0;
  font-size: 14px;
  line-height: 1.6;
  color: var(--lbl2);
}
.db-reset-pay-title {
  margin: 0 0 10px;
  font-size: 13px;
  font-weight: 600;
  color: var(--lbl3);
}
.db-reset-pay-list {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}
.db-reset-pay-item {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  padding: 8px 14px;
  border-radius: 10px;
  background: var(--fill3);
  border: 2px solid transparent;
  font-size: 13px;
  color: var(--lbl1);
  cursor: pointer;
}
.db-reset-pay-item--on {
  border-color: var(--blue);
}
.db-reset-ok {
  width: 100%;
  padding: 14px;
  border: none;
  border-radius: 16px;
  background: var(--blue);
  color: #fff;
  font-size: 17px;
  font-weight: 600;
  font-family: inherit;
}
.db-reset-ok:disabled {
  opacity: 0.6;
  cursor: not-allowed;
}
.db-reset-cancel {
  width: 100%;
  padding: 12px;
  border: none;
  border-radius: 16px;
  background: var(--fill3);
  color: var(--lbl2);
  font-size: 15px;
  font-family: inherit;
}

@media (max-width: 480px) {
  .db-clients {
    grid-template-columns: 1fr;
  }
  /* 窄屏：脚注两项仍同行（white-space:nowrap + 收缩），不再竖排折行 */
}
</style>
