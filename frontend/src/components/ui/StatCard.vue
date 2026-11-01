<script setup>
/**
 * 统计卡：仪表盘的「业务概览」和节点监控的「监控汇总」两排卡片共用。
 *
 * 抽出来的原因：两排卡片原本在 Dashboard.vue / NodeMonitor.vue 里各写了一份
 * 逐字节相同的样式（.ad-stat-* 与 .nm-stat-*），改一处要记得改另一处。
 *
 * 用法：
 *   <StatCard label="用户总数" :value="104" note="注册用户数" />
 *   <StatCard label="服务器" :value="6" :denominator="8" note="在线 / 总数"
 *             health="部分离线" health-state="degraded" />
 *   <StatCard label="覆盖地区" :value="3" suffix="个" note="..." />   ← 单位用 suffix
 *   <StatCard ...><template #note>带链接/条件的长说明</template></StatCard>
 *   <StatCard ...>  ← 默认插槽放迷你曲线之类的附加内容
 */
import Card from '@/components/ui/Card.vue'

defineProps({
  label: { type: String, required: true },
  value: { type: [String, Number], default: '' },
  /** 数值后缀（如「个」）；与 denominator 互斥使用 */
  suffix: { type: String, default: '' },
  /** 分母，渲染成「 / 8」；null 表示不显示 */
  denominator: { type: [String, Number], default: null },
  note: { type: String, default: '' },
  /** 右上角状态标签文案，空则不显示 */
  health: { type: String, default: '' },
  /** 状态标签配色：healthy | degraded | offline */
  healthState: { type: String, default: '' },
})
</script>

<template>
  <Card class="sc" :data-health="healthState || undefined">
    <div class="sc-head">
      <p class="sc-label">{{ label }}</p>
      <span v-if="health" class="sc-health">{{ health }}</span>
    </div>

    <p class="sc-value">
      <slot name="value">{{ value }}</slot>
      <span v-if="suffix" class="sc-unit">&nbsp;{{ suffix }}</span>
      <span v-if="denominator !== null" class="sc-unit">&nbsp;/ {{ denominator }}</span>
    </p>

    <p class="sc-note">
      <slot name="note">{{ note }}</slot>
    </p>

    <slot />
  </Card>
</template>

<style scoped>
.sc {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  min-width: 0;
}
.sc-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-2);
}
.sc-label {
  margin: 0;
  font-size: var(--text-xs);
  color: var(--text-muted);
}
.sc-health {
  font-size: var(--text-xs);
  font-weight: var(--font-semibold);
  padding: 1px 8px;
  border-radius: var(--radius-md);
  color: var(--text-secondary);
  background: var(--bg-elevated);
  white-space: nowrap;
}
.sc[data-health='healthy'] .sc-health {
  color: var(--success);
  background: var(--success-muted);
}
.sc[data-health='degraded'] .sc-health {
  color: var(--warning);
  background: var(--warning-muted);
}
.sc[data-health='offline'] .sc-health {
  color: var(--danger);
  background: var(--danger-muted);
}
/* 数值等宽并对齐小数点：刷新时数字不左右晃 */
.sc-value {
  margin: 0;
  font-size: var(--text-2xl);
  font-weight: var(--font-bold);
  font-family: var(--font-mono);
  font-variant-numeric: tabular-nums;
  letter-spacing: -0.02em;
}
.sc-unit {
  font-size: var(--text-md);
  font-weight: var(--font-regular);
  color: var(--text-muted);
}
.sc-note {
  margin: 0;
  font-size: var(--text-xs);
  color: var(--text-muted);
  min-height: 1.5em;
}
</style>
