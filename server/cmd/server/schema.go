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
