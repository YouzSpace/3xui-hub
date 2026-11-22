<script setup>
/**
 * 订阅格式配置：管理订阅链接支持的格式（Clash、Sing-box 等）。
 */
import { ref, onMounted, onActivated } from 'vue'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Input from '@/components/ui/Input.vue'
import Icon from '@/components/ui/Icon.vue'
import { adminApi } from '@/api/admin'
import { useToast } from '@/components/ui/Toast.vue'

const toast = useToast()
const loading = ref(false)
const saving = ref(false)

const form = ref({
  sub_clash_enabled: '0',
  sub_singbox_enabled: '0',
  sub_show_traffic: '0',
  sub_show_expire: '0',
  sub_show_flag: '0',
  sub_show_userid: '0',
  sub_rename_enabled: '0',
  sub_rename_regex: '',
  sub_rename_replacement: '',
  sub_custom_info_enabled: '0',
  sub_custom_info_text: '',
  sub_title: '',
  sub_clash_template: '',
})

const load = async () => {
  loading.value = true
  try {
    const data = await adminApi.getSubscriptionSettings()
    form.value = { ...form.value, ...data }
  } catch (e) {
    toast.error('加载失败')
  } finally {
    loading.value = false
  }
}

const save = async () => {
  saving.value = true
  try {
    await adminApi.updateSubscriptionSettings(form.value)
    toast.success('保存成功')
  } catch (e) {
    toast.error(e.message || '保存失败')
  } finally {
    saving.value = false
  }
}

const toggleFormat = (key) => {
  form.value[key] = form.value[key] === '1' ? '0' : '1'
}

// 载入内置默认模板到编辑器（方便在默认基础上改），保存后即成为自定义模板。
const loadingTemplate = ref(false)
const loadDefaultTemplate = async () => {
  loadingTemplate.value = true
  try {
    const tmpl = await adminApi.getDefaultClashTemplate()
    form.value.sub_clash_template = typeof tmpl === 'string' ? tmpl : ''
  } catch (e) {
    toast.error('读取默认模板失败')
  } finally {
    loadingTemplate.value = false
  }
}

onMounted(load)
onActivated(load)
</script>

<template>
  <div class="sub-settings">
    <h1 class="sub-settings-title">订阅配置</h1>

    <Card>
      <div class="sub-settings-form">
        <!-- 订阅格式开关 -->
        <div class="sub-settings-section">
          <h3 class="sub-settings-section-title">订阅格式</h3>
          <p class="sub-settings-section-desc">开启后用户的订阅链接将支持对应的格式</p>

          <div class="sub-settings-formats">
            <div
              class="sub-settings-format-card"
              :class="{ 'sub-settings-format-card--active': form.sub_clash_enabled === '1' }"
              @click="toggleFormat('sub_clash_enabled')"
            >
              <div class="sub-settings-format-icon">
                <img src="/images/clash.png" alt="Clash" class="sub-settings-format-img" />
              </div>
              <div class="sub-settings-format-info">
                <span class="sub-settings-format-name">Clash / FlClash</span>
                <span class="sub-settings-format-desc">Clash YAML 格式</span>
              </div>
              <div class="sub-settings-format-toggle">
                <div class="sub-settings-toggle" :class="{ 'sub-settings-toggle--on': form.sub_clash_enabled === '1' }">
                  <div class="sub-settings-toggle-knob"></div>
                </div>
              </div>
            </div>

            <div
              class="sub-settings-format-card"
              :class="{ 'sub-settings-format-card--active': form.sub_singbox_enabled === '1' }"
              @click="toggleFormat('sub_singbox_enabled')"
            >
              <div class="sub-settings-format-icon">
                <img src="/images/singbox.svg" alt="Sing-box" class="sub-settings-format-img" />
              </div>
              <div class="sub-settings-format-info">
                <span class="sub-settings-format-name">Sing-box / NekoBox</span>
                <span class="sub-settings-format-desc">Sing-box JSON 格式</span>
              </div>
              <div class="sub-settings-format-toggle">
                <div class="sub-settings-toggle" :class="{ 'sub-settings-toggle--on': form.sub_singbox_enabled === '1' }">
                  <div class="sub-settings-toggle-knob"></div>
                </div>
              </div>
            </div>
          </div>
        </div>

        <!-- 订阅标题 -->
        <div class="sub-settings-section">
          <h3 class="sub-settings-section-title">订阅标题</h3>
          <p class="sub-settings-section-desc">客户端导入订阅后显示的名称（全站统一）</p>

          <div class="sub-settings-field">
            <Input
              v-model="form.sub_title"
              placeholder="留空则显示地址自带名称"
            />
            <p class="sub-settings-field-hint">留空则客户端显示订阅地址自带名称（即现在的一串数字）</p>
          </div>
        </div>

        <!-- 订阅内容控制 -->
        <div class="sub-settings-section">
          <h3 class="sub-settings-section-title">订阅内容</h3>
          <p class="sub-settings-section-desc">控制订阅链接中包含的信息</p>

          <div class="sub-settings-options">
            <label class="sub-settings-option">
              <input
                type="checkbox"
                :checked="form.sub_show_traffic === '1'"
                @change="form.sub_show_traffic = form.sub_show_traffic === '1' ? '0' : '1'"
              />
              <span>包含流量信息</span>
            </label>

            <label class="sub-settings-option">
              <input
                type="checkbox"
                :checked="form.sub_show_expire === '1'"
                @change="form.sub_show_expire = form.sub_show_expire === '1' ? '0' : '1'"
              />
              <span>包含到期时间</span>
            </label>
          </div>
        </div>

        <!-- 节点名称格式 -->
        <div class="sub-settings-section">
          <h3 class="sub-settings-section-title">节点名称</h3>
          <p class="sub-settings-section-desc">控制节点名称的显示方式</p>

          <div class="sub-settings-options">
            <label class="sub-settings-option">
              <input
                type="checkbox"
                :checked="form.sub_show_flag === '1'"
                @change="form.sub_show_flag = form.sub_show_flag === '1' ? '0' : '1'"
              />
              <span>显示旗帜（如：🇭🇰 HK01）</span>
            </label>

            <label class="sub-settings-option">
              <input
                type="checkbox"
                :checked="form.sub_show_userid === '1'"
                @change="form.sub_show_userid = form.sub_show_userid === '1' ? '0' : '1'"
              />
              <span>显示用户 ID</span>
            </label>
          </div>
        </div>

        <!-- 正则重命名 -->
        <div class="sub-settings-section">
          <h3 class="sub-settings-section-title">正则重命名</h3>
          <p class="sub-settings-section-desc">使用正则表达式修改节点名称（高级功能）</p>

          <div class="sub-settings-options">
            <label class="sub-settings-option">
              <input
                type="checkbox"
                :checked="form.sub_rename_enabled === '1'"
                @change="form.sub_rename_enabled = form.sub_rename_enabled === '1' ? '0' : '1'"
              />
              <span>启用正则重命名</span>
            </label>
          </div>

          <div v-if="form.sub_rename_enabled === '1'" class="sub-settings-rename">
            <div class="sub-settings-field">
              <label class="sub-settings-field-label">正则表达式</label>
              <Input
                v-model="form.sub_rename_regex"
                placeholder="/HK-Mobile-(.*)/"
              />
              <p class="sub-settings-field-hint">匹配节点名称的正则表达式，如 /HK-(.*)/ 匹配 HK 开头的名称</p>
            </div>

            <div class="sub-settings-field">
              <label class="sub-settings-field-label">替换为</label>
              <Input
                v-model="form.sub_rename_replacement"
                placeholder="🇭🇰 $1"
              />
              <p class="sub-settings-field-hint">替换后的内容，$1 代表第一个捕获组，$2 代表第二个，以此类推</p>
            </div>
          </div>
        </div>

        <!-- 自定义显示 -->
        <div class="sub-settings-section">
          <h3 class="sub-settings-section-title">自定义显示</h3>
          <p class="sub-settings-section-desc">随订阅下发的信息条目（公告/联系方式/使用说明），每行一条</p>

          <div class="sub-settings-options">
            <label class="sub-settings-option">
              <input
                type="checkbox"
                :checked="form.sub_custom_info_enabled === '1'"
                @change="form.sub_custom_info_enabled = form.sub_custom_info_enabled === '1' ? '0' : '1'"
              />
              <span>启用自定义显示</span>
            </label>
          </div>

          <div v-if="form.sub_custom_info_enabled === '1'" class="sub-settings-rename">
            <div class="sub-settings-field">
              <label class="sub-settings-field-label">信息内容</label>
              <textarea
                v-model="form.sub_custom_info_text"
                class="sub-settings-textarea"
                rows="5"
                placeholder="每行一条信息，空行忽略"
              ></textarea>
              <p class="sub-settings-field-hint">每行生成一个信息条目，排在真实节点之前，空行忽略</p>
            </div>
          </div>
        </div>

        <!-- Clash 模板 -->
        <div class="sub-settings-section">
          <h3 class="sub-settings-section-title">Clash 模板</h3>
          <p class="sub-settings-section-desc">
            自定义 Clash 订阅的结构（DNS、代理组、分流规则）。留空使用内置默认模板。
          </p>

          <div class="sub-settings-field">
            <textarea
              v-model="form.sub_clash_template"
              class="sub-settings-textarea sub-settings-textarea--mono"
              rows="14"
              spellcheck="false"
              placeholder="留空使用内置默认模板；点「载入默认模板」可以拿到当前默认内容再改"
            ></textarea>
            <p class="sub-settings-field-hint">
              占位符：<code v-pre>{{proxies}}</code> 节点列表（必须保留）、<code v-pre>{{app_name}}</code> 站点标题；
              代理组里写 <code v-pre>- "regex:香港|HK"</code> 会自动展开成匹配的节点名。
              保存前会检查 YAML 语法，写坏了会拦下来。
            </p>
          </div>

          <div class="sub-settings-inline-actions">
            <Button variant="secondary" size="sm" :disabled="loadingTemplate" @click="loadDefaultTemplate">
              <Icon name="refresh" :size="16" /> {{ loadingTemplate ? '读取中...' : '载入默认模板' }}
            </Button>
            <Button variant="ghost" size="sm" @click="form.sub_clash_template = ''">
              清空（用默认）
            </Button>
          </div>
        </div>

        <div class="sub-settings-actions">
          <Button variant="primary" size="sm" :disabled="saving" @click="save">
            <Icon name="check" :size="16" /> {{ saving ? '保存中...' : '保存' }}
          </Button>
        </div>
      </div>
    </Card>
  </div>
</template>

<style scoped src="@/assets/styles/pages/admin/SubscriptionSettings.css"></style>
