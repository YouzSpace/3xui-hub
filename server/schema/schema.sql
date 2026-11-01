-- ControlHub schema（Go 后端化：全新安装建表，由 PHP 迁移最终状态导出）
-- 生成时间: 2026-10-24T19:57:49+00:00
SET NAMES utf8mb4;
SET FOREIGN_KEY_CHECKS = 0;

-- Table: access_tokens
CREATE TABLE `access_tokens` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `user_id` bigint unsigned NOT NULL,
  `token` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
  `expires_at` timestamp NULL DEFAULT NULL,
  `last_used_at` timestamp NULL DEFAULT NULL,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `access_tokens_token_unique` (`token`),
  KEY `access_tokens_user_id_index` (`user_id`),
  CONSTRAINT `access_tokens_user_id_foreign` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: admins
CREATE TABLE `admins` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `username` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `password` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `google2fa_secret` varchar(255) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `google2fa_enabled` tinyint(1) NOT NULL DEFAULT '0',
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `admins_username_unique` (`username`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: async_task_items
CREATE TABLE `async_task_items` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `task_id` bigint unsigned NOT NULL,
  `item_key` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `status` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'pending',
  `attempts` int unsigned NOT NULL DEFAULT '0',
  `error` text COLLATE utf8mb4_unicode_ci,
  `started_at` timestamp NULL DEFAULT NULL,
  `finished_at` timestamp NULL DEFAULT NULL,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `async_task_items_task_id_item_key_unique` (`task_id`,`item_key`),
  KEY `async_task_items_status_index` (`status`),
  CONSTRAINT `async_task_items_task_id_foreign` FOREIGN KEY (`task_id`) REFERENCES `async_tasks` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: async_tasks
CREATE TABLE `async_tasks` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `type` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `status` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'pending',
  `subject_type` varchar(255) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `subject_id` bigint unsigned DEFAULT NULL,
  `total` int unsigned NOT NULL DEFAULT '0',
  `completed` int unsigned NOT NULL DEFAULT '0',
  `failed` int unsigned NOT NULL DEFAULT '0',
  `attempts` int unsigned NOT NULL DEFAULT '0',
  `max_attempts` int unsigned NOT NULL DEFAULT '3',
  `error` text COLLATE utf8mb4_unicode_ci,
  `meta` json DEFAULT NULL,
  `started_at` timestamp NULL DEFAULT NULL,
  `finished_at` timestamp NULL DEFAULT NULL,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `async_tasks_subject_type_subject_id_index` (`subject_type`,`subject_id`),
  KEY `async_tasks_type_created_at_index` (`type`,`created_at`),
  KEY `async_tasks_status_index` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: cache
CREATE TABLE `cache` (
  `key` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `value` mediumtext COLLATE utf8mb4_unicode_ci NOT NULL,
  `expiration` bigint NOT NULL,
  PRIMARY KEY (`key`),
  KEY `cache_expiration_index` (`expiration`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: cache_locks
CREATE TABLE `cache_locks` (
  `key` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `owner` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `expiration` bigint NOT NULL,
  PRIMARY KEY (`key`),
  KEY `cache_locks_expiration_index` (`expiration`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: discount_code_folders
CREATE TABLE `discount_code_folders` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `name` varchar(60) COLLATE utf8mb4_unicode_ci NOT NULL COMMENT '文件夹名',
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: discount_codes
CREATE TABLE `discount_codes` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `code` varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL COMMENT '码本身，大写',
  `source` varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL COMMENT 'invite | redeem | admin',
  `user_id` bigint unsigned DEFAULT NULL COMMENT '持有者（admin 生成的为 null）',
  `discount` decimal(3,2) NOT NULL COMMENT '0.90 表示 9 折',
  `max_uses` int unsigned NOT NULL DEFAULT '1' COMMENT '该码总可用次数',
  `used_count` int unsigned NOT NULL DEFAULT '0' COMMENT '已使用次数',
  `max_uses_per_user` int unsigned NOT NULL DEFAULT '1' COMMENT '每用户可用次数',
  `expires_at` datetime DEFAULT NULL COMMENT '有效期，null = 永久',
  `note` varchar(255) COLLATE utf8mb4_unicode_ci DEFAULT NULL COMMENT '自定义说明文字（展示给用户）',
  `status` varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'active' COMMENT 'active | used_up | expired | refreshed',
  `plan_id` bigint unsigned DEFAULT NULL COMMENT '绑定套餐，null = 不限',
  `folder_id` bigint unsigned DEFAULT NULL COMMENT '所属文件夹 id，null = 未归档',
  `source_order_id` bigint unsigned DEFAULT NULL COMMENT '兑换码来源订单 id（发奖幂等）',
  `refreshed_at` timestamp NULL DEFAULT NULL COMMENT '邀请码最近刷新时间',
  `next_refresh_at` timestamp NULL DEFAULT NULL COMMENT '邀请码下次刷新时间',
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `discount_codes_code_unique` (`code`),
  KEY `discount_codes_user_id_source_index` (`user_id`,`source`),
  KEY `discount_codes_source_status_index` (`source`,`status`),
  KEY `discount_codes_folder_id_index` (`folder_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: domains
CREATE TABLE `domains` (
  `id` int unsigned NOT NULL AUTO_INCREMENT,
  `domain` varchar(190) COLLATE utf8mb4_unicode_ci NOT NULL,
  `is_primary` tinyint(1) NOT NULL DEFAULT '0',
  `enabled` tinyint(1) NOT NULL DEFAULT '1',
  `ssl_status` varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'none',
  `cert_expires_at` datetime DEFAULT NULL,
  `last_error` text COLLATE utf8mb4_unicode_ci,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `domains_domain_unique` (`domain`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: failed_jobs
CREATE TABLE `failed_jobs` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `uuid` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `connection` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `queue` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `payload` longtext COLLATE utf8mb4_unicode_ci NOT NULL,
  `exception` longtext COLLATE utf8mb4_unicode_ci NOT NULL,
  `failed_at` timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `failed_jobs_uuid_unique` (`uuid`),
  KEY `failed_jobs_connection_queue_failed_at_index` (`connection`,`queue`,`failed_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: job_batches
CREATE TABLE `job_batches` (
  `id` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `name` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `total_jobs` int NOT NULL,
  `pending_jobs` int NOT NULL,
  `failed_jobs` int NOT NULL,
  `failed_job_ids` longtext COLLATE utf8mb4_unicode_ci NOT NULL,
  `options` mediumtext COLLATE utf8mb4_unicode_ci,
  `cancelled_at` int DEFAULT NULL,
  `created_at` int NOT NULL,
  `finished_at` int DEFAULT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: jobs
CREATE TABLE `jobs` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `queue` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `payload` longtext COLLATE utf8mb4_unicode_ci NOT NULL,
  `attempts` smallint unsigned NOT NULL,
  `reserved_at` int unsigned DEFAULT NULL,
  `available_at` int unsigned NOT NULL,
  `created_at` int unsigned NOT NULL,
  PRIMARY KEY (`id`),
  KEY `jobs_queue_index` (`queue`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: mail_logs
CREATE TABLE `mail_logs` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `type` varchar(20) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'batch',
  `status` varchar(20) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'sent',
  `to_email` varchar(255) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `to_user_id` varchar(255) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `subject` varchar(255) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `error` text COLLATE utf8mb4_unicode_ci,
  `scene` varchar(40) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `batch_id` bigint unsigned DEFAULT NULL,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `mail_logs_type_status_index` (`type`,`status`),
  KEY `mail_logs_batch_id_index` (`batch_id`),
  KEY `mail_logs_to_user_id_index` (`to_user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: migrations
CREATE TABLE `migrations` (
  `id` int unsigned NOT NULL AUTO_INCREMENT,
  `migration` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `batch` int NOT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: monitor_daily
CREATE TABLE `monitor_daily` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `node_id` bigint unsigned NOT NULL,
  `date` date NOT NULL COMMENT '站点时区下的自然日',
  `sent_delta` bigint unsigned NOT NULL DEFAULT '0',
  `recv_delta` bigint unsigned NOT NULL DEFAULT '0',
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `monitor_daily_node_id_date_unique` (`node_id`,`date`),
  CONSTRAINT `monitor_daily_node_id_foreign` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: monitor_samples
CREATE TABLE `monitor_samples` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `node_id` bigint unsigned NOT NULL,
  `sent` bigint unsigned NOT NULL DEFAULT '0' COMMENT '累计发送字节（netTraffic.sent / agent net_total_up）',
  `recv` bigint unsigned NOT NULL DEFAULT '0' COMMENT '累计接收字节（netTraffic.recv / agent net_total_down）',
  `cpu` decimal(5,2) DEFAULT NULL,
  `mem` decimal(5,2) DEFAULT NULL,
  `sampled_at` datetime NOT NULL,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `monitor_samples_node_id_sampled_at_index` (`node_id`,`sampled_at`),
  KEY `monitor_samples_sampled_at_index` (`sampled_at`),
  CONSTRAINT `monitor_samples_node_id_foreign` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: node_api_commands
CREATE TABLE `node_api_commands` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `node_id` bigint unsigned NOT NULL,
  `user_id` bigint unsigned DEFAULT NULL,
  `action` varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL,
  `payload` json DEFAULT NULL,
  `status` varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'pending',
  `result` text COLLATE utf8mb4_unicode_ci,
  `acknowledged_at` timestamp NULL DEFAULT NULL,
  `executed_at` timestamp NULL DEFAULT NULL,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `node_api_commands_user_id_foreign` (`user_id`),
  KEY `node_api_commands_node_id_status_created_at_index` (`node_id`,`status`,`created_at`),
  KEY `node_api_commands_status_index` (`status`),
  CONSTRAINT `node_api_commands_node_id_foreign` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE,
  CONSTRAINT `node_api_commands_user_id_foreign` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: node_inbounds
CREATE TABLE `node_inbounds` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `node_id` bigint unsigned NOT NULL,
  `protocol` varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL,
  `driver_type` varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '3x-ui',
  `inbound_id` bigint NOT NULL,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `node_inbounds_node_id_protocol_inbound_id_unique` (`node_id`,`protocol`,`inbound_id`),
  CONSTRAINT `node_inbounds_node_id_foreign` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: node_warp_accounts
CREATE TABLE `node_warp_accounts` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `node_id` bigint unsigned NOT NULL,
  `device_id` varchar(64) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `access_token` text COLLATE utf8mb4_unicode_ci,
  `private_key` text COLLATE utf8mb4_unicode_ci,
  `public_key` varchar(64) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `peer_public_key` varchar(128) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `peer_endpoint` varchar(190) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `addresses` json DEFAULT NULL,
  `reserved` json DEFAULT NULL,
  `license_key` varchar(128) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `client_id` varchar(64) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `auto_rotate_hours` smallint unsigned NOT NULL DEFAULT '0',
  `last_rotate_at` timestamp NULL DEFAULT NULL,
  `enabled` tinyint(1) NOT NULL DEFAULT '0',
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `node_warp_accounts_node_id_unique` (`node_id`),
  CONSTRAINT `node_warp_accounts_node_id_foreign` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: nodes
CREATE TABLE `nodes` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `name` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `host` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `port` int NOT NULL DEFAULT '443',
  `scheme` varchar(8) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'https',
  `web_base_path` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
  `username` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
  `password` text COLLATE utf8mb4_unicode_ci,
  `api_key` text COLLATE utf8mb4_unicode_ci,
  `enabled` tinyint(1) NOT NULL DEFAULT '1',
  `driver_type` varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '3x-ui',
  `driver_version` varchar(16) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `driver_config` json DEFAULT NULL,
  `verify_ssl` tinyint(1) NOT NULL DEFAULT '0',
  `status` varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'offline',
  `traffic_multiplier` decimal(6,2) DEFAULT NULL,
  `latency` int NOT NULL DEFAULT '0',
  `cpu` decimal(5,2) DEFAULT NULL,
  `mem` decimal(5,2) DEFAULT NULL,
  `last_check_at` timestamp NULL DEFAULT NULL,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  `deleted_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `nodes_driver_type_enabled_index` (`driver_type`,`enabled`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: operation_logs
CREATE TABLE `operation_logs` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `type` varchar(20) COLLATE utf8mb4_unicode_ci NOT NULL,
  `actor_id` bigint unsigned DEFAULT NULL,
  `actor_name` varchar(255) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `action` varchar(120) COLLATE utf8mb4_unicode_ci NOT NULL,
  `method` varchar(10) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `path` varchar(255) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `status` int DEFAULT NULL,
  `error` text COLLATE utf8mb4_unicode_ci,
  `ip` varchar(45) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `created_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `operation_logs_type_created_at_index` (`type`,`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: orders
CREATE TABLE `orders` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `order_no` varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL COMMENT '订单号',
  `user_id` bigint unsigned NOT NULL,
  `order_type` varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'plan' COMMENT '订单类型：plan=节点套餐 / proxy=代理套餐',
  `plan_id` bigint unsigned DEFAULT NULL,
  `proxy_sku_id` bigint unsigned DEFAULT NULL COMMENT '代理规格 id（仅代理订单）',
  `proxy_renewal` tinyint(1) NOT NULL DEFAULT '0' COMMENT '代理订单：true=续费（不占库存），false=新开通',
  `amount` decimal(10,2) NOT NULL COMMENT '订单金额(元)',
  `original_amount` decimal(10,2) DEFAULT NULL COMMENT '原价',
  `discount_code_id` bigint unsigned DEFAULT NULL COMMENT '使用的折扣码 id',
  `discount_amount` decimal(10,2) DEFAULT NULL COMMENT '实际优惠掉的金额',
  `currency` varchar(8) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'CNY',
  `status` varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'pending' COMMENT 'pending/paid/failed/expired',
  `payment_config_id` bigint unsigned DEFAULT NULL,
  `payment_driver_type` varchar(32) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `payment_metadata` json DEFAULT NULL,
  `trade_no` varchar(64) COLLATE utf8mb4_unicode_ci DEFAULT NULL COMMENT '第三方交易号',
  `paid_at` timestamp NULL DEFAULT NULL,
  `pay_ip` varchar(45) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `orders_order_no_unique` (`order_no`),
  KEY `orders_user_id_foreign` (`user_id`),
  KEY `orders_payment_config_id_foreign` (`payment_config_id`),
  KEY `orders_plan_id_foreign` (`plan_id`),
  KEY `orders_order_type_status_index` (`order_type`,`status`),
  CONSTRAINT `orders_payment_config_id_foreign` FOREIGN KEY (`payment_config_id`) REFERENCES `payment_configs` (`id`) ON DELETE SET NULL,
  CONSTRAINT `orders_plan_id_foreign` FOREIGN KEY (`plan_id`) REFERENCES `plans` (`id`) ON DELETE CASCADE,
  CONSTRAINT `orders_user_id_foreign` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: password_reset_tokens
CREATE TABLE `password_reset_tokens` (
  `email` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `token` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `created_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`email`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: payment_configs
CREATE TABLE `payment_configs` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `name` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL COMMENT '配置名称',
  `driver_type` varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'wwspay',
  `driver_config` json DEFAULT NULL,
  `gateway` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL COMMENT '支付网关地址',
  `query_gateway` varchar(255) COLLATE utf8mb4_unicode_ci DEFAULT NULL COMMENT '查询网关地址',
  `member_id` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL COMMENT '商户号',
  `api_key` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL COMMENT 'API密钥',
  `notify_url` varchar(255) COLLATE utf8mb4_unicode_ci DEFAULT NULL COMMENT '回调地址',
  `bank_code` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'alipay' COMMENT '支付方式编码',
  `enabled` tinyint(1) NOT NULL DEFAULT '1',
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: plans
CREATE TABLE `plans` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `name` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `price` decimal(10,2) NOT NULL DEFAULT '0.00',
  `reset_price` decimal(10,2) NOT NULL DEFAULT '0.00' COMMENT '重置流量价格(元)',
  `type` varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL,
  `months` int DEFAULT NULL,
  `monthly_traffic` bigint DEFAULT NULL,
  `period_traffic` bigint DEFAULT NULL,
  `total_traffic` bigint DEFAULT NULL,
  `is_active` tinyint(1) NOT NULL DEFAULT '1' COMMENT '是否上架可购买',
  `include_local` tinyint(1) NOT NULL DEFAULT '1' COMMENT '订阅是否包含本地节点',
  `third_party_sub_ids` text COLLATE utf8mb4_unicode_ci COMMENT '注入的第三方订阅 ID（JSON 数组，空 = 不含第三方）',
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: proxy_accounts
CREATE TABLE `proxy_accounts` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `provider_id` bigint unsigned NOT NULL,
  `upstream_id` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL COMMENT 'Webshare subuser id（字符串化，防供应商改类型）',
  `user_id` bigint unsigned DEFAULT NULL,
  `sku_id` bigint unsigned DEFAULT NULL COMMENT '购买的代理规格（手动开的号为空）',
  `label` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL COMMENT '子用户备注（Webshare 侧 label）',
  `username` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL COMMENT '代理用户名',
  `password` text COLLATE utf8mb4_unicode_ci COMMENT '代理密码（encrypted cast 加密存储）',
  `proxy_limit` decimal(8,2) NOT NULL DEFAULT '0.00' COMMENT '带宽上限 GB，0=不限量',
  `traffic_limit_bytes` bigint unsigned DEFAULT NULL COMMENT '本账号流量上限（字节，= SKU 的 traffic_gb）',
  `expires_at` timestamp NULL DEFAULT NULL COMMENT '到期时间（手动开的号为空 = 不过期）',
  `order_id` bigint unsigned DEFAULT NULL COMMENT '首次开通订单 id（续费不覆盖）',
  `max_thread_count` int unsigned NOT NULL DEFAULT '0' COMMENT '并发线程上限',
  `proxy_countries` json DEFAULT NULL COMMENT '国家池 {"US":500,...}',
  `used_bytes` bigint unsigned NOT NULL DEFAULT '0' COMMENT '已用流量（字节，上游回采）',
  `status` varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'active' COMMENT 'active/disabled',
  `last_synced_at` timestamp NULL DEFAULT NULL COMMENT '最近一次用量回采时间',
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `proxy_accounts_provider_id_upstream_id_unique` (`provider_id`,`upstream_id`),
  KEY `proxy_accounts_user_id_status_index` (`user_id`,`status`),
  CONSTRAINT `proxy_accounts_provider_id_foreign` FOREIGN KEY (`provider_id`) REFERENCES `proxy_providers` (`id`) ON DELETE CASCADE,
  CONSTRAINT `proxy_accounts_user_id_foreign` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: proxy_entitlements
CREATE TABLE `proxy_entitlements` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `user_id` bigint unsigned NOT NULL COMMENT '购买人',
  `order_id` bigint unsigned DEFAULT NULL COMMENT '开通/续费的订单',
  `sku_id` bigint unsigned NOT NULL COMMENT '购买的规格',
  `kind` varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL COMMENT '凭据类型：webshare=独占子账号 / socks=共享 socks',
  `account_id` bigint unsigned DEFAULT NULL COMMENT 'proxy_accounts.id（webshare）',
  `socks_id` bigint unsigned DEFAULT NULL COMMENT 'proxy_socks.id（共享）',
  `expires_at` timestamp NULL DEFAULT NULL COMMENT '到期时间',
  `status` varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'active' COMMENT 'active / expired / disabled',
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `proxy_entitlements_user_id_status_index` (`user_id`,`status`),
  KEY `proxy_entitlements_status_expires_at_index` (`status`,`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: proxy_products
CREATE TABLE `proxy_products` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `name` varchar(100) COLLATE utf8mb4_unicode_ci NOT NULL COMMENT '商品名（对外展示）',
  `subtitle` varchar(200) COLLATE utf8mb4_unicode_ci DEFAULT NULL COMMENT '一句话卖点',
  `description` text COLLATE utf8mb4_unicode_ci COMMENT '详情（支持纯文本/简单 Markdown）',
  `image_url` varchar(500) COLLATE utf8mb4_unicode_ci DEFAULT NULL COMMENT '封面图地址',
  `is_active` tinyint(1) NOT NULL DEFAULT '1' COMMENT '上架开关',
  `sort_order` int unsigned NOT NULL DEFAULT '0' COMMENT '排序，小的在前',
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `proxy_products_is_active_sort_order_index` (`is_active`,`sort_order`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: proxy_providers
CREATE TABLE `proxy_providers` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `name` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL COMMENT '管理员备注名（仅后台可见）',
  `driver` varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'webshare' COMMENT '上游类型，当前只有 webshare',
  `api_token` text COLLATE utf8mb4_unicode_ci NOT NULL COMMENT '上游 API Token（encrypted cast 加密存储）',
  `enabled` tinyint(1) NOT NULL DEFAULT '1' COMMENT '是否参与开号/同步',
  `last_status` varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'pending' COMMENT 'pending/success/failed',
  `last_error` varchar(255) COLLATE utf8mb4_unicode_ci DEFAULT NULL COMMENT '最近一次失败原因',
  `last_checked_at` timestamp NULL DEFAULT NULL COMMENT '最近一次检查时间',
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: proxy_skus
CREATE TABLE `proxy_skus` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `product_id` bigint unsigned NOT NULL COMMENT '所属商品',
  `name` varchar(100) COLLATE utf8mb4_unicode_ci NOT NULL COMMENT '规格名，如 100G/30天',
  `source_type` varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'webshare' COMMENT '货源类型：webshare / socks',
  `allocation` varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'exclusive' COMMENT '分配方式：exclusive=独占账号 / shared=多人共用',
  `source_id` bigint unsigned DEFAULT NULL COMMENT '关联货源：webshare=proxy_providers.id；socks=proxy_socks.id',
  `region` varchar(32) COLLATE utf8mb4_unicode_ci DEFAULT NULL COMMENT '地区（US/GB/ZZ=不限），null = 不限',
  `traffic_gb` int unsigned NOT NULL COMMENT '流量额度（GB，建号时写进上游 proxy_limit）',
  `duration_days` int unsigned NOT NULL DEFAULT '30' COMMENT '有效期天数',
  `max_thread_count` int unsigned NOT NULL DEFAULT '0' COMMENT '并发线程数，0 = 上游默认',
  `price` decimal(10,2) NOT NULL DEFAULT '0.00' COMMENT '售价',
  `cost_price` decimal(10,2) NOT NULL DEFAULT '0.00' COMMENT '成本价（算毛利，不对外）',
  `stock_total` int unsigned NOT NULL DEFAULT '0' COMMENT '可售总数（= 上游可开子用户数）',
  `stock_locked` int unsigned NOT NULL DEFAULT '0' COMMENT '待支付占用',
  `stock_sold` int unsigned NOT NULL DEFAULT '0' COMMENT '已售出',
  `is_active` tinyint(1) NOT NULL DEFAULT '1' COMMENT '规格启用',
  `sort_order` int unsigned NOT NULL DEFAULT '0' COMMENT '排序',
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `proxy_skus_product_id_is_active_sort_order_index` (`product_id`,`is_active`,`sort_order`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: proxy_socks
CREATE TABLE `proxy_socks` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `name` varchar(100) COLLATE utf8mb4_unicode_ci NOT NULL COMMENT '备注名，如 美国住宅 socks 池',
  `host` varchar(190) COLLATE utf8mb4_unicode_ci NOT NULL COMMENT '地址（IP 或域名）',
  `port` int unsigned NOT NULL COMMENT '端口',
  `username` varchar(190) COLLATE utf8mb4_unicode_ci DEFAULT NULL COMMENT '认证账号（无认证留空）',
  `password` text COLLATE utf8mb4_unicode_ci COMMENT '认证密码（encrypted cast）',
  `country` varchar(16) COLLATE utf8mb4_unicode_ci DEFAULT NULL COMMENT '国家代码，如 US',
  `city` varchar(64) COLLATE utf8mb4_unicode_ci DEFAULT NULL COMMENT '城市',
  `note` varchar(255) COLLATE utf8mb4_unicode_ci DEFAULT NULL COMMENT '备注',
  `is_active` tinyint(1) NOT NULL DEFAULT '1' COMMENT '启用',
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `proxy_socks_is_active_index` (`is_active`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: scheduled_task_runs
CREATE TABLE `scheduled_task_runs` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `command` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `status` varchar(20) COLLATE utf8mb4_unicode_ci NOT NULL,
  `error` text COLLATE utf8mb4_unicode_ci,
  `duration_ms` int unsigned DEFAULT NULL,
  `ran_at` timestamp NOT NULL,
  PRIMARY KEY (`id`),
  KEY `scheduled_task_runs_ran_at_index` (`ran_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: sessions
CREATE TABLE `sessions` (
  `id` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `user_id` bigint unsigned DEFAULT NULL,
  `ip_address` varchar(45) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `user_agent` text COLLATE utf8mb4_unicode_ci,
  `payload` longtext COLLATE utf8mb4_unicode_ci NOT NULL,
  `last_activity` int NOT NULL,
  PRIMARY KEY (`id`),
  KEY `sessions_user_id_index` (`user_id`),
  KEY `sessions_last_activity_index` (`last_activity`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: site_configs
CREATE TABLE `site_configs` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `key` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `value` text COLLATE utf8mb4_unicode_ci,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `site_configs_key_unique` (`key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: smtp_accounts
CREATE TABLE `smtp_accounts` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `host` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `port` int NOT NULL DEFAULT '587',
  `username` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '',
  `password_enc` text COLLATE utf8mb4_unicode_ci,
  `encryption` varchar(8) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'tls',
  `from_name` varchar(100) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `weight` int NOT NULL DEFAULT '1',
  `daily_limit` int NOT NULL DEFAULT '0',
  `sent_today` int NOT NULL DEFAULT '0',
  `last_used_at` timestamp NULL DEFAULT NULL,
  `reset_at` timestamp NULL DEFAULT NULL,
  `status` varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'active',
  `cooldown_until` timestamp NULL DEFAULT NULL,
  `error_count` int NOT NULL DEFAULT '0',
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `smtp_accounts_status_index` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: third_party_subs
CREATE TABLE `third_party_subs` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `name` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL COMMENT '管理员备注名（仅后台可见）',
  `url` text COLLATE utf8mb4_unicode_ci NOT NULL COMMENT '第三方订阅链接（含机场 token，仅管理端展示）',
  `enabled` tinyint(1) NOT NULL DEFAULT '1' COMMENT '这条订阅是否参与注入',
  `cached_links` mediumtext COLLATE utf8mb4_unicode_ci COMMENT '最近一次拉取到的节点链接缓存',
  `node_count` int unsigned NOT NULL DEFAULT '0' COMMENT '最近一次拉取到的节点数',
  `last_fetched_at` timestamp NULL DEFAULT NULL COMMENT '最近一次拉取时间',
  `last_status` varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'pending' COMMENT 'pending/success/failed',
  `last_error` varchar(255) COLLATE utf8mb4_unicode_ci DEFAULT NULL COMMENT '最近一次拉取失败原因',
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: traffic_snapshots
CREATE TABLE `traffic_snapshots` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `user_id` bigint unsigned NOT NULL,
  `node_id` bigint unsigned NOT NULL,
  `upload` bigint NOT NULL DEFAULT '0',
  `download` bigint NOT NULL DEFAULT '0',
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `traffic_snapshots_user_id_node_id_unique` (`user_id`,`node_id`),
  KEY `traffic_snapshots_node_id_foreign` (`node_id`),
  CONSTRAINT `traffic_snapshots_node_id_foreign` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE,
  CONSTRAINT `traffic_snapshots_user_id_foreign` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: tutorials
CREATE TABLE `tutorials` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `title` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `category` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '默认',
  `content` longtext COLLATE utf8mb4_unicode_ci NOT NULL,
  `sort` int NOT NULL DEFAULT '0',
  `enabled` tinyint(1) NOT NULL DEFAULT '1',
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: user_traffic_daily
CREATE TABLE `user_traffic_daily` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `user_id` bigint unsigned NOT NULL,
  `node_id` bigint unsigned NOT NULL,
  `date` date NOT NULL,
  `up_delta` bigint unsigned NOT NULL DEFAULT '0' COMMENT '面板侧原始上行字节（未乘倍率）',
  `down_delta` bigint unsigned NOT NULL DEFAULT '0' COMMENT '面板侧原始下行字节（未乘倍率）',
  `weighted_delta` bigint unsigned NOT NULL DEFAULT '0' COMMENT '乘节点倍率后的字节（= 套餐扣费口径）',
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `user_traffic_daily_user_id_node_id_date_unique` (`user_id`,`node_id`,`date`),
  KEY `user_traffic_daily_node_id_foreign` (`node_id`),
  KEY `user_traffic_daily_date_user_id_index` (`date`,`user_id`),
  KEY `user_traffic_daily_date_node_id_index` (`date`,`node_id`),
  CONSTRAINT `user_traffic_daily_node_id_foreign` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE,
  CONSTRAINT `user_traffic_daily_user_id_foreign` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: user_traffic_hourly
CREATE TABLE `user_traffic_hourly` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `user_id` bigint unsigned NOT NULL,
  `node_id` bigint unsigned NOT NULL,
  `hour_start` datetime NOT NULL,
  `up_delta` bigint unsigned NOT NULL DEFAULT '0' COMMENT '面板侧原始上行字节（未乘倍率）',
  `down_delta` bigint unsigned NOT NULL DEFAULT '0' COMMENT '面板侧原始下行字节（未乘倍率）',
  `weighted_delta` bigint unsigned NOT NULL DEFAULT '0' COMMENT '乘节点倍率后的字节（= 套餐扣费口径）',
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `user_traffic_hourly_unique` (`user_id`,`node_id`,`hour_start`),
  KEY `user_traffic_hourly_node_id_foreign` (`node_id`),
  KEY `user_traffic_hourly_hour_start_user_id_index` (`hour_start`,`user_id`),
  KEY `user_traffic_hourly_hour_start_node_id_index` (`hour_start`,`node_id`),
  CONSTRAINT `user_traffic_hourly_node_id_foreign` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE,
  CONSTRAINT `user_traffic_hourly_user_id_foreign` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: users
CREATE TABLE `users` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `email` varchar(255) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `password` varchar(255) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `token` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
  `uuid` char(36) COLLATE utf8mb4_unicode_ci NOT NULL,
  `driver_identifier` varchar(128) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `client_config` json DEFAULT NULL,
  `protocol` varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'vless',
  `plan_id` bigint unsigned DEFAULT NULL,
  `traffic_limit` bigint NOT NULL DEFAULT '0',
  `traffic_used` bigint NOT NULL DEFAULT '0',
  `monthly_traffic_used` bigint NOT NULL DEFAULT '0',
  `monthly_traffic_limit` bigint NOT NULL DEFAULT '0',
  `next_traffic_reset_at` datetime DEFAULT NULL COMMENT '下次流量重置时间',
  `expired_at` datetime DEFAULT NULL,
  `enabled` tinyint(1) NOT NULL DEFAULT '1',
  `traffic_disabled_at` timestamp NULL DEFAULT NULL COMMENT '最近一次确认关闭 3x-ui 流量的时间，null=未关闭',
  `used_invite_code_id` bigint unsigned DEFAULT NULL COMMENT '该用户用过的邀请码 id，非空即永久锁死',
  `invite_code_used_at` timestamp NULL DEFAULT NULL COMMENT '邀请码使用时间',
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  `deleted_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `users_token_unique` (`token`),
  UNIQUE KEY `users_uuid_unique` (`uuid`),
  KEY `users_plan_id_foreign` (`plan_id`),
  KEY `users_updated_at_index` (`updated_at`),
  CONSTRAINT `users_plan_id_foreign` FOREIGN KEY (`plan_id`) REFERENCES `plans` (`id`) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: xray_balancers
CREATE TABLE `xray_balancers` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `node_id` bigint unsigned NOT NULL,
  `tag` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
  `selectors` json NOT NULL,
  `strategy` varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'roundRobin',
  `fallback_tag` varchar(64) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `remark` varchar(128) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `enabled` tinyint(1) NOT NULL DEFAULT '1',
  `sort` int unsigned NOT NULL DEFAULT '0',
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `xray_balancers_node_id_tag_unique` (`node_id`,`tag`),
  KEY `xray_balancers_node_id_enabled_sort_index` (`node_id`,`enabled`,`sort`),
  CONSTRAINT `xray_balancers_node_id_foreign` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: xray_inbounds
CREATE TABLE `xray_inbounds` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `node_id` bigint unsigned NOT NULL,
  `tag` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
  `remark` varchar(128) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `protocol` varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL,
  `port` int unsigned NOT NULL,
  `listen` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '0.0.0.0',
  `settings` json DEFAULT NULL,
  `stream_settings` json DEFAULT NULL,
  `sniffing` json DEFAULT NULL,
  `reality_private_key` text COLLATE utf8mb4_unicode_ci,
  `salamander_password` text COLLATE utf8mb4_unicode_ci,
  `enabled` tinyint(1) NOT NULL DEFAULT '1',
  `sort` int unsigned NOT NULL DEFAULT '0',
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `xray_inbounds_node_id_tag_unique` (`node_id`,`tag`),
  KEY `xray_inbounds_node_id_enabled_index` (`node_id`,`enabled`),
  CONSTRAINT `xray_inbounds_node_id_foreign` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: xray_outbound_subscriptions
CREATE TABLE `xray_outbound_subscriptions` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `node_id` bigint unsigned NOT NULL,
  `remark` varchar(128) COLLATE utf8mb4_unicode_ci DEFAULT NULL COMMENT '管理员备注名',
  `url` text COLLATE utf8mb4_unicode_ci NOT NULL COMMENT '订阅地址（http/https）',
  `tag_prefix` varchar(64) COLLATE utf8mb4_unicode_ci DEFAULT NULL COMMENT '出站 tag 前缀；留空回退 sub<id>-',
  `update_interval` int unsigned NOT NULL DEFAULT '600' COMMENT '刷新间隔（秒）',
  `prepend` tinyint(1) NOT NULL DEFAULT '0' COMMENT '注入位置：true=排在自定义出站之前',
  `priority` int unsigned NOT NULL DEFAULT '0' COMMENT '同组内排序（小者靠前）',
  `enabled` tinyint(1) NOT NULL DEFAULT '1',
  `fetched` mediumtext COLLATE utf8mb4_unicode_ci COMMENT '最近一次成功拉取的出站 JSON 数组',
  `link_identities` mediumtext COLLATE utf8mb4_unicode_ci COMMENT 'identity → tag 映射（保 tag 稳定）',
  `content_hash` varchar(64) COLLATE utf8mb4_unicode_ci DEFAULT NULL COMMENT '最近一次成功拉取的内容指纹',
  `node_count` int unsigned NOT NULL DEFAULT '0' COMMENT '最近一次拉取到的出站数',
  `last_fetched_at` timestamp NULL DEFAULT NULL COMMENT '最近一次拉取时间',
  `last_status` varchar(16) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT 'pending' COMMENT 'pending/success/failed',
  `last_error` varchar(500) COLLATE utf8mb4_unicode_ci DEFAULT NULL COMMENT '最近一次拉取失败原因',
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `xray_outbound_subscriptions_node_id_enabled_index` (`node_id`,`enabled`),
  KEY `xray_outbound_subscriptions_enabled_priority_index` (`enabled`,`priority`),
  CONSTRAINT `xray_outbound_subscriptions_node_id_foreign` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: xray_outbound_traffic
CREATE TABLE `xray_outbound_traffic` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `node_id` bigint unsigned NOT NULL,
  `tag` varchar(128) COLLATE utf8mb4_unicode_ci NOT NULL,
  `upload` bigint unsigned NOT NULL DEFAULT '0',
  `download` bigint unsigned NOT NULL DEFAULT '0',
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `xray_outbound_traffic_node_id_tag_unique` (`node_id`,`tag`),
  CONSTRAINT `xray_outbound_traffic_node_id_foreign` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: xray_outbounds
CREATE TABLE `xray_outbounds` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `node_id` bigint unsigned NOT NULL,
  `tag` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
  `protocol` varchar(32) COLLATE utf8mb4_unicode_ci NOT NULL,
  `settings` json DEFAULT NULL,
  `stream_settings` json DEFAULT NULL,
  `secret_key` text COLLATE utf8mb4_unicode_ci,
  `enabled` tinyint(1) NOT NULL DEFAULT '1',
  `sort` int unsigned NOT NULL DEFAULT '0',
  `remark` varchar(128) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `relay_target_node_id` bigint unsigned DEFAULT NULL,
  `relay_target_inbound_id` bigint unsigned DEFAULT NULL,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `xray_outbounds_node_id_tag_unique` (`node_id`,`tag`),
  KEY `xray_outbounds_node_id_enabled_index` (`node_id`,`enabled`),
  CONSTRAINT `xray_outbounds_node_id_foreign` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: xray_relay_clients
CREATE TABLE `xray_relay_clients` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `node_id` bigint unsigned NOT NULL,
  `inbound_id` bigint unsigned NOT NULL,
  `email` varchar(128) COLLATE utf8mb4_unicode_ci NOT NULL,
  `client_id` varchar(255) COLLATE utf8mb4_unicode_ci NOT NULL,
  `source_node_id` bigint unsigned DEFAULT NULL,
  `source_inbound_id` bigint unsigned DEFAULT NULL,
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `xray_relay_clients_node_id_email_unique` (`node_id`,`email`),
  UNIQUE KEY `xray_relay_clients_node_id_inbound_id_client_id_unique` (`node_id`,`inbound_id`,`client_id`),
  KEY `xray_relay_clients_inbound_id_foreign` (`inbound_id`),
  KEY `xray_relay_clients_source_node_id_source_inbound_id_index` (`source_node_id`,`source_inbound_id`),
  CONSTRAINT `xray_relay_clients_inbound_id_foreign` FOREIGN KEY (`inbound_id`) REFERENCES `xray_inbounds` (`id`) ON DELETE CASCADE,
  CONSTRAINT `xray_relay_clients_node_id_foreign` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE,
  CONSTRAINT `xray_relay_clients_source_node_id_foreign` FOREIGN KEY (`source_node_id`) REFERENCES `nodes` (`id`) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Table: xray_routing_rules
CREATE TABLE `xray_routing_rules` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `node_id` bigint unsigned NOT NULL,
  `domains` json DEFAULT NULL,
  `ips` json DEFAULT NULL,
  `port` varchar(64) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `network` varchar(16) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `protocol` varchar(16) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `inbound_tag` varchar(64) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `outbound_tag` varchar(64) COLLATE utf8mb4_unicode_ci NOT NULL,
  `remark` varchar(128) COLLATE utf8mb4_unicode_ci DEFAULT NULL,
  `enabled` tinyint(1) NOT NULL DEFAULT '1',
  `sort` int unsigned NOT NULL DEFAULT '0',
  `created_at` timestamp NULL DEFAULT NULL,
  `updated_at` timestamp NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `xray_routing_rules_node_id_enabled_sort_index` (`node_id`,`enabled`,`sort`),
  CONSTRAINT `xray_routing_rules_node_id_foreign` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

SET FOREIGN_KEY_CHECKS = 1;
