package admin

import (
	"strconv"
	"strings"

	lcrypto "controlhub-server/internal/crypto"
	"controlhub-server/internal/tgbot"

	"github.com/gin-gonic/gin"
)

// telegramShow 对齐 GET /admin-api/telegram（token 解密回显，其余明文）。
func (d *TelegramDomain) telegramShow(c *gin.Context) {
	cfg, err := d.store.SiteConfigMany(ctxOf(c), telegramConfigKeys)
	if err != nil {
		d.failServer(c, "读取 TG 配置", err)
		return
	}
	out := &jsObj{}
	for _, k := range telegramConfigKeys {
		v := cfg[k]
		if k == tgbot.KeyToken && v != "" {
			if plain, derr := lcrypto.DecryptString(v, d.appKey); derr == nil {
				v = plain
			}
		}
		out.Put(k, v)
	}
	d.success(c, out)
}

// telegramSave 对齐 PUT /admin-api/telegram（token 加密存，其余明文）。
func (d *TelegramDomain) telegramSave(c *gin.Context) {
	data := jsonInput(c)
	v := newValidator(ctxOf(c), data)
	v.field(tgbot.KeyToken, "nullable", "string", "max:255")
	v.field(tgbot.KeyChatID, "nullable", "string", "max:64")
	v.field(tgbot.KeyEnabled, "nullable", "boolean")
	v.field(tgbot.KeyNotifyNodeOff, "nullable", "boolean")
	v.field(tgbot.KeyNotifyPayment, "nullable", "boolean")
	v.field(tgbot.KeyBackupPush, "nullable", "boolean")
	if !v.passes() {
		if err := v.storeFailure(); err != nil {
			d.failServer(c, "校验 TG 配置", err)
			return
		}
		d.failValidation(c, v.summary())
		return
	}

	updates := map[string]string{}
	for _, k := range telegramConfigKeys {
		val, ok := data[k]
		if !ok {
			continue // 未提交的键不写
		}
		s := mailConfigStoreValue(k, val)
		if k == tgbot.KeyToken && s != "" {
			enc, err := lcrypto.EncryptString(s, d.appKey)
			if err != nil {
				d.failServer(c, "加密 TG token", err)
				return
			}
			s = enc
		}
		updates[k] = s
	}

	// 空值保护：token 一旦被空串写进去，通知就再也发不出去。
	// 语义：只提交非空值视为「保持原样」；要真正清空请在数据库侧操作。
	if updates[tgbot.KeyToken] == "" {
		current, err := d.store.SiteConfigMany(ctxOf(c), []string{tgbot.KeyToken})
		if err != nil {
			d.failServer(c, "读取 TG 配置", err)
			return
		}
		if current[tgbot.KeyToken] != "" {
			delete(updates, tgbot.KeyToken)
		}
	}

	kv := make([]KV, 0, len(updates))
	for k, val := range updates {
		v := val
		kv = append(kv, KV{Key: k, Value: &v})
	}
	if err := d.store.SiteConfigSetMany(ctxOf(c), kv); err != nil {
		d.failServer(c, "保存 TG 配置", err)
		return
	}
	d.success(c, nil)
}

// telegramTest 对齐 POST /admin-api/telegram/test：向配置的 chat_id 发一条测试消息。
func (d *TelegramDomain) telegramTest(c *gin.Context) {
	cfg, err := d.store.SiteConfigMany(ctxOf(c), telegramConfigKeys)
	if err != nil {
		d.failServer(c, "读取 TG 配置", err)
		return
	}
	chatIDStr := strings.TrimSpace(cfg[tgbot.KeyChatID])
	chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
	if err != nil || chatID == 0 {
		d.fail(c, "请先填写正确的 Chat ID", 400)
		return
	}
	encToken := cfg[tgbot.KeyToken]
	if encToken == "" {
		d.fail(c, "请先填写 Bot Token", 400)
		return
	}
	token, err := lcrypto.DecryptString(encToken, d.appKey)
	if err != nil {
		d.fail(c, "Bot Token 解密失败，请重新保存", 400)
		return
	}

	client := tgbot.NewClient(token, nil)
	if err := client.SendMessage(ctxOf(c), chatID, "ControlHub 面板：Telegram 通知测试成功 ✅"); err != nil {
		d.fail(c, "发送失败："+err.Error(), 400)
		return
	}
	d.success(c, nil)
}
