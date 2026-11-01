<script setup>
/**
 * QrSheet — 订阅二维码弹层（稿中弹层样式 + 真实二维码，qrcode 库生成）。
 */
import { ref, watch, nextTick } from 'vue'
import QRCode from 'qrcode'
import BottomSheet from '@/components/user/BottomSheet.vue'

const props = defineProps({
  modelValue: { type: Boolean, default: false },
  url: { type: String, default: '' },
})
const emit = defineEmits(['update:modelValue'])

const canvas = ref(null)

const renderQR = async () => {
  if (!props.url || !canvas.value) return
  try {
    await QRCode.toCanvas(canvas.value, props.url, {
      width: 220,
      margin: 2,
      color: { dark: '#111111', light: '#ffffff' },
    })
  } catch (e) {
    // 生成失败时留白
  }
}

watch(
  () => props.modelValue,
  async (open) => {
    if (open) {
      await nextTick()
      await renderQR()
    }
  }
)
</script>

<template>
  <BottomSheet :model-value="modelValue" title="订阅二维码" max-width="384px" @update:model-value="emit('update:modelValue', $event)">
    <div class="qr-box">
      <canvas ref="canvas" class="qr-canvas"></canvas>
    </div>
    <p class="qr-url">{{ url.length > 52 ? url.slice(0, 52) + '…' : url }}</p>
    <template #footer>
      <button type="button" class="qr-close uu-press" @click="emit('update:modelValue', false)">关闭</button>
    </template>
  </BottomSheet>
</template>

<style scoped>
.qr-box {
  display: flex;
  justify-content: center;
  padding: 16px;
  background: #fff;
  border-radius: 16px;
}
.qr-canvas {
  border-radius: 8px;
}
.qr-url {
  margin: 0;
  padding: 0 16px;
  font-size: 12px;
  line-height: 1.5;
  color: var(--lbl3);
  text-align: center;
  word-break: break-all;
}
.qr-close {
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
</style>
