<script setup>
/**
 * 入站表单弹窗外壳：标题（新建 / 编辑入站 tag）、字段区、底部保存按钮。
 * 状态全部来自页面容器的上下文（useInboundForm），这里不持有表单数据。
 */
import Button from '@/components/ui/Button.vue'
import Modal from '@/components/ui/Modal.vue'
import InboundFormFields from './InboundFormFields.vue'
import { useInboundCtx, writable } from './formContext'

const { modalOpen, editing, saving, saved, save } = useInboundCtx()

const modalOpenModel = writable(modalOpen)
</script>

<template>
  <Modal v-model="modalOpenModel" :title="editing ? `编辑入站 ${editing.tag}` : '新建入站'" width="560px">
    <InboundFormFields />
    <template #footer>
      <Button variant="ghost" @click="modalOpenModel = false">取消</Button>
      <Button variant="primary" :loading="saving" :success="saved" @click="save()">{{ editing ? '保存' : '创建' }}</Button>
    </template>
  </Modal>
</template>

<style scoped src="@/assets/styles/pages/admin/XrayInbounds.css"></style>
