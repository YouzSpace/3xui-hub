<template>
  <div class="email-page">
    <div class="email-head">
      <h1 class="email-title">邮箱配置</h1>
      <span class="email-autosave-hint">修改自动保存</span>
    </div>

    <!-- 模块导航：设置 / 限制 / 定时 / 日志（?tab= 持久化到 URL，刷新不丢） -->
    <div class="email-tabs">
      <button
        v-for="t in tabs"
        :key="t.key"
        type="button"
        class="email-tab"
        :class="{ 'email-tab--active': tab === t.key }"
        @click="switchTab(t.key)"
      >
        {{ t.label }}
      </button>
    </div>

    <template v-if="tab === 'settings'">
      <Card class="email-card">
        <template #header><h2>注册验证方式</h2></template>
        <div class="email-switch-row">
          <span class="email-switch-label">启用邮箱验证注册</span>
          <label class="email-switch">
            <input v-model="form.register_email_verify" type="checkbox" class="email-switch-input" />
            <span class="email-switch-slider"></span>
          </label>
          <span class="email-switch-hint">{{ form.register_email_verify ? '邮箱验证' : '图形验证码' }}</span>

      </div>
      <p class="email-hint"><Badge variant="info">提示</Badge> 开启后，用户注册将通过邮箱接收验证码，关闭则使用图形验证码。</p>
    </Card>

      <!-- 其余卡片（发送测试/SMTP/模板/批量发信）两栏均分，归「设置」Tab，仍受邮箱验证开关控制 -->
      <template v-if="form.register_email_verify">
      <div class="email-cols">

        <div class="email-col">

          <Card class="email-card">
            <template #header><h2>SMTP 服务器</h2></template>
            <div class="email-grid">
              <div>
                <label class="email-label">SMTP 服务器</label>
                <Input v-model="form.smtp_host" placeholder="smtp.example.com" required />
              </div>
              <div>
                <label class="email-label">端口</label>
                <Input v-model="form.smtp_port" type="number" placeholder="465" />
              </div>
              <div>
                <label class="email-label">加密方式</label>
                <Select v-model="form.smtp_encryption" :options="encryptionOptions" />
              </div>
              <div>
                <label class="email-label">发件邮箱</label>
                <Input v-model="form.smtp_username" placeholder="noreply@example.com" required />
                <p class="email-rate-desc">发件地址与邮箱账号相同（邮箱服务商要求）</p>
              </div>
              <div>
                <label class="email-label">授权码</label>
                <div class="email-pwd">
                  <Input
                    v-model="form.smtp_password"
                    :type="smtpPwdVisible ? 'text' : 'password'"
                    placeholder="QQ 邮箱需用授权码"
                    required
                  />
                  <button
                    type="button"
                    class="email-pwd-toggle"
                    :class="{ 'email-pwd-toggle--on': smtpPwdVisible }"
                    :title="smtpPwdVisible ? '隐藏密码' : '显示密码'"
                    :aria-label="smtpPwdVisible ? '隐藏密码' : '显示密码'"
                    :aria-pressed="smtpPwdVisible"
                    @click="smtpPwdVisible = !smtpPwdVisible"
                  >
                    <Icon name="eye-off" :size="16" />
                  </button>
                </div>
              </div>
              <div>
                <label class="email-label">发件名称</label>
                <Input v-model="form.smtp_from_name" placeholder="ControlHub" />
              </div>
            </div>
          </Card>

          <Card class="email-card">
            <template #header><h2>发送测试</h2></template>
            <div class="email-test-row">
              <div class="flex-1">
                <label class="email-label">收件邮箱</label>
                <Input v-model="testTo" placeholder="填入你的邮箱测试" />
              </div>
              <div class="email-test-account">
                <label class="email-label">用哪个账号发</label>
                <Select v-model="testAccount" :options="testAccountOptions" />
              </div>
              <Button :loading="loading" @click="onTest" class="self-end">发送测试</Button>
            </div>
            <p class="email-rate-desc">选「站点默认账号」= 用上方 SMTP 配置发；选池里的号 = 用该号发，可用来验证池号是否真的通。</p>
          </Card>

          <Card class="email-card">
            <template #header><h2>邮件模板</h2></template>
            <p class="email-hint"><Badge variant="info">提示</Badge> <code v-pre>{{code}}</code> 会自动替换为验证码。</p>
            <textarea v-model="form.email_template" class="email-template" rows="6" placeholder='<div style="padding:20px"><h2>验证码</h2><p style="font-size:24px;color:#2563eb">{{code}}</p></div>'></textarea>
          </Card>
        </div>

        <div class="email-col">

          <Card class="email-card">
            <template #header><h2>批量发信</h2></template>
            <p class="email-hint">
              <Badge variant="warning">注意</Badge>
              群发会立刻进入发送队列，无法撤回。发信前建议先「单发」给自己试一封。
            </p>

            <div class="email-grid">
              <div>
                <label class="email-label">发送方式</label>
                <Select v-model="batch.mode" :options="sendModeOptions" />
              </div>
              <div v-if="batch.mode === 'single'">
                <label class="email-label">收件人（邮箱或用户 ID）</label>
                <Input v-model="batch.single_query" placeholder="a@example.com 或 12" />
              </div>
              <template v-else>
                <div>
                  <label class="email-label">用户状态</label>
                  <Select v-model="batch.user_status" :options="userStatusOptions" />
                </div>
                <div>
                  <label class="email-label">限定套餐（可选）</label>
                  <Select v-model="batch.plan_id" :options="batchPlanOptions" />
                </div>
              </template>
            </div>

            <div class="email-mt">
              <label class="email-label">邮件标题</label>
              <Input v-model="batch.subject" placeholder="例如：您的套餐即将到期" />
            </div>

            <div class="email-vars">
              <span class="email-vars-label">正文可用变量：</span>
              <code v-for="v in VARS" :key="v" class="email-var">{{ v }}</code>
            </div>

            <textarea
              v-model="batch.body"
              class="email-template"
              rows="6"
              placeholder="邮件内容，支持上方变量"
            ></textarea>

            <div v-if="batchResult" class="email-result">
              <p class="email-hint">
                <Badge variant="success">已入队</Badge>
                批次 <code>{{ batchResult.batch_id }}</code>，共 {{ batchResult.total }} 封<template v-if="batchResult.rate_limited">（每分钟 {{ batchResult.rate_limited }} 封）</template>。
              </p>

              <!-- 预计时长：入队时后端算好（两套限速里更慢的那个）。发完后不再显示，
                   免得「发送完成」旁边还挂一句还能发多久。 -->
              <p v-if="!batchDone && batchEstimateText" class="email-hint email-estimate">
                <Badge variant="info">预计</Badge>
                {{ batchEstimateText }}
              </p>

              <div v-if="batchProgress" class="email-progress">
                <div class="email-progress-head">
                  <span class="email-progress-count">已发 {{ batchProgress.sent }} / {{ batchProgress.total }}</span>
                  <span v-if="batchProgress.failed > 0" class="email-progress-failed">
                    失败 {{ batchProgress.failed }} 封
                  </span>
                </div>
                <div
                  class="email-progress-track"
                  role="progressbar"
                  :aria-valuenow="batchPercent"
                  aria-valuemin="0"
                  aria-valuemax="100"
                >
                  <span class="email-progress-fill" :style="{ width: batchPercent + '%' }"></span>
                </div>
                <p v-if="batchDone" class="email-progress-done">
                  发送完成（成功 {{ batchProgress.sent }} / 失败 {{ batchProgress.failed }}）
                </p>
                <p v-if="batchProgress.failed > 0" class="email-progress-hint email-progress-hint--failed">
                  失败 {{ batchProgress.failed }} 封，去「日志」Tab 看原因
                </p>
                <p class="email-progress-hint">进度可在「日志」Tab 查看。</p>
              </div>
            </div>

            <div class="email-save-wrap">
              <Button :loading="batchSending" @click="onBatchSend">
                {{ batch.mode === 'single' ? '发送' : '加入队列群发' }}
              </Button>
            </div>
          </Card>
        </div>

      </div>
      </template>
    </template>

    <!-- 限制 Tab：安全限制独立于「邮箱验证」开关（防刷限流常驻生效），始终显示 -->
    <template v-if="tab === 'limits'">
      <Card class="email-card">
        <template #header><h2>发信速度限制</h2></template>
        <p class="email-hint"><Badge variant="info">提示</Badge> 控制群发节奏，防止一次发太多被邮箱服务商判定为垃圾邮件。「批量发信」和「按月定时发信」共用这一份设置。</p>
        <div class="email-switch-row">
          <span class="email-switch-label">限制发送速度</span>
          <label class="email-switch">
            <input v-model="form.mail_rate_limit_enabled" type="checkbox" class="email-switch-input" />
            <span class="email-switch-slider"></span>
          </label>
          <span class="email-switch-hint">{{ form.mail_rate_limit_enabled ? '每分钟 ' + form.mail_rate_per_minute + ' 封' : '不限制，一次性全部排队' }}</span>
        </div>
        <div v-if="form.mail_rate_limit_enabled" class="email-grid">
          <div>
            <label class="email-label">每分钟最多发几封</label>
            <Input v-model="form.mail_rate_per_minute" type="number" min="1" max="600" placeholder="30" />
            <p class="email-rate-desc">超出部分自动排到后面的分钟，发得慢但不会被邮箱服务商拦</p>
          </div>
        </div>
        <div class="email-grid">
          <div>
            <label class="email-label">每封间隔（毫秒）</label>
            <Input v-model="form.mail_send_interval_ms" type="number" min="0" placeholder="0" />
            <p class="email-rate-desc">两封信之间至少等多久，填 <code>0</code> 表示不等。它和上面的「每分钟最多发几封」<strong>同时生效，谁更慢就按谁</strong> —— 想发得更快，先把这个调小。</p>
          </div>
        </div>
        <p class="email-hint effective-rate">
          <Badge variant="info">当前实际</Badge>
          <template v-if="effectivePerMinute > 0">最多约 <strong>{{ effectivePerMinute }}</strong> 封/分钟（两套设置取更慢的那个）</template>
          <template v-else>不限速，按邮箱服务器能接受的速度尽快发</template>
        </p>
      </Card>

      <Card class="email-card">
        <template #header><h2>安全限制</h2></template>
        <p class="email-hint"><Badge variant="info">提示</Badge> 防止接口被刷。填 <code>0</code> 表示这一项不限制。</p>
        <div class="email-grid">
          <div class="email-rate-item">
            <label class="email-label">发码间隔（秒）</label>
            <Input v-model="form.rate_code_interval" type="number" placeholder="60" />
            <p class="email-rate-desc">同一邮箱两次收验证码的最小间隔（找回密码 + 注册验证共用）</p>
          </div>
          <div class="email-rate-item">
            <label class="email-label">每日发码上限（次）</label>
            <Input v-model="form.rate_code_per_day" type="number" placeholder="10" />
            <p class="email-rate-desc">同一邮箱 24 小时内最多收几次验证码（找回密码 + 注册验证共用）</p>
          </div>
          <div class="email-rate-item">
            <label class="email-label">发码 IP 上限（次/小时）</label>
            <Input v-model="form.rate_code_ip_hourly" type="number" placeholder="20" />
            <p class="email-rate-desc">同一 IP 每小时最多请求几次发码（找回密码 + 注册合计）</p>
          </div>
          <div class="email-rate-item">
            <label class="email-label">验证码试错上限（次）</label>
            <Input v-model="form.rate_verify_max_attempts" type="number" placeholder="10" />
            <p class="email-rate-desc">同一邮箱错几次验证码后被锁定</p>
          </div>
          <div class="email-rate-item">
            <label class="email-label">试错锁定时长（秒）</label>
            <Input v-model="form.rate_verify_lock_seconds" type="number" placeholder="300" />
            <p class="email-rate-desc">试错超限后锁多久，期间正确验证码也不放行</p>
          </div>
          <div class="email-rate-item">
            <label class="email-label">登录失败上限（次）</label>
            <Input v-model="form.rate_login_max_attempts" type="number" placeholder="5" />
            <p class="email-rate-desc">邮箱密码登录错几次后锁定</p>
          </div>
          <div class="email-rate-item">
            <label class="email-label">登录锁定时长（秒）</label>
            <Input v-model="form.rate_login_lock_seconds" type="number" placeholder="300" />
            <p class="email-rate-desc">登录锁定后多久可以重试</p>
          </div>
          <div class="email-rate-item">
            <label class="email-label">注册 IP 上限（个/小时）</label>
            <Input v-model="form.rate_register_ip_hourly" type="number" placeholder="10" />
            <p class="email-rate-desc">同一 IP 每小时最多注册几个账号</p>
          </div>
        </div>
      </Card>
    </template>

    <!-- 账号池 Tab：多 SMTP 号轮换发信。池里有可用号就轮换，池空回落「设置」页的单套配置 -->
    <template v-if="tab === 'pool'">
      <Card class="email-card email-card-full">
        <template #header>
          <div class="email-pool-head">
            <h2>SMTP 账号池</h2>
            <Button size="sm" @click="openPoolCreate">
              <Icon name="plus" :size="16" />
              新增账号
            </Button>
          </div>
        </template>
        <p class="email-hint">
          <Badge variant="info">提示</Badge>
          发信时按「最久没用过的先上」轮换挑号；某个号认证失败会自动停用、限流/超时则冷却 10 分钟并换下一个（单封最多换 3 个号）。
          池里没有可用号时回落「设置」页的站点默认账号，所以配错号不会让发信彻底停摆。
        </p>

        <p v-if="poolLoading" class="email-empty">加载中…</p>
        <p v-else-if="pool.length === 0" class="email-empty">
          还没有账号，发信全部走「设置」页的站点默认账号。点右上角「新增账号」把号加进来。
        </p>

        <div v-else class="email-table-wrap">
          <table class="email-table email-pool-table">
            <thead>
              <tr>
                <th>账号（host:port）</th>
                <th>用户名</th>
                <th>日限额</th>
                <th>今日已发</th>
                <th>状态</th>
                <th>冷却到</th>
                <th>连续失败</th>
                <th class="email-pool-actions-col">操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="a in pool" :key="a.id" :data-account-id="a.id">
                <td class="mono">{{ a.host }}:{{ a.port }}</td>
                <td class="mono email-pool-user" :title="a.username">{{ a.username || '—' }}</td>
                <td>{{ poolLimitText(a) }}</td>
                <td>
                  {{ sentTodayOf(a) }}
                  <span v-if="a.daily_limit > 0" class="email-pool-of">/ {{ a.daily_limit }}</span>
                </td>
                <td>
                  <Badge :variant="poolStatusVariant[a.status] || 'info'">
                    {{ poolStatusText[a.status] || a.status }}
                  </Badge>
                </td>
                <td class="mono">{{ cooldownText(a) }}</td>
                <td>
                  <span :class="{ 'email-pool-err': a.error_count > 0 }">{{ a.error_count }}</span>
                </td>
                <td>
                  <div class="email-pool-actions">
                    <Button variant="ghost" size="sm" @click="openPoolEdit(a)">编辑</Button>
                    <Button
                      variant="ghost"
                      size="sm"
                      @click="togglePoolStatus(a)"
                    >{{ a.status === 'active' ? '停用' : '启用' }}</Button>
                    <Button variant="ghost" size="sm" @click="resetToday(a)">重置今日</Button>
                    <Button variant="ghost" size="sm" @click="removePool(a)">删除</Button>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <p class="email-rate-desc">
          冷却中的号到期会自动恢复；「启用/停用」是手动开关，停用后该号不再参与挑号。「重置今日」只清今日已发与连续失败，不动状态和冷却。
        </p>
      </Card>
    </template>

    <!-- 定时 Tab：自动通知（后台每 5 分钟扫描任务，场景开关 + 阈值）；原在邮箱验证开关控制内，gate 保持 -->
    <template v-if="tab === 'scheduled'">
      <template v-if="form.register_email_verify">
      <Card class="email-card email-card-full">
            <template #header><h2>自动通知</h2></template>
              <p class="email-hint">
                <Badge variant="info">提示</Badge>
                所有通知<strong>默认全部关闭</strong>，按需单独打开。收件人可以选用户本人或管理员自己。
              </p>

              <div class="email-grid email-grid--3">
                <div>
                  <label class="email-label">管理员接收邮箱</label>
                  <Input v-model="notify.notify_admin_email" placeholder="选「发给管理员」时收件到这个地址" />
                </div>
                <div>
                  <label class="email-label">流量即将用尽阈值（%）</label>
                  <Input v-model="notify.notify_traffic_almost_threshold" type="number" min="1" max="100" placeholder="90" />
                  <p class="email-rate-desc">用量达到套餐的百分之多少时提醒</p>
                </div>
                <div>
                  <label class="email-label">到期提醒提前天数</label>
                  <Input v-model="notify.notify_expiring_days" type="number" min="0" max="365" placeholder="3" />
                  <p class="email-rate-desc">套餐到期前几天开始提醒续费</p>
                </div>
              </div>

              <div class="email-vars">
                <span class="email-vars-label">正文可用变量：</span>
                <code v-for="v in VARS" :key="v" class="email-var">{{ v }}</code>
              </div>

              <div class="email-scenes">
                <div v-for="s in SCENES" :key="s.key" class="email-scene">
                  <div class="email-switch-row">
                    <span class="email-switch-label">{{ s.name }}</span>
                    <label class="email-switch">
                      <input v-if="sceneCfg[s.key]" v-model="sceneCfg[s.key].enabled" type="checkbox" class="email-switch-input" />
                      <span class="email-switch-slider"></span>
                    </label>
                    <Select v-if="sceneCfg[s.key]" v-model="sceneCfg[s.key].to" :options="recipientOptions" size="sm" />
                  </div>
                  <p class="email-rate-desc">{{ s.desc }}</p>
                  <template v-if="sceneCfg[s.key]?.enabled">
                    <div>
                      <label class="email-label">邮件标题</label>
                      <Input v-model="sceneCfg[s.key].title" placeholder="留空则用默认标题" />
                    </div>
                    <textarea
                      v-model="sceneCfg[s.key].body"
                      class="email-template email-scene-body"
                      rows="4"
                      placeholder="留空则用默认内容，可用上方变量"
                    ></textarea>
                  </template>
                </div>
              </div>

          </Card>
      </template>
      <p v-else class="email-empty">邮箱验证开关关闭时自动通知不生效——去「设置」页打开开关后再配置</p>

      <!-- 按月定时发信：与邮箱验证开关无关（管理员主动配置，配了才发），始终显示 -->
      <Card class="email-card email-card-full">
        <template #header><h2>按月定时发信</h2></template>
        <p class="email-hint">
          <Badge variant="info">提示</Badge>
          选好内容和发送时间后，每月到点系统自动发一封（同自动通知一样走 5 分钟扫描任务，每个自然月只发一轮）。
          收件人与内容同「批量发信」一套；发送速度按「限制」页的统一设置。默认关闭。
        </p>

        <div class="email-switch-row">
          <span class="email-switch-label">启用按月定时发信</span>
          <label class="email-switch">
            <input v-model="schedule.enabled" type="checkbox" class="email-switch-input" />
            <span class="email-switch-slider"></span>
          </label>
          <span class="email-switch-hint">关掉后到点不发，配置保留</span>
        </div>

        <div class="email-grid">
          <div>
            <label class="email-label">每月第几天</label>
            <Input v-model="schedule.day" type="number" min="1" max="31" placeholder="1" />
            <p class="email-rate-desc">当月没有这一天（如 2 月的 31 号）就跳过，下个月再发</p>
          </div>
          <div>
            <label class="email-label">发送时间（24 小时制）</label>
            <Input v-model="schedule.time" type="time" />
            <p class="email-rate-desc">到点后 5 分钟内发出</p>
          </div>
          <div>
            <label class="email-label">发送方式</label>
            <Select v-model="schedule.config.mode" :options="sendModeOptions" />
          </div>
          <div v-if="schedule.config.mode === 'single'">
            <label class="email-label">收件人（邮箱或用户 ID）</label>
            <Input v-model="schedule.config.single_query" placeholder="a@example.com 或 12" />
          </div>
          <template v-else>
            <div>
              <label class="email-label">用户状态</label>
              <Select v-model="schedule.config.user_status" :options="userStatusOptions" />
            </div>
            <div>
              <label class="email-label">限定套餐（可选）</label>
              <Select v-model="schedule.config.plan_id" :options="batchPlanOptions" />
            </div>
          </template>
        </div>

        <div class="email-mt">
          <label class="email-label">邮件标题</label>
          <Input v-model="schedule.config.subject" placeholder="例如：月度服务报告" />
        </div>
        <textarea
          v-model="schedule.config.body"
          class="email-template email-mt"
          rows="5"
          placeholder="邮件内容，支持上方变量"
        ></textarea>

        <p v-if="schedule.enabled && (!schedule.config.subject || !schedule.config.body)" class="email-warn-note">
          标题或内容为空，到点不会发送；填上后自动保存。
        </p>
      </Card>
    </template>

    <!-- 日志 Tab：发信记录，独立显示（不受邮箱验证开关控制） -->
    <template v-if="tab === 'logs'">
      <Card class="email-card email-card-full">
        <template #header><h2>发信日志</h2></template>
        <div class="email-grid">
          <div>
            <label class="email-label">类型</label>
            <Select v-model="logFilter.type" :options="logTypeOptions" @change="logPage = 1; loadLogs()" />
          </div>
          <div>
            <label class="email-label">状态</label>
            <Select v-model="logFilter.status" :options="logStatusOptions" @change="logPage = 1; loadLogs()" />
          </div>
        </div>

        <p v-if="!logsLoading && logs.length === 0" class="email-empty">还没有发信记录</p>

        <div v-else class="email-table-wrap">
          <table class="email-table">
            <thead>
              <tr>
                <th>时间</th><th>类型</th><th>场景</th><th>收件人</th><th>标题</th><th>状态</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="l in logs" :key="l.id">
                <td class="mono">{{ new Date(l.created_at).toLocaleString('zh-CN', { hour12: false }) }}</td>
                <td>{{ logTypeText[l.type] || l.type }}</td>
                <td>{{ l.scene ? (logSceneText[l.scene] || l.scene) : '—' }}</td>
                <td class="mono email-log-to" :title="l.to_email">{{ l.to_email }}</td>
                <td class="email-log-subject" :title="l.subject">{{ l.subject }}</td>
                <td>
                  <Badge :variant="l.status === 'sent' ? 'success' : 'danger'">{{ logStatusText[l.status] || l.status }}</Badge>
                  <p v-if="l.error" class="email-log-error" :title="l.error">{{ l.error }}</p>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <div v-if="logPages > 1" class="email-log-pager">
          <Button variant="secondary" :disabled="logPage <= 1" @click="logPage--; loadLogs()">上一页</Button>
          <span class="email-pager-info">第 {{ logPage }} / {{ logPages }} 页 · 共 {{ logTotal }} 条</span>
          <Button variant="secondary" :disabled="logPage >= logPages" @click="logPage++; loadLogs()">下一页</Button>
        </div>
      </Card>
    </template>

    <!-- 账号池新增/编辑弹窗：就地改，不跳单独设置页 -->
    <Modal
      v-model="poolModal"
      :title="poolEditing ? '编辑 SMTP 账号' : '新增 SMTP 账号'"
      width="520px"
    >
      <div class="email-grid">
        <div>
          <label class="email-label">SMTP 服务器</label>
          <Input v-model="poolForm.host" placeholder="smtp.example.com" mono />
        </div>
        <div>
          <label class="email-label">端口</label>
          <Input v-model="poolForm.port" type="number" min="1" max="65535" placeholder="587" />
        </div>
        <div>
          <label class="email-label">用户名 / 发件邮箱</label>
          <Input v-model="poolForm.username" placeholder="noreply@example.com" mono />
        </div>
        <div>
          <label class="email-label">加密方式</label>
          <Select v-model="poolForm.encryption" :options="encryptionOptions" />
        </div>
        <div>
          <label class="email-label">{{ poolEditing ? '密码 / 授权码（留空 = 不修改）' : '密码 / 授权码' }}</label>
          <div class="email-pwd">
            <Input
              v-model="poolForm.password"
              :type="poolPwdVisible ? 'text' : 'password'"
              :placeholder="poolEditing ? '留空保持原密码' : 'QQ 邮箱需用授权码'"
            />
            <button
              type="button"
              class="email-pwd-toggle"
              :class="{ 'email-pwd-toggle--on': poolPwdVisible }"
              :title="poolPwdVisible ? '隐藏密码' : '显示密码'"
              :aria-label="poolPwdVisible ? '隐藏密码' : '显示密码'"
              :aria-pressed="poolPwdVisible"
              @click="poolPwdVisible = !poolPwdVisible"
            >
              <Icon name="eye-off" :size="16" />
            </button>
          </div>
        </div>
        <div>
          <label class="email-label">发件名称</label>
          <Input v-model="poolForm.from_name" placeholder="留空用站点名称" />
        </div>
        <div>
          <label class="email-label">日限额（封）</label>
          <Input v-model="poolForm.daily_limit" type="number" min="0" placeholder="0" />
          <p class="email-rate-desc">该号每天最多发几封，<code>0</code> = 不限。发满后当天不再挑它。</p>
        </div>
      </div>
      <template #footer>
        <Button variant="secondary" size="sm" @click="poolModal = false">取消</Button>
        <Button size="sm" :loading="poolSaving" @click="savePool">保存</Button>
      </template>
    </Modal>
  </div>
</template>

<script setup>
import { ref, computed, watch, onMounted, onActivated, onDeactivated, onUnmounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useToast } from '@/components/ui/Toast.vue'
import { adminApi } from '@/api/admin'
import Card from '@/components/ui/Card.vue'
import Button from '@/components/ui/Button.vue'
import Input from '@/components/ui/Input.vue'
import Select from '@/components/ui/Select.vue'
import Badge from '@/components/ui/Badge.vue'
import Icon from '@/components/ui/Icon.vue'
import Modal from '@/components/ui/Modal.vue'

const toast = useToast()
const route = useRoute()
const router = useRouter()

// 模块导航：设置 / 限制 / 账号池 / 定时 / 日志。当前 Tab 走 URL ?tab= 持久化，刷新/切走再回来不丢。
const tabs = [
  { key: 'settings', label: '设置' },
  { key: 'limits', label: '限制' },
  { key: 'pool', label: '账号池' },
  { key: 'scheduled', label: '定时' },
  { key: 'logs', label: '日志' },
]
const TAB_KEYS = tabs.map((t) => t.key)
const tab = ref(TAB_KEYS.includes(route.query.tab) ? route.query.tab : 'settings')

const switchTab = (key) => {
  if (tab.value === key) return
  tab.value = key
  router.replace({ query: { ...route.query, tab: key } })
  // 日志是持续增长的数据：每次切进「日志」Tab 都拉最新。
  // 否则同页内刚发完信切过来，看到的还是进页面时的旧列表（会以为没记录）。
  if (key === 'logs') {
    logPage.value = 1
    loadLogs()
  }
  // 账号池同理：别处刚加过号，切过来要看到最新的
  if (key === 'pool') loadPool()
  // 站内切 Tab 不触发 keep-alive 生命周期，进度轮询得在这里单独停/续：
  // 「设置」页之外看不到进度条，还轮询就是白打接口。
  if (key !== 'settings') {
    stopBatchPolling()
  } else {
    resumeBatchPolling()
  }
}

// 限流默认值：与后端 config/panel.php 的 ratelimit 段、后台校验规则保持一致。
// 后端没配过时 show() 回的是空串，空串会被 load() 补回这里的默认值（空串提交回去也合法，但输入框会空着）。
const RATE_DEFAULTS = {
  rate_code_interval: 60,
  rate_code_per_day: 10,
  rate_code_ip_hourly: 20,
  rate_verify_max_attempts: 10,
  rate_verify_lock_seconds: 300,
  rate_login_max_attempts: 5,
  rate_login_lock_seconds: 300,
  rate_register_ip_hourly: 10,
}

const form = ref({
  smtp_host: '',
  smtp_port: '465',
  smtp_encryption: 'ssl',
  smtp_username: '',
  smtp_password: '',
  smtp_from_name: 'ControlHub',
  register_email_verify: false,
  email_template: '',
  // 发信速度限制（「限制」页配置，批量与定时共用）
  mail_rate_limit_enabled: false,
  mail_rate_per_minute: 30,
  // 串行 worker 两封之间的固定间隔（毫秒，0 = 不等）
  mail_send_interval_ms: 0,
  ...RATE_DEFAULTS,
})

// 两套限速旋钮同时生效，实际速率由更慢的那个决定（与后端 estimate_seconds 同一口径）：
// 每分钟 N 封 → 每封等效 60/N 秒；mail_send_interval_ms → 两封之间的固定等待。
const effectivePerMinute = computed(() => {
  let perMailSec = (Number(form.value.mail_send_interval_ms) || 0) / 1000
  if (form.value.mail_rate_limit_enabled) {
    const rate = Number(form.value.mail_rate_per_minute) || 0
    if (rate > 0) perMailSec = Math.max(perMailSec, 60 / rate)
  }
  if (perMailSec <= 0) return 0
  return Math.max(1, Math.round(60 / perMailSec))
})

const testTo = ref('')
// 授权码「显示/隐藏」：纯 UI 状态，刻意不放进 form —— 它若进了 form，
// 点眼睛就会让 JSON 快照变化，触发一次毫无意义的自动保存。
const smtpPwdVisible = ref(false)
const loading = ref(false)
// 读取已存的邮件配置（老 UI 行为：读失败不阻塞页面）。
// 不做「加载失败就禁用保存」的前端门禁——后端 save() 已挡空值覆盖，
// 前端再拦一层只会误伤正常操作。
const load = async () => {
  // 同一份配置同时只允许一个加载在飞（keep-alive 下 onMounted/onActivated 会重复触发）
  if (formLoading) return
  formLoading = true
  const seq = formSaveSeq
  try {
    const data = await adminApi.getEmail()
    // 响应到达时若本地已有未保存修改，或本轮请求期间已成功保存过 → 服务器数据是旧的，丢弃
    if (data && !formDirty && seq === formSaveSeq) {
      Object.assign(form.value, data)
      form.value.register_email_verify = data.register_email_verify === true || data.register_email_verify === "1" || data.register_email_verify === 1
      // 后端没存过限流项 → 回空串，这里显示成默认值（0 是有效值，不能被当成空）
      for (const key of Object.keys(RATE_DEFAULTS)) {
        if (form.value[key] === '' || form.value[key] === null || form.value[key] === undefined) {
          form.value[key] = RATE_DEFAULTS[key]
        }
      }
      // 发信速度：后端未配置回空串——开关归一到布尔、速率回落默认 30
      form.value.mail_rate_limit_enabled = data.mail_rate_limit_enabled === true || data.mail_rate_limit_enabled === '1' || data.mail_rate_limit_enabled === 1
      if (form.value.mail_rate_per_minute === '' || form.value.mail_rate_per_minute === null || form.value.mail_rate_per_minute === undefined) {
        form.value.mail_rate_per_minute = 30
      }
      // 发信间隔：没配过回空串 = 0（不等）。0 是有效值，不能被当成空留成默认以外的值。
      if (form.value.mail_send_interval_ms === '' || form.value.mail_send_interval_ms === null || form.value.mail_send_interval_ms === undefined) {
        form.value.mail_send_interval_ms = 0
      }
      savedFormSnap = JSON.stringify(form.value)   // 自动保存基线：加载出来的值不算「修改」
    }
  } catch (e) {
    // 读取失败保持静默：后端 save() 已挡空值覆盖（见 EmailController::save 保护逻辑），
    // 这里读到空值也不会清空库里已存的配置，前端不再做禁用门禁以免误伤正常操作。
  } finally {
    formLoading = false
  }
}

// ============ 修改自动保存 ============
// 本页全部配置（设置/限制、自动通知、按月定时发信）改为「改完即存」：深度监听
// + 800ms 防抖（停手后才提交，输入过程不打扰）；用 JSON 快照与「上次已保存值」比对，
// 加载/回读赋值不算修改；保存成功提示 1.5 秒内合并一次，连续修改不刷屏。
let savedFormSnap = ''
let savedNotifySnap = ''
let savedScheduleSnap = ''
let formTimer = null
let notifyTimer = null
let scheduleTimer = null
let lastAutoSaveToastAt = 0

// ===== 加载安全（修「滑动开关打开了却没保存 / 限制发送没生效」）=====
// 本页是 keep-alive 页面：首次进入 onMounted 与 onActivated 会各触发一次加载，
// 且加载响应可能比用户的手速更晚到达。迟到的响应会把刚打开/刚填好的值覆盖回服务器
// 旧值，随后自动保存又把这个旧值提交回去 —— 表现就是「开关状态没保存」。
// 两道闸：
//   ①同一份配置同时只允许一个加载在飞（去掉重复请求）；
//   ②响应到达时若本地已有未保存修改(formDirty) 或 已晚于之后的成功保存(序号变了)
//     → 直接丢弃这份响应，绝不覆盖用户的改动。
let formLoading = false
let formDirty = false
let formSaveSeq = 0
let notifyLoading = false
let notifyDirty = false
let notifySaveSeq = 0
let scheduleLoading = false
let scheduleDirty = false
let scheduleSaveSeq = 0

const autoSaveToast = () => {
  const now = Date.now()
  if (now - lastAutoSaveToastAt > 1500) {
    toast.success('已自动保存')
    lastAutoSaveToastAt = now
  }
}

const saveForm = async () => {
  const snap = JSON.stringify(form.value)
  if (snap === savedFormSnap) return
  try {
    // 序号先自增：这个请求一旦发出，任何更早发出的加载响应都作废，不许再覆盖本地
    formSaveSeq++
    await adminApi.saveEmail(form.value)
    savedFormSnap = snap
    // 保存成功且本地仍是提交时那份 → 无未保存修改；提交后又被改过则保持脏标记
    formDirty = JSON.stringify(form.value) !== snap
    autoSaveToast()
  } catch (e) {
    toast.error(e?.message || '保存失败')
  }
}

const onTest = async () => {
  if (!testTo.value) return toast.error('请填写收件邮箱')
  loading.value = true
  try {
    // 先保存当前配置（同步基线，避免自动保存重复提交），再用保存的值测试
    await adminApi.saveEmail(form.value)
    savedFormSnap = JSON.stringify(form.value)
    // 选了池号就用池号发（不选 = 站点默认账号）；空串按「不选」处理
    await adminApi.testEmail(testTo.value, testAccount.value || undefined)
    toast.success('已发送，请检查邮箱（含垃圾箱）')
  } catch (e) {
    toast.error(e?.message || '发送失败')
  } finally {
    loading.value = false
  }
}

// ============ SMTP 账号池 ============
// 池里有可用号就轮换着发，池空/全不可用回落站点默认账号（后端 deliverWithPool）。
// weight 字段刻意不做进界面：挑号逻辑（selectSMTPAccount）只按 last_used_at，
// 填了也不影响发信，摆出来只会让人以为它有用。
const pool = ref([])
const poolLoading = ref(false)
const poolModal = ref(false)
const poolSaving = ref(false)
const poolEditing = ref(null)          // null = 新增
const poolPwdVisible = ref(false)
// 弹窗里的表单；id 与 has_password 不参与提交，单独放在 poolEditing 里
const poolForm = ref({
  host: '',
  port: 587,
  username: '',
  password: '',
  encryption: 'tls',
  from_name: '',
  daily_limit: 0,
})

const resetPoolForm = (a) => {
  poolPwdVisible.value = false
  if (!a) {
    poolForm.value = { host: '', port: 587, username: '', password: '', encryption: 'tls', from_name: '', daily_limit: 0 }
    return
  }
  poolForm.value = {
    host: a.host || '',
    port: a.port || 587,
    username: a.username || '',
    // 密码不回显：留空 = 保持原密码（后端同口径）
    password: '',
    encryption: a.encryption || 'tls',
    from_name: a.from_name || '',
    daily_limit: a.daily_limit ?? 0,
  }
}

const loadPool = async () => {
  if (poolLoading.value) return
  poolLoading.value = true
  try {
    const res = await adminApi.listSmtpAccounts()
    // 列表端点回 data:{items:[…]}（不是分页结构，拦截器解包后就是对象）
    pool.value = Array.isArray(res?.items) ? res.items : Array.isArray(res) ? res : []
  } catch (e) {
    pool.value = []
  } finally {
    poolLoading.value = false
  }
}

const openPoolCreate = () => {
  poolEditing.value = null
  resetPoolForm(null)
  poolModal.value = true
}

const openPoolEdit = (a) => {
  poolEditing.value = a
  resetPoolForm(a)
  poolModal.value = true
}

const savePool = async () => {
  const f = poolForm.value
  if (!String(f.host || '').trim()) return toast.error('请填写 SMTP 服务器地址')
  const payload = {
    host: String(f.host).trim(),
    port: Number(f.port) || 587,
    username: String(f.username || '').trim(),
    encryption: f.encryption || 'tls',
    from_name: String(f.from_name || '').trim(),
    daily_limit: Number(f.daily_limit) || 0,
  }
  // 密码留空 = 不修改（编辑时）；新增时留空也允许（有些内网中继不认证）
  if (String(f.password || '') !== '') payload.password = String(f.password)
  poolSaving.value = true
  try {
    if (poolEditing.value) {
      await adminApi.updateSmtpAccount(poolEditing.value.id, payload)
      toast.success('已保存')
    } else {
      await adminApi.createSmtpAccount(payload)
      toast.success('已添加')
    }
    poolModal.value = false
    await loadPool()
  } catch (e) {
    toast.error(e?.message || '保存失败')
  } finally {
    poolSaving.value = false
  }
}

const togglePoolStatus = async (a) => {
  const next = a.status === 'active' ? 'disabled' : 'active'
  try {
    await adminApi.setSmtpAccountStatus(a.id, next)
    toast.success(next === 'active' ? '已启用' : '已停用')
    await loadPool()
  } catch (e) {
    toast.error(e?.message || '操作失败')
  }
}

const resetToday = async (a) => {
  if (!confirm(`把「${a.username || a.host}」的今日已发与连续失败清零？`)) return
  try {
    await adminApi.resetSmtpAccountToday(a.id)
    toast.success('今日计数已重置')
    await loadPool()
  } catch (e) {
    toast.error(e?.message || '重置失败')
  }
}

const removePool = async (a) => {
  if (!confirm(`删除账号「${a.username || a.host}」？删除后无法恢复。`)) return
  try {
    await adminApi.deleteSmtpAccount(a.id)
    toast.success('已删除')
    await loadPool()
  } catch (e) {
    toast.error(e?.message || '删除失败')
  }
}

// 状态文案/配色：cooling 是发信失败自动进入的临时态，和手动 disabled 要区分开
const poolStatusText = { active: '启用', cooling: '冷却中', disabled: '已停用' }
const poolStatusVariant = { active: 'success', cooling: 'warning', disabled: 'danger' }

// 测试发送的「用哪个号发」：空 = 站点默认账号
const testAccount = ref('')
const testAccountOptions = computed(() => [
  { label: '站点默认账号', value: '' },
  ...pool.value.map((a) => ({
    label: `${a.username || a.host}（${a.host}:${a.port}）`,
    value: a.id,
  })),
])

// 时间显示：后端 ISO 串；空值回 —（冷却到/最近使用在多数行都是空）
const fmtTime = (iso) => {
  if (!iso) return '—'
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString('zh-CN', { hour12: false })
}

// 今日已发：sent_today 只在 reset_at 是今天时有意义（后端同口径）
const sentTodayOf = (a) => {
  if (!a.reset_at) return 0
  const r = new Date(a.reset_at)
  const n = new Date()
  if (Number.isNaN(r.getTime())) return 0
  return r.toISOString().slice(0, 10) === n.toISOString().slice(0, 10) ? a.sent_today || 0 : 0
}

const poolLimitText = (a) => (a.daily_limit > 0 ? String(a.daily_limit) : '不限')

// 冷却到：只有 cooling 且未到期才有意义
const cooldownText = (a) => {
  if (a.status !== 'cooling' || !a.cooldown_until) return '—'
  return fmtTime(a.cooldown_until)
}

// ============ 自动通知 ============
// 四个场景全部默认关闭，管理员自己开。后端把「未配置」一律当关闭，
// 所以这里读到 false 就保持关闭，不会因为没配过就自动发信。
// 去重口径：同一收件人、同一场景、每个自然月最多一封（后端 MailNotifyService::claimMonthly）。
const SCENES = [
  {
    key: 'traffic_exhausted',
    name: '流量用尽',
    desc: '用户流量跑满、3x-ui 流量被关闭时发一封。每自然月最多一封。',
  },
  {
    key: 'traffic_almost',
    name: '流量即将用尽',
    desc: '定时扫描（每 5 分钟），达到上方「流量即将用尽阈值」时提醒。每自然月最多一封。',
  },
  {
    key: 'expiring',
    name: '套餐即将到期',
    desc: '定时扫描，提前「到期提醒天数」天提醒续费。每自然月最多一封。',
  },
  {
    key: 'expired',
    name: '套餐已到期',
    desc: '定时扫描，过期后 7 天内提醒一次。',
  },
]

// 可插入的变量：管理员写标题/正文时直接用
const VARS = [
  '{{email}}', '{{user_id}}', '{{site_title}}', '{{used}}', '{{limit}}',
  '{{percent}}', '{{plan_name}}', '{{expire_date}}', '{{days_left}}', '{{subscribe_url}}',
]

const notify = ref({
  notify_admin_email: '',
  notify_traffic_almost_threshold: 90,
  notify_expiring_days: 3,
})
// 每个场景一份 enabled/title/body/to
const sceneCfg = ref({})

const loadNotify = async () => {
  if (notifyLoading) return
  notifyLoading = true
  const seq = notifySaveSeq
  try {
    const data = await adminApi.getEmailNotify()
    // 有未保存修改 / 请求期间已保存过 → 丢弃这份旧数据（同 load）
    if (!data || notifyDirty || seq !== notifySaveSeq) return
    notify.value.notify_admin_email = data.notify_admin_email || ''
    notify.value.notify_traffic_almost_threshold = data.notify_traffic_almost_threshold || 90
    notify.value.notify_expiring_days = data.notify_expiring_days ?? 3
    const cfg = {}
    for (const s of SCENES) {
      cfg[s.key] = {
        // 后端未配置 = 关闭，用 ==='1' 严格判断，避免空串/false 被当成开启
        enabled: data[`notify_${s.key}_enabled`] === '1',
        title: data[`notify_${s.key}_title`] || '',
        body: data[`notify_${s.key}_body`] || '',
        to: data[`notify_${s.key}_to`] || 'user',
      }
    }
    sceneCfg.value = cfg
    savedNotifySnap = JSON.stringify(buildNotifyPayload())   // 自动保存基线
  } catch (e) {} finally {
    notifyLoading = false
  }
}

const buildNotifyPayload = () => {
  const payload = { ...notify.value }
  for (const s of SCENES) {
    const c = sceneCfg.value[s.key] || {}
    payload[`notify_${s.key}_enabled`] = c.enabled
    payload[`notify_${s.key}_title`] = c.title || ''
    payload[`notify_${s.key}_body`] = c.body || ''
    payload[`notify_${s.key}_to`] = c.to || 'user'
  }
  return payload
}

const saveNotifyCfg = async () => {
  const payload = buildNotifyPayload()
  const snap = JSON.stringify(payload)
  if (snap === savedNotifySnap) return
  try {
    // 序号先自增：更早发出的加载响应一律作废（同 saveForm）
    notifySaveSeq++
    await adminApi.saveEmailNotify(payload)
    savedNotifySnap = snap
    notifyDirty = JSON.stringify(buildNotifyPayload()) !== snap
    autoSaveToast()
  } catch (e) {
    toast.error(e?.message || '保存失败')
  }
}

// ============ 批量发信 ============
const batch = ref({
  mode: 'filter',        // filter=按条件群发 / single=单发
  subject: '',
  body: '',
  plan_id: '',
  user_status: 'normal',
  single_query: '',
})
const batchSending = ref(false)
const batchResult = ref(null)
// 批次进度（轮询 /admin-api/email/batch-progress；后端按 batch_id 只算本批）
const batchProgress = ref(null)
const batchDone = ref(false)
const planOptions = ref([])

const loadPlans = async () => {
  try {
    // 套餐选择器要一次看到全部套餐（分页只回前 50 条且是「数组+pagination」结构），
    // 与 Users.vue 同口径：显式 all=1，接口直接返回数组
    const plans = await adminApi.listPlans({ all: 1 })
    planOptions.value = Array.isArray(plans) ? plans : []
  } catch (e) {
    planOptions.value = []
  }
}

// 预计时长文案：estimate_seconds 由后端按「两套限速里更慢的那个」算好；
// 0/空 = 不限速，队列立刻抽干 → 「立即发完」。超过 60 秒向上取整到分钟，
// 避免出现「预计 0 分钟」（59 秒）或「预计 1.7 分钟」这种难读的数字。
const batchEstimateText = computed(() => {
  const sec = Math.max(0, Number(batchResult.value?.estimate_seconds) || 0)
  if (sec <= 0) return '立即发完'
  if (sec < 60) return `预计 ${sec} 秒内发完`
  return `预计 ${Math.ceil(sec / 60)} 分钟内发完`
})

// 已发+失败占总量的百分比（分母取 total，含还排队和正在发的）。
// total 恒 > 0（入队成功至少 1 封），这里仍兜一下 0 防 NaN 宽度。
const batchPercent = computed(() => {
  const p = batchProgress.value
  const total = Number(p?.total) || 0
  if (total <= 0) return 0
  const finished = (Number(p.sent) || 0) + (Number(p.failed) || 0)
  return Math.max(0, Math.min(100, Math.round((finished / total) * 100)))
})

// ===== 批次进度轮询 =====
// 每 5 秒拉一次 batch-progress；done 后停表（后端 queued/sending 归零才算完）。
// 定时器与在飞请求都要能撤：切 Tab / 离开页面后不许再回写已卸载组件的状态
// （本页是 keep-alive，onUnmounted 只在真正销毁时触发），所以 onUnmounted +
// onDeactivated 都清；重新 onActivated 且还没发完时再续上。
let batchTimer = null
let batchPollSeq = 0

const stopBatchPolling = () => {
  if (batchTimer) {
    clearTimeout(batchTimer)
    batchTimer = null
  }
  // 序号一变，在飞请求的响应到达后被丢弃
  batchPollSeq++
}

const scheduleBatchPoll = () => {
  stopBatchPolling()
  if (batchDone.value || !batchResult.value?.batch_id) return
  const seq = batchPollSeq
  batchTimer = setTimeout(() => {
    batchTimer = null
    if (seq !== batchPollSeq) return
    pollBatchProgress(seq)
  }, 5000)
}

const pollBatchProgress = async (seq) => {
  const batchId = batchResult.value?.batch_id
  if (!batchId) return
  try {
    const p = await adminApi.mailBatchProgress(batchId)
    // 期间已切走 / 已开始新批次 → 这份响应作废
    if (seq !== batchPollSeq) return
    if (p) {
      batchProgress.value = p
      // done 以后不再重排；先把状态落下来再判，避免最后一帧不显示 100%
      if (p.done === true) {
        batchDone.value = true
        return
      }
    }
  } catch (e) {
    // 轮询失败保持静默（下一轮会自己重试）；不打扰正在看页面的人
    if (seq !== batchPollSeq) return
  }
  scheduleBatchPoll()
}

const startBatchPolling = () => {
  stopBatchPolling()
  batchProgress.value = null
  batchDone.value = false
  if (!batchResult.value?.batch_id) return
  // 立刻拉一次：total 在入队响应里就有，不用干等 5 秒才出现进度条
  pollBatchProgress(batchPollSeq)
}

// 从暂停状态续上（切回「设置」Tab / keep-alive 回来）：不清已有进度，立刻刷新一次，
// 免得用户回来先看到 5 秒前的旧数字。
const resumeBatchPolling = () => {
  if (batchDone.value || !batchResult.value?.batch_id || batchTimer) return
  pollBatchProgress(batchPollSeq)
}

const onBatchSend = async () => {
  if (!batch.value.subject.trim()) return toast.error('请填写邮件标题')
  if (!batch.value.body.trim()) return toast.error('请填写邮件内容')
  if (batch.value.mode === 'single' && !batch.value.single_query.trim()) {
    return toast.error('请填写要发给谁（邮箱或用户 ID）')
  }
  batchSending.value = true
  // 旧批次的进度先停表并清空，别把上一批的「发送完成」留给这一批
  stopBatchPolling()
  batchResult.value = null
  batchProgress.value = null
  batchDone.value = false
  try {
    const res = await adminApi.batchSendEmail({
      ...batch.value,
      plan_id: batch.value.plan_id || null,
    })
    batchResult.value = res
    toast.success(`已加入发送队列，共 ${res.total} 封`)
    startBatchPolling()
  } catch (e) {
    toast.error(e?.message || '发送失败')
  } finally {
    batchSending.value = false
  }
}

// ============ 按月定时发信 ============
const schedule = ref({
  enabled: false,
  day: 1,
  time: '09:00',
  config: {
    mode: 'filter',
    user_status: 'normal',
    plan_id: '',
    single_query: '',
    subject: '',
    body: '',
  },
})

const loadSchedule = async () => {
  if (scheduleLoading) return
  scheduleLoading = true
  const seq = scheduleSaveSeq
  try {
    const data = await adminApi.getEmailSchedule()
    // 有未保存修改 / 请求期间已保存过 → 丢弃这份旧数据，否则刚打开的开关会被盖回关闭
    if (!data || scheduleDirty || seq !== scheduleSaveSeq) return
    schedule.value.enabled = data.enabled === true || data.enabled === '1'
    schedule.value.day = data.day ?? 1
    schedule.value.time = data.time || '09:00'
    const c = data.config || {}
    schedule.value.config = {
      // 'all' 在下拉里没有对应项（旧默认值），归一为「按条件群发」显示；
      // 后端对 all/filter 的处理本来就相同（都按 user_status 过滤）
      mode: c.mode === 'single' ? 'single' : 'filter',
      user_status: c.user_status || 'normal',
      plan_id: c.plan_id || '',
      single_query: c.single_query || '',
      subject: c.subject || '',
      body: c.body || '',
    }
    savedScheduleSnap = JSON.stringify(buildSchedulePayload())   // 自动保存基线
  } catch (e) {} finally {
    scheduleLoading = false
  }
}

const buildSchedulePayload = () => ({
  enabled: schedule.value.enabled,
  day: schedule.value.day,
  time: schedule.value.time,
  config: {
    ...schedule.value.config,
    plan_id: schedule.value.config.plan_id || null,
  },
})

const saveScheduleCfg = async () => {
  const payload = buildSchedulePayload()
  const snap = JSON.stringify(payload)
  if (snap === savedScheduleSnap) return
  try {
    // 序号先自增：更早发出的加载响应一律作废，避免把刚打开的开关盖回关闭（同 saveForm）
    scheduleSaveSeq++
    await adminApi.saveEmailSchedule(payload)
    savedScheduleSnap = snap
    scheduleDirty = JSON.stringify(buildSchedulePayload()) !== snap
    autoSaveToast()
  } catch (e) {
    toast.error(e?.message || '保存失败')
  }
}

// ============ 发信日志 ============
const logs = ref([])
const logPage = ref(1)
const logPages = ref(1)
const logTotal = ref(0)
const logFilter = ref({ type: '', status: '' })
const logsLoading = ref(false)

const loadLogs = async () => {
  logsLoading.value = true
  try {
    const res = await adminApi.getEmailLogs({
      page: logPage.value,
      type: logFilter.value.type || undefined,
      status: logFilter.value.status || undefined,
    })
    // 分页接口经 client.js 拦截器解包后 = 列表数组本身（pagination 挂在数组上）。
    // 直接消费返回值；再取 .data 永远拿到 undefined —— 这正是日志页一直空白的原因。
    logs.value = Array.isArray(res) ? res : []
    logTotal.value = res?.pagination?.total ?? 0
    logPages.value = Math.max(1, res?.pagination?.last_page ?? 1)
  } catch (e) {
    logs.value = []
  } finally {
    logsLoading.value = false
  }
}

const logTypeText = { notify: '自动通知', batch: '批量发信', schedule: '按月发信', test: '测试邮件' }
const logStatusText = { sent: '成功', failed: '失败' }
const encryptionOptions = [
  { label: 'SSL (465)', value: 'ssl' },
  { label: 'TLS (587)', value: 'tls' },
]

const recipientOptions = [
  { label: '发给用户', value: 'user' },
  { label: '发给管理员', value: 'admin' },
]

const sendModeOptions = [
  { label: '按条件群发', value: 'filter' },
  { label: '单发（指定一个人）', value: 'single' },
]

const userStatusOptions = [
  { label: '正常（未过期、未超量）', value: 'normal' },
  { label: '流量超量', value: 'over' },
  { label: '已过期', value: 'expired' },
]

const logTypeOptions = [
  { label: '全部', value: '' },
  { label: '自动通知', value: 'notify' },
  { label: '批量发信', value: 'batch' },
  { label: '按月发信', value: 'schedule' },
  { label: '测试邮件', value: 'test' },
]

const logStatusOptions = [
  { label: '全部', value: '' },
  { label: '成功', value: 'sent' },
  { label: '失败', value: 'failed' },
]

// 批量发信「限定套餐」：与套餐下拉同源
const batchPlanOptions = computed(() => [
  { label: '全部套餐', value: '' },
  ...planOptions.value.map((p) => ({ label: p.name, value: p.id })),
])

const logSceneText = {
  traffic_exhausted: '流量用尽',
  traffic_almost: '流量即将用尽',
  expiring: '即将到期',
  expired: '已到期',
  no_plan: '无套餐',
}

// ===== 自动保存监听：值没变（加载/回读）不触发；真实修改停手 800ms 后提交 =====
// 偏离已存基线 = 有未保存修改 → 置脏标记；加载回来的数据若赶上脏标记会被丢弃（见 load）。
watch(form, () => {
  if (JSON.stringify(form.value) === savedFormSnap) return
  formDirty = true
  clearTimeout(formTimer)
  formTimer = setTimeout(saveForm, 800)
}, { deep: true })
watch([notify, sceneCfg], () => {
  if (JSON.stringify(buildNotifyPayload()) === savedNotifySnap) return
  notifyDirty = true
  clearTimeout(notifyTimer)
  notifyTimer = setTimeout(saveNotifyCfg, 800)
}, { deep: true })
watch(schedule, () => {
  if (JSON.stringify(buildSchedulePayload()) === savedScheduleSnap) return
  scheduleDirty = true
  clearTimeout(scheduleTimer)
  scheduleTimer = setTimeout(saveScheduleCfg, 800)
}, { deep: true })

onMounted(() => { load(); loadNotify(); loadSchedule(); loadPlans(); loadLogs(); loadPool() })
onActivated(() => {
  load(); loadNotify(); loadSchedule(); loadLogs(); loadPool()
  // keep-alive 退场时轮询被停了：回来时若这批还没发完、也还在「设置」Tab，接着看进度。
  // 已 done 的不再重启，否则每次切 Tab 回来都多打一次已经没意义的请求。
  if (tab.value === 'settings') resumeBatchPolling()
})
// 切走（keep-alive 缓存）与真正销毁都要停表：定时器留着会继续打接口，
// 在飞的响应还会回写一个用户已经看不到的页面状态。
onDeactivated(() => stopBatchPolling())
onUnmounted(() => stopBatchPolling())
</script>

<style scoped>
/* 页面铺满主区：原先 max-width:640px 在宽屏下右侧整片留白。
   下面用两栏网格承载「左=基础配置 / 右=高级功能」，窄屏自动并成一列。 */
.email-page { max-width: 1180px; }
.email-head { display: flex; align-items: baseline; gap: var(--space-3); }
.email-title { font-size: var(--text-xl); font-weight: var(--font-semibold); margin-bottom: var(--space-5); }
.email-autosave-hint { font-size: var(--text-xs); color: var(--text-muted); }
.email-warn-note { font-size: var(--text-xs); color: #b45309; margin-top: var(--space-3); }
html.dark .email-warn-note { color: #fbbf24; }
/* 模块导航：横向标签，激活项主题色下划线；窄屏可横向滚动不换行 */
.email-tabs {
  display: flex;
  gap: var(--space-1);
  margin-bottom: var(--space-5);
  border-bottom: 1px solid var(--border-subtle);
  overflow-x: auto;
}
.email-tab {
  flex: 0 0 auto;
  padding: var(--space-2) var(--space-3);
  font-size: var(--text-sm);
  font-weight: var(--font-medium);
  color: var(--text-secondary);
  background: none;
  border: none;
  border-bottom: 2px solid transparent;
  cursor: pointer;
  white-space: nowrap;
  transition: color 0.15s, border-color 0.15s;
}
.email-tab:hover { color: var(--text-primary); }
.email-tab--active {
  color: var(--accent, #0d9488);
  border-bottom-color: var(--accent, #0d9488);
}
/* 两栏：左列发送测试/SMTP/邮件模板，右列批量发信。
   只承载「基础配置」——自动通知与发信日志改成下方通栏。
   grid-template-columns 用 minmax(0,1fr) 防止长内容（代码框）把栅格撑破。 */
.email-cols {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  gap: var(--space-5);
}
/* 两列纵排：列内最后一张卡吸收剩余高度（flex:1），左右两列底部自动对齐；
   拉伸出的空间交给正文编辑区（textarea 弹性变高），而不是留成一片空白 */
.email-col { min-width: 0; display: flex; flex-direction: column; }
.email-col > .email-card:last-child { flex: 1; display: flex; flex-direction: column; }
.email-col > .email-card:last-child textarea.email-template { flex: 1 1 auto; min-height: 120px; }
.email-col > .email-card:last-child > .email-save-wrap:last-child { margin-top: auto; }
/* 单栏时每张卡片自身的下边距交给父级 gap 统一控制，避免间距双份 */
.email-col > .email-card:last-child,
.email-col > .email-save-wrap:last-child { margin-bottom: 0; }

/* 1280px 以下并成一列：两栏时每栏太窄，输入框会被挤到换行 */
@media (max-width: 1280px) {
  .email-cols { grid-template-columns: minmax(0, 1fr); }
}

/* 窄屏：卡片内边距收一点，避免输入框被挤 */
@media (max-width: 640px) {
  .email-page { max-width: 100%; }
}
.email-card { margin-bottom: var(--space-5); }
/* 通栏卡（自动通知/发信日志）：不在两栏网格内，本身就是块级，宽度自然撑满。
   留一条规则是为了在「被误塞回 .email-cols」时能看出来，而不是静默变半宽。 */
.email-card-full { max-width: 100%; }
/* 通栏下表格有更多宽度，收件人/标题不必再截断到 160px */
.email-card-full .email-log-to, .email-card-full .email-log-subject { max-width: 260px; }
.email-card h2 { font-size: var(--text-base); font-weight: var(--font-semibold); }
.email-grid { display: grid; grid-template-columns: 1fr 1fr; gap: var(--space-4); }
/* 3 个并列参数（通知收件邮箱 / 阈值 / 天数）一排放下 */
.email-grid--3 { grid-template-columns: repeat(3, minmax(0, 1fr)); }
@media (max-width: 1100px) { .email-grid--3 { grid-template-columns: 1fr 1fr; } }
/* 8 项刚好 4 行两列，不需要按开关数量切换列数 */
@media (max-width: 640px) { .email-grid, .email-grid--3 { grid-template-columns: 1fr; } }
.email-label { font-size: var(--text-sm); font-weight: var(--font-medium); color: var(--text-secondary); display: block; margin-bottom: var(--space-1); }


.email-hint { font-size: var(--text-sm); color: var(--text-secondary); margin-bottom: var(--space-3); display: flex; align-items: center; gap: var(--space-2); }
.email-hint code { background: var(--bg-hover); padding: 0 4px; border-radius: var(--radius-sm); font-size: var(--text-xs); }
.email-template { width: 100%; padding: var(--space-3); border: 1px solid var(--border-subtle); border-radius: var(--radius-md); background: var(--bg-input); color: var(--text-primary); font-family: monospace; font-size: var(--text-sm); resize: vertical; line-height: 1.6; }
.email-template:focus { outline: none; border-color: var(--accent); box-shadow: 0 0 0 2px var(--accent-muted); }
.email-test-row { display: flex; align-items: flex-end; gap: var(--space-3); flex-wrap: wrap; }
/* 「用哪个账号发」下拉定宽：Select 默认 width:100%，不定宽会把收件邮箱挤没 */
.email-test-account { width: 220px; flex: 0 0 auto; min-width: 0; }
@media (max-width: 640px) { .email-test-account { width: 100%; } }
/* flex 工具类：模板里给「发送测试」行贴了这两个名字，但项目里没有全局定义，
   缺了输入框就不撑满、按钮不贴底。定义在本地 scoped 里，别去动 main.css。 */
.flex-1 { flex: 1 1 0%; min-width: 0; }
.self-end { align-self: flex-end; }
/* 安全限制的 8 个输入项：min-width:0 防长 placeholder 撑破两列网格 */
.email-rate-item { min-width: 0; }
.email-rate-desc { font-size: var(--text-xs); color: var(--text-muted); margin-top: var(--space-1); }
/* 授权码输入框 + 眼睛切换：按钮绝对定位贴在输入框右侧内缘，输入框留出右侧内边距避免文字压在图标下 */
.email-pwd { position: relative; }
.email-pwd :deep(.ch-input) { padding-right: 36px; }
.email-pwd-toggle {
  position: absolute;
  top: 50%;
  right: 4px;
  transform: translateY(-50%);
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 28px;
  height: 28px;
  padding: 0;
  border: none;
  border-radius: var(--radius-sm);
  background: none;
  color: var(--text-muted);
  cursor: pointer;
  transition: color 0.15s, background-color 0.15s;
}
.email-pwd-toggle:hover { color: var(--text-primary); background: var(--bg-hover); }
/* 明文可见时图标点亮为强调色（图标集只有 eye-off，用状态色区分显示/隐藏） */
.email-pwd-toggle--on { color: var(--accent, #0d9488); }
.email-pwd-toggle--on:hover { color: var(--accent, #0d9488); }
.email-pwd-toggle:focus-visible { outline: 2px solid var(--accent); outline-offset: 1px; }
.email-switch-row { display: flex; align-items: center; gap: var(--space-3); margin-bottom: var(--space-3); }
.email-switch-label { font-size: var(--text-sm); font-weight: var(--font-medium); color: var(--text-primary); }
.email-switch-hint { font-size: var(--text-xs); color: var(--text-muted); }
.email-switch { position: relative; display: inline-block; width: 48px; height: 27px; flex-shrink: 0; }
.email-switch-input { opacity: 0; width: 0; height: 0; position: absolute; }
.email-switch-slider { position: absolute; cursor: pointer; inset: 0; background: var(--switch-off, #d1d5db); border-radius: 14px; transition: 0.3s; box-shadow: inset 0 1px 3px rgba(0,0,0,0.1); }
.email-switch-slider::before { content: ''; position: absolute; height: 21px; width: 21px; left: 3px; bottom: 3px; background: var(--switch-knob, #fff); border-radius: 50%; transition: 0.3s; box-shadow: 0 2px 4px rgba(0,0,0,0.2); }
.email-switch-input:checked + .email-switch-slider { background: var(--switch-on, #2563eb); box-shadow: inset 0 0 0 0 rgba(0,0,0,0); }
.email-switch-input:checked + .email-switch-slider::before { transform: translateX(21px); }
.email-save-wrap { margin-top: var(--space-4); display: flex; justify-content: flex-end; }
@media (max-width: 640px) { .email-save-wrap { justify-content: stretch; } .email-save-wrap > button { width: 100%; } }
/* 自动通知：变量提示 + 场景卡 */
.email-vars { display: flex; flex-wrap: wrap; align-items: center; gap: var(--space-2); margin: var(--space-3) 0; }
.email-vars-label { font-size: var(--text-xs); color: var(--text-muted); }
.email-var { background: var(--bg-hover); padding: 2px 6px; border-radius: var(--radius-sm); font-size: var(--text-xs); color: var(--text-secondary); cursor: pointer; }
.email-var:hover { color: var(--accent); }
/* 通知场景：两列网格。5 个场景单列排下来近 1000px 高，撑爆整页；
   两列后高度砍半，且展开正文框时两栏仍有余量。 */
/* 行内卡片默认 stretch 等高：展开一侧时另一侧跟随对齐，避免「犬牙交错」 */
.email-scenes { display: grid; grid-template-columns: 1fr 1fr; gap: var(--space-3); }
@media (max-width: 1100px) { .email-scenes { grid-template-columns: 1fr; } }
.email-scene { border: 1px solid var(--border-subtle); border-radius: var(--radius-md); padding: var(--space-3); min-width: 0; }
.email-scene .email-switch-row { margin-bottom: var(--space-2); }
/* 场景行标题不换行 + 下拉定宽：Select 默认 width:100%，不定宽会把标题挤到折行 */
.email-scene .email-switch-row .email-switch-label { white-space: nowrap; }
.email-scene .email-switch-row .ch-select { width: auto; min-width: 128px; flex: 0 0 auto; }

.email-scene .email-rate-desc { margin-bottom: var(--space-3); }

/* 场景标题/正文与开关之间的间距 */
.email-scene-body { margin-top: var(--space-3); }
.email-mt { margin-top: var(--space-3); }
.email-result { margin-top: var(--space-3); margin-bottom: 0; }
/* 批量发信进度条：沿用本页的 hint/description 字号与主题变量，不另起设计语言 */
.email-result .email-hint { margin-bottom: var(--space-2); }
.email-estimate { color: var(--text-secondary); }
.email-progress { margin-top: var(--space-2); }
.email-progress-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: var(--space-2);
  margin-bottom: var(--space-1);
  font-size: var(--text-xs);
}
.email-progress-count { color: var(--text-secondary); font-variant-numeric: tabular-nums; }
/* 失败数标红：进度条本身不因为失败变色（否则看不出走到哪），红字单独提示 */
.email-progress-failed { color: var(--danger, #dc2626); font-weight: var(--font-medium); }
.email-progress-track {
  height: 8px;
  background: var(--bg-hover);
  border-radius: var(--radius-full);
  overflow: hidden;
}
.email-progress-fill {
  display: block;
  height: 100%;
  background: var(--accent, #0d9488);
  border-radius: var(--radius-full);
  transition: width var(--duration-base, 0.2s) var(--ease-out, ease-out);
}
.email-progress-done { font-size: var(--text-xs); color: var(--text-secondary); margin-top: var(--space-2); }
.email-progress-hint { font-size: var(--text-xs); color: var(--text-muted); margin-top: var(--space-1); }
.email-progress-hint--failed { color: var(--danger, #dc2626); }
/* 发信日志 */
.email-table-wrap { margin-top: var(--space-3); overflow-x: auto; }
.email-table { width: 100%; border-collapse: collapse; font-size: var(--text-xs); }
.email-table th { text-align: left; padding: var(--space-2); color: var(--text-muted); font-weight: var(--font-medium); border-bottom: 1px solid var(--border-subtle); white-space: nowrap; }
.email-table td { padding: var(--space-2); border-bottom: 1px solid var(--border-subtle); vertical-align: top; }
.email-log-to, .email-log-subject { max-width: 160px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.email-log-error { color: var(--danger, #dc2626); font-size: var(--text-xs); margin-top: 2px; max-width: 200px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.email-empty { font-size: var(--text-sm); color: var(--text-muted); text-align: center; padding: var(--space-6) 0; }
/* 账号池表格：列多，用户名/操作列不折行；操作按钮排成一行靠右 */
.email-pool-head { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); }
.email-pool-head h2 { font-size: var(--text-base); font-weight: var(--font-semibold); margin: 0; }
.email-pool-table { font-size: var(--text-sm); }
.email-pool-table td { vertical-align: middle; }
.email-pool-user { max-width: 200px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.email-pool-of { color: var(--text-muted); }
.email-pool-err { color: var(--danger, #dc2626); font-weight: var(--font-semibold); }
.email-pool-actions-col { width: 1%; }
.email-pool-actions { display: flex; align-items: center; gap: var(--space-1); white-space: nowrap; }
.email-log-pager { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); margin-top: var(--space-3); }
.email-pager-info { font-size: var(--text-xs); color: var(--text-muted); }

/* 表单栅格对齐：标签统一占两行高度，长标签换行时不会把输入框挤下去 */
.email-cols { align-items: start; }
.email-cols .email-label { min-height: 2.6em; display: flex; align-items: flex-end; margin-bottom: 0; }
</style>
