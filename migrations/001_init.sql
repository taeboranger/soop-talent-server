-- SoopTalent MySQL 8 초기 스키마.
-- 이 파일 하나가 전체 스키마다. 새 DB에서 한 번 성공하면 schema_migrations에
-- 기록되므로 Pod가 재시작되어도 다시 실행되지 않는다.
-- 모든 CREATE는 IF NOT EXISTS여서 최초 실행 중 Pod가 끊겨도 다음 실행이 이어받는다.

CREATE TABLE IF NOT EXISTS streamers (
    bid VARCHAR(255) NOT NULL,
    nickname VARCHAR(255) NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    chat_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (bid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS broadcasts (
    broad_no BIGINT NOT NULL,
    bid VARCHAR(255) NOT NULL,
    title TEXT NULL,
    started_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    ended_at DATETIME(6) NULL,
    PRIMARY KEY (broad_no),
    INDEX broadcasts_bid_started_idx (bid, started_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS users (
    bid VARCHAR(255) NOT NULL,
    user_id VARCHAR(255) NOT NULL,
    nickname VARCHAR(255) NULL,
    fanclub_weeks INT NOT NULL DEFAULT -1,
    first_seen DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    last_seen DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (bid, user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS raw_chat_events (
    id BIGINT NOT NULL AUTO_INCREMENT,
    bid VARCHAR(255) NOT NULL,
    broad_no BIGINT NULL,
    svc VARCHAR(16) NOT NULL,
    parts JSON NOT NULL,
    raw LONGBLOB NULL,
    recv_at DATETIME(6) NOT NULL,
    PRIMARY KEY (id),
    INDEX raw_svc_recv_idx (svc, recv_at),
    INDEX raw_bid_recv_idx (bid, recv_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS point_events (
    id BIGINT NOT NULL AUTO_INCREMENT,
    bid VARCHAR(255) NOT NULL,
    user_id VARCHAR(255) NOT NULL,
    kind VARCHAR(64) NOT NULL,
    amount INT NOT NULL,
    occurred_at DATETIME(6) NOT NULL,
    idem_key VARCHAR(255) NOT NULL,
    broad_no BIGINT NULL,
    meta JSON NULL,
    reason TEXT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY point_events_idem_uniq (bid, idem_key),
    INDEX pe_user_time_idx (bid, user_id, occurred_at),
    INDEX pe_kind_time_idx (bid, kind, occurred_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS presence (
    bid VARCHAR(255) NOT NULL,
    broad_no BIGINT NOT NULL,
    user_id VARCHAR(255) NOT NULL,
    started_at DATETIME(6) NOT NULL,
    last_seen DATETIME(6) NOT NULL,
    ticks_paid INT NOT NULL DEFAULT 0,
    PRIMARY KEY (bid, broad_no, user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS scoring_rules (
    bid VARCHAR(255) NOT NULL,
    `key` VARCHAR(128) NOT NULL,
    value JSON NOT NULL,
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (bid, `key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS accounts (
    id BIGINT NOT NULL AUTO_INCREMENT,
    email VARCHAR(320) NULL,
    password_hash TEXT NULL,
    is_admin BOOLEAN NOT NULL DEFAULT FALSE,
    disabled BOOLEAN NOT NULL DEFAULT FALSE,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    last_login_at DATETIME(6) NULL,
    PRIMARY KEY (id),
    UNIQUE KEY accounts_email_uniq (email)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS sessions (
    token_hash CHAR(64) NOT NULL,
    account_id BIGINT NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    expires_at DATETIME(6) NOT NULL,
    last_used_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    user_agent VARCHAR(200) NULL,
    PRIMARY KEY (token_hash),
    INDEX sessions_account_idx (account_id),
    INDEX sessions_expires_idx (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS streamer_claims (
    id BIGINT NOT NULL AUTO_INCREMENT,
    account_id BIGINT NOT NULL,
    bid VARCHAR(255) NOT NULL,
    soop_nick VARCHAR(255) NULL,
    status VARCHAR(16) NOT NULL DEFAULT 'pending',
    note TEXT NULL,
    requested_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    decided_at DATETIME(6) NULL,
    decided_by BIGINT NULL,
    approved_bid VARCHAR(255) AS (CASE WHEN status = 'approved' THEN LOWER(bid) ELSE NULL END) STORED,
    PRIMARY KEY (id),
    UNIQUE KEY streamer_claims_account_bid_uniq (account_id, bid),
    UNIQUE KEY streamer_claims_approved_bid_uniq (approved_bid),
    INDEX streamer_claims_status_idx (status, requested_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS oauth_identities (
    provider VARCHAR(32) NOT NULL,
    provider_user_id VARCHAR(255) NOT NULL,
    account_id BIGINT NOT NULL,
    email VARCHAR(320) NULL,
    email_verified BOOLEAN NOT NULL DEFAULT FALSE,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    last_login_at DATETIME(6) NULL,
    PRIMARY KEY (provider, provider_user_id),
    UNIQUE KEY oauth_identities_account_provider_uniq (account_id, provider),
    INDEX oauth_identities_account_idx (account_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS oauth_states (
    state CHAR(48) NOT NULL,
    provider VARCHAR(32) NOT NULL,
    redirect_uri TEXT NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    expires_at DATETIME(6) NOT NULL,
    PRIMARY KEY (state),
    INDEX oauth_states_expires_idx (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS shop_pages (
    id BIGINT NOT NULL AUTO_INCREMENT,
    bid VARCHAR(255) NOT NULL,
    title VARCHAR(255) NOT NULL,
    description TEXT NULL,
    status VARCHAR(16) NOT NULL DEFAULT 'draft',
    post_url TEXT NULL,
    post_title_no BIGINT NULL,
    opened_at DATETIME(6) NULL,
    closed_at DATETIME(6) NULL,
    settled_at DATETIME(6) NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    open_bid VARCHAR(255) AS (CASE WHEN status = 'open' THEN bid ELSE NULL END) STORED,
    PRIMARY KEY (id),
    UNIQUE KEY shop_pages_one_open_per_bid (open_bid),
    UNIQUE KEY shop_pages_post_uniq (post_title_no),
    INDEX shop_pages_bid_idx (bid, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS shop_items (
    id BIGINT NOT NULL AUTO_INCREMENT,
    page_id BIGINT NOT NULL,
    name VARCHAR(255) NOT NULL,
    price INT NOT NULL,
    stock INT NULL,
    per_user_limit INT NULL,
    sort_order INT NOT NULL DEFAULT 0,
    image_id BIGINT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY shop_items_page_name_uniq (page_id, name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS shop_images (
    id BIGINT NOT NULL AUTO_INCREMENT,
    bid VARCHAR(255) NOT NULL,
    mime VARCHAR(64) NOT NULL,
    bytes LONGBLOB NOT NULL,
    byte_size INT NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    INDEX shop_images_bid_idx (bid, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS purchase_orders (
    id BIGINT NOT NULL AUTO_INCREMENT,
    page_id BIGINT NOT NULL,
    bid VARCHAR(255) NOT NULL,
    user_id VARCHAR(255) NOT NULL,
    nickname VARCHAR(255) NULL,
    comment_no BIGINT NOT NULL,
    raw_comment TEXT NOT NULL,
    total_price INT NOT NULL DEFAULT 0,
    status VARCHAR(16) NOT NULL,
    reject_reason TEXT NULL,
    commented_at DATETIME(6) NULL,
    settled_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY purchase_orders_comment_uniq (page_id, comment_no),
    INDEX purchase_orders_page_idx (page_id, settled_at),
    INDEX purchase_orders_user_idx (bid, user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS purchase_lines (
    id BIGINT NOT NULL AUTO_INCREMENT,
    order_id BIGINT NOT NULL,
    item_id BIGINT NULL,
    item_name VARCHAR(255) NOT NULL,
    quantity INT NOT NULL,
    unit_price INT NOT NULL,
    PRIMARY KEY (id),
    INDEX purchase_lines_order_idx (order_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
