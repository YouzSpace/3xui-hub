<script setup>
/**
 * UserIcon — 用户端新 UI 图标（SVG 移植自设计稿 ui/new/src/App.tsx）。
 * name: home|cart|person|shield|copy|check|checkCircle|chevronLeft|chevronRight|chevronDown|
 *       bell|qr|refresh|sun|moon|logout|wifi|doc|globe|clock|clash|singbox|mail
 * filled: home/cart/person 实心态；dot: bell 红点；spin: refresh 旋转
 * size 覆盖默认尺寸（不传则用稿中原始尺寸）。
 */
import { computed } from 'vue'

const NATURAL = {
  home: 26, cart: 26, person: 26, shield: 22, copy: 16, check: 16,
  checkCircle: 20, chevronLeft: 16, chevronRight: 16, chevronDown: 14, bell: 22, qr: 17,
  refresh: 16, sun: 20, moon: 20, logout: 20, wifi: 16, doc: 18,
  globe: 14, clock: 14, clash: 20, singbox: 20, mail: 20,
}

const props = defineProps({
  name: { type: String, required: true },
  size: { type: Number, default: 0 },
  filled: { type: Boolean, default: false },
  dot: { type: Boolean, default: false },
  spin: { type: Boolean, default: false },
})

const wh = computed(() => props.size || NATURAL[props.name] || 20)
const vb = computed(() => NATURAL[props.name] || 20)
</script>

<template>
  <svg
    :width="wh"
    :height="wh"
    :class="{ 'uu-spin': spin }"
    :viewBox="`0 0 ${vb} ${vb}`"
    fill="none"
    aria-hidden="true"
    role="img"
  >
    <!-- home -->
    <template v-if="name === 'home'">
      <path
        v-if="filled"
        d="M3 11.5L13 3l10 8.5V22a1.5 1.5 0 01-1.5 1.5h-5.25v-5.25h-4.5V23.5H4.5A1.5 1.5 0 013 22z"
        fill="currentColor"
      />
      <path
        v-else
        d="M3 11.5L13 3l10 8.5V22a1.5 1.5 0 01-1.5 1.5h-5.25v-5.25h-4.5V23.5H4.5A1.5 1.5 0 013 22z"
        stroke="currentColor"
        stroke-width="1.6"
        stroke-linejoin="round"
      />
    </template>

    <!-- cart -->
    <template v-else-if="name === 'cart'">
      <path
        d="M1.5 1.5h3.8l2.6 12.5H19l2.5-8.5H6.5"
        fill="none"
        stroke="currentColor"
        :stroke-width="filled ? 1.8 : 1.6"
        stroke-linecap="round"
        stroke-linejoin="round"
      />
      <circle cx="9.5" cy="20.5" r="2" fill="currentColor" />
      <circle cx="17.5" cy="20.5" r="2" fill="currentColor" />
    </template>

    <!-- person -->
    <template v-else-if="name === 'person'">
      <template v-if="filled">
        <circle cx="13" cy="9" r="5" fill="currentColor" />
        <path d="M3 23c0-4.4 4.5-8 10-8s10 3.6 10 8" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" />
      </template>
      <template v-else>
        <circle cx="13" cy="9" r="5" stroke="currentColor" stroke-width="1.6" />
        <path d="M3 23c0-4.4 4.5-8 10-8s10 3.6 10 8" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" />
      </template>
    </template>

    <!-- shield -->
    <g v-else-if="name === 'shield'">
      <path
        d="M11 2L3 5.5v6c0 4.8 3.6 9.3 8 10.5 4.4-1.2 8-5.7 8-10.5v-6L11 2z"
        stroke="currentColor"
        stroke-width="1.5"
        stroke-linejoin="round"
      />
      <path d="M8 11l2.5 2.5 4-4.5" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" />
    </g>

    <!-- copy -->
    <g v-else-if="name === 'copy'">
      <rect x="5.5" y="5.5" width="8" height="8" rx="2" stroke="currentColor" stroke-width="1.4" />
      <path d="M3.5 10.5V3a.5.5 0 01.5-.5h7.5" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" />
    </g>

    <!-- check -->
    <path v-else-if="name === 'check'" d="M2.5 8l4 4 7-7" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" />

    <!-- checkCircle -->
    <g v-else-if="name === 'checkCircle'">
      <circle cx="10" cy="10" r="8.5" stroke="currentColor" stroke-width="1.4" />
      <path d="M6 10l3 3 5-5" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" />
    </g>

    <!-- chevronLeft -->
    <path v-else-if="name === 'chevronLeft'" d="M10 4l-4 4 4 4" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" />

    <!-- chevronRight -->
    <path v-else-if="name === 'chevronRight'" d="M6 4l4 4-4 4" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" />

    <!-- chevronDown -->
    <path v-else-if="name === 'chevronDown'" d="M3 5l4 4 4-4" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" />

    <!-- bell -->
    <g v-else-if="name === 'bell'">
      <path
        d="M11 2.5a7 7 0 00-7 7c0 4-1.5 6-1.5 6h17s-1.5-2-1.5-6a7 7 0 00-7-7zM9 18a2 2 0 004 0"
        stroke="currentColor"
        stroke-width="1.5"
        stroke-linecap="round"
      />
      <circle v-if="dot" cx="16.5" cy="5" r="3.5" fill="var(--red)" stroke="rgba(255,255,255,0.15)" stroke-width="1.5" />
    </g>

    <!-- qr -->
    <g v-else-if="name === 'qr'">
      <rect x="1.5" y="1.5" width="5" height="5" rx="1.2" stroke="currentColor" stroke-width="1.3" />
      <rect x="10.5" y="1.5" width="5" height="5" rx="1.2" stroke="currentColor" stroke-width="1.3" />
      <rect x="1.5" y="10.5" width="5" height="5" rx="1.2" stroke="currentColor" stroke-width="1.3" />
      <rect x="3" y="3" width="2" height="2" fill="currentColor" />
      <rect x="12" y="3" width="2" height="2" fill="currentColor" />
      <rect x="3" y="12" width="2" height="2" fill="currentColor" />
      <path d="M10.5 10.5h2M14.5 10.5v2M10.5 14.5h4v-2" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round" />
    </g>

    <!-- refresh -->
    <g v-else-if="name === 'refresh'">
      <path d="M13.5 8a5.5 5.5 0 11-1.2-3.4" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
      <path d="M12.5 1.5v3.5H9" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" />
    </g>

    <!-- sun -->
    <g v-else-if="name === 'sun'">
      <circle cx="10" cy="10" r="3.8" stroke="currentColor" stroke-width="1.5" />
      <path
        d="M10 2v2M10 16v2M2 10h2M16 10h2M4.1 4.1l1.4 1.4M14.5 14.5l1.4 1.4M15.9 4.1l-1.4 1.4M5.5 14.5l-1.4 1.4"
        stroke="currentColor"
        stroke-width="1.5"
        stroke-linecap="round"
      />
    </g>

    <!-- moon -->
    <path
      v-else-if="name === 'moon'"
      d="M17 11.5A7.5 7.5 0 018.5 3c0-.7.08-1.37.24-2A8.5 8.5 0 1019 17.76c-.63.16-1.3.24-2 .24z"
      stroke="currentColor"
      stroke-width="1.5"
      stroke-linecap="round"
      stroke-linejoin="round"
    />

    <!-- logout -->
    <g v-else-if="name === 'logout'">
      <path d="M7.5 3.5H4a1 1 0 00-1 1v11a1 1 0 001 1h3.5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
      <path d="M13 6.5L17 10l-4 3.5M17 10H8" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" />
    </g>

    <!-- wifi -->
    <g v-else-if="name === 'wifi'">
      <path d="M1 6c1.9-2.2 4.8-3.5 7-3.5S13.1 3.8 15 6" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" />
      <path d="M3.5 9C4.9 7.5 6.4 6.7 8 6.7s3.1.8 4.5 2.3" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" />
      <path d="M6 11.8C6.8 11 7.4 10.5 8 10.5s1.2.5 2 1.3" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" />
      <circle cx="8" cy="14" r="1.2" fill="currentColor" />
    </g>

    <!-- doc（使用文档） -->
    <g v-else-if="name === 'doc'">
      <path d="M4 2h7l4 4v10a1 1 0 01-1 1H4a1 1 0 01-1-1V3a1 1 0 011-1z" stroke="currentColor" stroke-width="1.4" stroke-linejoin="round" />
      <path d="M11 2v4h4" stroke="currentColor" stroke-width="1.4" stroke-linejoin="round" />
      <path d="M6 9h6M6 12h4" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" />
    </g>

    <!-- globe（订阅地址） -->
    <g v-else-if="name === 'globe'">
      <path d="M7 1.5a5.5 5.5 0 100 11 5.5 5.5 0 000-11z" stroke="currentColor" stroke-width="1.3" />
      <path d="M4.5 7c0-2 1-4 2.5-4s2.5 2 2.5 4-1 4-2.5 4S4.5 9 4.5 7z" stroke="currentColor" stroke-width="1.3" />
      <path d="M1.5 7h11" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" />
    </g>

    <!-- clock（重置日期） -->
    <g v-else-if="name === 'clock'">
      <path d="M11.5 7a4.5 4.5 0 11-.9-2.7" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" />
      <path d="M11 2v3H8" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round" />
    </g>

    <!-- clash 导入 -->
    <g v-else-if="name === 'clash'">
      <path
        d="M10 2C7 2 4.5 4.2 4.5 7c0 1.6.6 3 1.5 4L5 16h10l-1-5c.9-1 1.5-2.4 1.5-4C15.5 4.2 13 2 10 2z"
        fill="#FF6B35"
      />
      <path d="M6.5 5.5L5 3.5l2.5 1.5M13.5 5.5L15 3.5l-2.5 1.5" stroke="#FF6B35" stroke-width="1.1" stroke-linecap="round" stroke-linejoin="round" />
      <circle cx="8.5" cy="8" r="1.1" fill="white" />
      <circle cx="11.5" cy="8" r="1.1" fill="white" />
    </g>

    <!-- singbox 导入 -->
    <g v-else-if="name === 'singbox'">
      <path d="M3.5 7.5L10 4.5l6.5 3v5l-6.5 3-6.5-3v-5z" fill="rgba(10,132,255,0.2)" stroke="#0A84FF" stroke-width="1.3" stroke-linejoin="round" />
      <path d="M3.5 7.5L10 10.5l6.5-3" stroke="#0A84FF" stroke-width="1.3" stroke-linejoin="round" />
      <path d="M10 10.5V17.5" stroke="#0A84FF" stroke-width="1.3" stroke-linecap="round" />
    </g>

    <!-- mail（反馈） -->
    <g v-else-if="name === 'mail'">
      <rect x="2" y="4" width="16" height="12" rx="2.5" stroke="currentColor" stroke-width="1.5" />
      <path d="M3 6l7 5 7-5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" />
    </g>
  </svg>
</template>
