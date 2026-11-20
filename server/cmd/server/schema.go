package main

import (
	"database/sql"
	"fmt"
)

// ensureSchema 启动时做增量建表 / 加列（全部幂等，老用户升级不用手跑 SQL）。
//
// 为什么必须有这一步：install.sh 只在「全新安装」（数据库表数为 0）时执行
// server/schema/schema.sql；老用户升级（表数 > 0）直接跳过建表。于是新增的表
// 如果只写进 schema.sql，老用户升级后表根本不会建、功能一跑就报「表不存在」。
// 新增列同理：老库的 xray_inbounds 不会有新列，读写会直接 Unknown column 报错。
// 所以每新增一张表 / 一列，都要在这里补一条幂等语句，保证老用户升级全自动。
func ensureSchema(db *sql.DB) error {
	stmts := []string{
		// 多 SMTP 账号池（#66）：备用发信账号，主号风控时自动轮换。
		`CREATE TABLE IF NOT EXISTS smtp_accounts (
			id bigint unsigned NOT NULL AUTO_INCREMENT,
			host varchar(255) NOT NULL,
			port int NOT NULL DEFAULT 587,
			username varchar(255) NOT NULL DEFAULT '',
			password_enc text,
			encryption varchar(8) NOT NULL DEFAULT 'tls',
			from_name varchar(100) DEFAULT NULL,
			weight int NOT NULL DEFAULT 1,
			daily_limit int NOT NULL DEFAULT 0,
			sent_today int NOT NULL DEFAULT 0,
			last_used_at timestamp NULL DEFAULT NULL,
			reset_at timestamp NULL DEFAULT NULL,
			status varchar(16) NOT NULL DEFAULT 'active',
			cooldown_until timestamp NULL DEFAULT NULL,
			error_count int NOT NULL DEFAULT 0,
			created_at timestamp NULL DEFAULT NULL,
			updated_at timestamp NULL DEFAULT NULL,
			PRIMARY KEY (id),
			KEY smtp_accounts_status_index (status)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		// TLS 证书库（HY2）：入站只存 tlsCertId，私钥加密后放这里，
		// 渲染配置时才解密注入 tlsSettings.certificates（私钥永不进 xray_inbounds.stream_settings）。
		`CREATE TABLE IF NOT EXISTS xray_certificates (
			id bigint unsigned NOT NULL AUTO_INCREMENT,
			name varchar(190) NOT NULL,
			domain varchar(190) DEFAULT NULL,
			cert_pem mediumtext,
			key_enc mediumtext,
			not_after datetime DEFAULT NULL,
			created_at timestamp NULL DEFAULT NULL,
			updated_at timestamp NULL DEFAULT NULL,
			PRIMARY KEY (id),
			UNIQUE KEY xray_certificates_name_unique (name)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		// 余额兑换码（#账户余额）：单次使用，面值整数分。
		`CREATE TABLE IF NOT EXISTS giftcards (
			id bigint unsigned NOT NULL AUTO_INCREMENT,
			code varchar(32) NOT NULL COMMENT '兑换码，格式 XXXX-XXXX-XXXX-XXXX',
			name varchar(255) NOT NULL DEFAULT '' COMMENT '名称/备注',
			value bigint NOT NULL COMMENT '面值(分)',
			status varchar(16) NOT NULL DEFAULT 'active' COMMENT 'active=未用 / used=已用 / disabled=作废',
			used_by bigint unsigned DEFAULT NULL COMMENT '使用它的用户 id，null=未用',
			used_at timestamp NULL DEFAULT NULL,
			expires_at datetime DEFAULT NULL COMMENT '过期时间，null=永久',
			created_by bigint unsigned DEFAULT NULL COMMENT '创建的管理员 id',
			note varchar(255) DEFAULT NULL COMMENT '管理员备注',
			created_at timestamp NULL DEFAULT NULL,
			updated_at timestamp NULL DEFAULT NULL,
			PRIMARY KEY (id),
			UNIQUE KEY giftcards_code_unique (code),
			KEY giftcards_status_index (status),
			KEY giftcards_used_by_index (used_by)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		// 余额兑换码文件夹（对齐优惠码的分组口径）：giftcards.folder_id 指向这里。
		// 与 discount_code_folders 同一套语义：删除文件夹默认把码退回「未归档」。
		`CREATE TABLE IF NOT EXISTS giftcard_folders (
			id bigint unsigned NOT NULL AUTO_INCREMENT,
			name varchar(60) NOT NULL COMMENT '文件夹名',
			created_at timestamp NULL DEFAULT NULL,
			updated_at timestamp NULL DEFAULT NULL,
			PRIMARY KEY (id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		// 余额流水（#账户余额）：每次变动一条，含变动后余额。
		`CREATE TABLE IF NOT EXISTS balance_logs (
			id bigint unsigned NOT NULL AUTO_INCREMENT,
			user_id bigint unsigned NOT NULL COMMENT '用户 id',
			amount bigint NOT NULL COMMENT '变动额(分)，正=加 / 负=扣',
			type varchar(16) NOT NULL COMMENT 'recharge=充值 / consume=消费 / redeem=兑换 / admin=管理员调整 / rebate=邀请返佣',
			order_id bigint unsigned DEFAULT NULL COMMENT '关联订单 id（充值/消费/返佣）',
			giftcard_id bigint unsigned DEFAULT NULL COMMENT '关联兑换码 id（兑换）',
			operator_id bigint unsigned DEFAULT NULL COMMENT '操作管理员 id（admin 调整时）',
			balance_after bigint NOT NULL COMMENT '变动后余额(分)',
			note varchar(255) DEFAULT NULL,
			created_at timestamp NULL DEFAULT NULL,
			PRIMARY KEY (id),
			KEY balance_logs_user_id_index (user_id),
			KEY balance_logs_type_index (type),
			KEY balance_logs_order_id_index (order_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		// 邀请返佣流水（#邀请返佣）：一笔订单最多一条 —— order_id 唯一键就是发放幂等闸门，
		// 网关回调重推 / 余额支付重复完成时，第二次 INSERT 撞唯一键即跳过（不加钱、不写流水）。
		`CREATE TABLE IF NOT EXISTS affiliate_rebates (
			id bigint unsigned NOT NULL AUTO_INCREMENT,
			inviter_id bigint unsigned NOT NULL COMMENT '邀请人用户 id（返佣收款方）',
			invitee_id bigint unsigned NOT NULL COMMENT '被邀请人用户 id（产生订单的人）',
			order_id bigint unsigned NOT NULL COMMENT '触发的订单 id，唯一 = 同一订单只返一次',
			order_no varchar(64) NOT NULL DEFAULT '' COMMENT '订单号（留痕，订单删了也能看）',
			amount bigint NOT NULL COMMENT '返佣金额(分)，正数',
			rate decimal(5,4) NOT NULL DEFAULT '0.0000' COMMENT '发放时的比例（如 0.1000）',
			created_at timestamp NULL DEFAULT NULL,
			PRIMARY KEY (id),
			UNIQUE KEY affiliate_rebates_order_unique (order_id),
			KEY affiliate_rebates_inviter_index (inviter_id),
			KEY affiliate_rebates_invitee_index (invitee_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		// 地区（#地区功能）：节点按地区分组，后续按地区决定用户可见节点。
		// 纯字典表 —— 升级后保持为空，NULL region_id 的节点 = 不参与地区过滤，
		// 老用户升级后看到的节点、订阅、下发行为与升级前完全一致。
		`CREATE TABLE IF NOT EXISTS regions (
			id bigint unsigned NOT NULL AUTO_INCREMENT,
			name varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
			sort int NOT NULL DEFAULT 0,
			created_at timestamp NULL DEFAULT NULL,
			updated_at timestamp NULL DEFAULT NULL,
			PRIMARY KEY (id),
			UNIQUE KEY regions_name_unique (name)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		// 用户 ↔ 节点绑定（#地区功能）：一个用户可绑多个节点。
		// 升级后保持空表 —— 空表 = 不做任何节点过滤（老用户行为不变）。
		`CREATE TABLE IF NOT EXISTS user_nodes (
			user_id bigint unsigned NOT NULL,
			node_id bigint unsigned NOT NULL,
			PRIMARY KEY (user_id, node_id),
			KEY idx_node (node_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		// 用户换节点历史（#地区功能）：谁在什么时候因为什么从哪个节点换到哪个节点。
		`CREATE TABLE IF NOT EXISTS user_node_migrations (
			id bigint unsigned NOT NULL AUTO_INCREMENT,
			user_id bigint unsigned NOT NULL,
			from_node_id bigint unsigned DEFAULT NULL,
			to_node_id bigint unsigned NOT NULL,
			reason varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
			operator varchar(64) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
			created_at timestamp NULL DEFAULT NULL,
			PRIMARY KEY (id),
			KEY idx_user (user_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		// 节点指标采样（#地区功能）：按时间累积，供负载/容量曲线用。
		`CREATE TABLE IF NOT EXISTS node_metrics (
			id bigint unsigned NOT NULL AUTO_INCREMENT,
			node_id bigint unsigned NOT NULL,
			ts datetime NOT NULL,
			users int NOT NULL DEFAULT 0,
			mem_used int NOT NULL DEFAULT 0,
			mem_total int NOT NULL DEFAULT 0,
			cpu decimal(5,2) DEFAULT NULL,
			conns int NOT NULL DEFAULT 0,
			PRIMARY KEY (id),
			KEY idx_node_ts (node_id, ts)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		// 配置模板（#地区功能-19）：一套标准入站/出站/路由，新节点一键套用。
		// 三张表都是纯字典表 —— 升级后保持为空，不套用的节点行为完全不变。
		// 只对 driver_type=xray 的节点生效（3x-ui 节点的配置在面板侧，没有对应表）。
		// 刻意不含 reality_private_key：私钥必须每个节点单独生成，否则全网共用一把。
		`CREATE TABLE IF NOT EXISTS xray_template_inbounds (
			id bigint unsigned NOT NULL AUTO_INCREMENT,
			name varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
			sort int NOT NULL DEFAULT 0,
			tag varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
			remark varchar(128) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
			protocol varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL,
			port int unsigned NOT NULL,
			listen varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '0.0.0.0',
			settings json DEFAULT NULL,
			stream_settings json DEFAULT NULL,
			sniffing json DEFAULT NULL,
			enabled tinyint NOT NULL DEFAULT 1,
			created_at timestamp NULL DEFAULT NULL,
			updated_at timestamp NULL DEFAULT NULL,
			PRIMARY KEY (id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS xray_template_outbounds (
			id bigint unsigned NOT NULL AUTO_INCREMENT,
			name varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
			sort int NOT NULL DEFAULT 0,
			tag varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
			protocol varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL,
			settings json DEFAULT NULL,
			stream_settings json DEFAULT NULL,
			enabled tinyint NOT NULL DEFAULT 1,
			created_at timestamp NULL DEFAULT NULL,
			updated_at timestamp NULL DEFAULT NULL,
			PRIMARY KEY (id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS xray_template_routing (
			id bigint unsigned NOT NULL AUTO_INCREMENT,
			name varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
			sort int NOT NULL DEFAULT 0,
			domains json DEFAULT NULL,
			ips json DEFAULT NULL,
			port varchar(64) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
			network varchar(16) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
			protocol varchar(16) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
			inbound_tag varchar(64) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
			outbound_tag varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
			remark varchar(128) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
			enabled tinyint NOT NULL DEFAULT 1,
			created_at timestamp NULL DEFAULT NULL,
			updated_at timestamp NULL DEFAULT NULL,
			PRIMARY KEY (id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("增量建表失败: %w", err)
		}
	}

	// 增量加列：HY2 的 salamander 混淆密码（issue #94）。密码是高敏字段，
	// 独立加密列存密文，stream_settings 里只留 {"type":"salamander"} 占位。
	columns := []struct{ table, column, definition string }{
		{"xray_inbounds", "salamander_password", "text COLLATE utf8mb4_unicode_ci"},
		// client_host：某些入站要走 CDN（客户端连 CDN 域名，回源才连节点地址），
		// 订阅链接里的地址需要按入站覆盖节点地址；空 = 维持原行为。
		{"xray_inbounds", "client_host", "varchar(255) COLLATE utf8mb4_unicode_ci"},
		{"users", "balance", "bigint NOT NULL DEFAULT 0 COMMENT '账户余额(分)'"},
		// 兑换码归档到文件夹（对齐优惠码分组）：老库升级时补列，null=未归档。
		{"giftcards", "folder_id", "bigint unsigned DEFAULT NULL COMMENT '所属文件夹 id，null=未归档'"},
		// 订单缓存的支付链接：下单时落库，「我的订单」续付直接复用，不再拿原单号去网关
		// 重新下单（payindex 会把同单号判重 → 每次都被作废成 expired）。过期/支付时清空。
		{"orders", "pay_url", "varchar(2048) COLLATE utf8mb4_unicode_ci DEFAULT NULL COMMENT '缓存的支付链接，过期/支付后清空'"},
		// 支付链接失效时刻 = 下单时间 + 全局支付有效期（site_configs.payment_ttl_minutes）。
		// 由我们自己的时钟判定过期（前端倒计时同源），不再依赖定时任务。老单为 NULL，按缺省 TTL 兜底。
		{"orders", "pay_expires_at", "datetime DEFAULT NULL COMMENT '支付链接失效时刻（下单时按全局支付有效期算出）'"},
		// 地区（#地区功能）：NULL = 该节点不属于任何地区，不做地区过滤（升级默认状态）。
		{"nodes", "region_id", "bigint unsigned DEFAULT NULL COMMENT '所属地区 id，NULL=不参与地区过滤'"},
		// 容量（#地区功能）：该节点能承载的用户数上限，NULL = 不限。
		{"nodes", "capacity", "int DEFAULT NULL COMMENT '承载用户数上限，NULL=不限'"},
		// 批次号（#地区功能-10）：同一次故障迁移 / 批量均衡产生的记录共用一个 batch_id，
		// 撤销接口按它整批反向回滚。NULL = 该行不是通过迁移接口产生的（历史数据 / 其他来源）。
		{"user_node_migrations", "batch_id", "varchar(36) COLLATE utf8mb4_unicode_ci DEFAULT NULL COMMENT '迁移批次号，NULL=非接口产生的记录'"},
		// 套餐绑定地区（#地区功能-15）：NULL = 不限地区（老套餐行为完全不变），
		// [1,3] = 买了该套餐的用户只能落在 1、3 号地区。写入侧把「不勾地区」归一成 NULL，不存 []。
		{"plans", "region_ids", "json DEFAULT NULL COMMENT '允许的地区 id（JSON 数组），NULL=不限地区'"},
		// 独占 socks 池（#地区功能-17）：三个人共用一条线路的现状保持不变，
		// 新增 exclusive = 一条只卖一个人。sold_to/sold_at 记录这条卖给了谁、什么时候，
		// 卖完自动跳到下一条；到期释放（internal/task）把这两列清空，线路重回池子。
		// 配置模板（#地区功能-19）：1 = 该节点的入站/出站/路由被单独改过，
		// 套用模板时默认跳过它（勾「覆盖单独配置」才重写）。套用成功会清回 0。
		{"nodes", "is_custom_config", "tinyint NOT NULL DEFAULT 0 COMMENT '1=该节点配置已单独改过，不跟随模板'"},
		// SMTP 账号池的「默认账号」标记：挑号时优先用 is_default=1 的那个（仍受
		// 日限额 / 冷却 / 停用约束），它不可用时才按「最久未用」轮换其它号。
		// 默认账号只允许有一个；接口里「设为默认」会把其它行清零。
		{"smtp_accounts", "is_default", "tinyint(1) NOT NULL DEFAULT 0 COMMENT '1=默认优先使用的账号'"},
	}
	for _, c := range columns {
		if err := ensureColumn(db, c.table, c.column, c.definition); err != nil {
			return err
		}
	}

	// 增量放长列：node_api_commands.action 原为 varchar(16)，但端口跳跃的三个
	// action（firewall_hop_apply=18 / _remove=19 / _status=19）都超长，插库直接
	// 报「Data too long for column 'action'」——跳跃的防火墙规则永远下发不出去，
	// 面板只看到一句「节点指令入队失败」。放宽到 varchar(32)。
	if err := ensureVarcharAtLeast(db, "node_api_commands", "action", 32); err != nil {
		return err
	}

	// 增量加索引：保留期清理（internal/task/retention.go）按 created_at / (status,updated_at)
	// 删行，没有索引就是全表扫。新库 schema.sql 里已带，老库升级时在这里补齐。
	indexes := []struct{ table, name, definition string }{
		{"mail_logs", "mail_logs_created_at_index", "(`created_at`)"},
		{"node_api_commands", "node_api_commands_status_updated_at_index", "(`status`,`updated_at`)"},
		{"giftcards", "giftcards_folder_id_index", "(`folder_id`)"},
		// 兜底作废任务按 (status, pay_expires_at) 扫超时未支付单；没有它就是全表扫。
		{"orders", "orders_status_pay_expires_at_index", "(`status`,`pay_expires_at`)"},
		// 地区（#地区功能）：按地区筛节点（节点列表、订阅渲染）不能全表扫。
		{"nodes", "idx_nodes_region", "(`region_id`)"},
		// 批次号（#地区功能-10）：撤销按 batch_id 取整批记录，历史列表也可能按它筛。
		{"user_node_migrations", "idx_batch", "(`batch_id`)"},
		// 独占 socks 池（#地区功能-17）：每次下单都是「在池子里找一条未售出的」，
		// 没有这个索引就是全表扫 + 拿不到行锁。
	}
	for _, idx := range indexes {
		if err := ensureIndex(db, idx.table, idx.name, idx.definition); err != nil {
			return err
		}
	}
	return nil
}

// ensureIndex 幂等加索引。
//
// 与 ensureColumn 同思路：MySQL 没有 `ADD INDEX IF NOT EXISTS`（MariaDB 才有），
// 所以先查 information_schema.STATISTICS 再决定要不要 ALTER —— 重复启动、重复升级
// 都不会报「Duplicate key name」。
func ensureIndex(db *sql.DB, table, index, columns string) error {
	var n int
	err := db.QueryRow(
		"SELECT COUNT(*) FROM information_schema.STATISTICS "+
			"WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ?",
		table, index).Scan(&n)
	if err != nil {
		return fmt.Errorf("检查索引 %s.%s 失败: %w", table, index, err)
	}
	if n > 0 {
		return nil
	}
	if _, err := db.Exec("ALTER TABLE `" + table + "` ADD INDEX `" + index + "` " + columns); err != nil {
		return fmt.Errorf("增量加索引 %s.%s 失败: %w", table, index, err)
	}
	return nil
}

// ensureVarcharAtLeast 幂等把 varchar 列放长到至少 n（已经够长就什么都不做）。
//
// 与 ensureColumn 同思路：MySQL 没有 `MODIFY COLUMN IF ...`，先查
// information_schema.CHARACTER_MAXIMUM_LENGTH 再决定要不要 ALTER。
func ensureVarcharAtLeast(db *sql.DB, table, column string, n int) error {
	var cur sql.NullInt64
	err := db.QueryRow(
		"SELECT CHARACTER_MAXIMUM_LENGTH FROM information_schema.COLUMNS "+
			"WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?",
		table, column).Scan(&cur)
	if err != nil {
		return fmt.Errorf("检查列长度 %s.%s 失败: %w", table, column, err)
	}
	if !cur.Valid || cur.Int64 >= int64(n) {
		return nil
	}
	if _, err := db.Exec(fmt.Sprintf(
		"ALTER TABLE `%s` MODIFY COLUMN `%s` varchar(%d) COLLATE utf8mb4_unicode_ci NOT NULL",
		table, column, n)); err != nil {
		return fmt.Errorf("放长列 %s.%s 失败: %w", table, column, err)
	}
	return nil
}

// ensureColumn 幂等加列。
//
// MySQL 没有 `ADD COLUMN IF NOT EXISTS`（MariaDB 才有），所以先查
// information_schema 再决定要不要 ALTER —— 重复启动、重复升级都不会报
// 「Duplicate column name」。
func ensureColumn(db *sql.DB, table, column, definition string) error {
	var n int
	err := db.QueryRow(
		"SELECT COUNT(*) FROM information_schema.COLUMNS "+
			"WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?",
		table, column).Scan(&n)
	if err != nil {
		return fmt.Errorf("检查列 %s.%s 失败: %w", table, column, err)
	}
	if n > 0 {
		return nil
	}
	if _, err := db.Exec("ALTER TABLE `" + table + "` ADD COLUMN `" + column + "` " + definition); err != nil {
		return fmt.Errorf("增量加列 %s.%s 失败: %w", table, column, err)
	}
	return nil
}
