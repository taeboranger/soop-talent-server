-- 과거 초기화 과정에서 schema_migrations 이력만 남고 달란트 시장 테이블이
-- 빠진 설치를 복구한다. 모든 문장은 IF NOT EXISTS여서 정상 설치와 Pod 재시작에는
-- 영향을 주지 않으며, 기존 시장 데이터도 변경하지 않는다.

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
