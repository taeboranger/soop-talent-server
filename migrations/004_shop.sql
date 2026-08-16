ALTER TABLE point_events ADD COLUMN reason TEXT NULL;

CREATE TABLE shop_pages (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    bid VARCHAR(255) NOT NULL,
    title VARCHAR(255) NOT NULL,
    description TEXT NULL,
    status ENUM('draft','open','closed') NOT NULL DEFAULT 'draft',
    post_url TEXT NULL,
    post_title_no BIGINT NULL,
    opened_at DATETIME(6) NULL,
    closed_at DATETIME(6) NULL,
    settled_at DATETIME(6) NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    open_bid VARCHAR(255) AS (CASE WHEN status = 'open' THEN bid ELSE NULL END) STORED,
    CONSTRAINT shop_pages_bid_fk FOREIGN KEY (bid) REFERENCES streamers(bid) ON DELETE CASCADE,
    UNIQUE KEY shop_pages_one_open_per_bid (open_bid),
    UNIQUE KEY shop_pages_post_uniq (post_title_no),
    INDEX shop_pages_bid_idx (bid, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE shop_items (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    page_id BIGINT NOT NULL,
    name VARCHAR(255) NOT NULL,
    price INT NOT NULL,
    stock INT NULL,
    sort_order INT NOT NULL DEFAULT 0,
    CONSTRAINT shop_items_page_fk FOREIGN KEY (page_id) REFERENCES shop_pages(id) ON DELETE CASCADE,
    CONSTRAINT shop_items_price_chk CHECK (price >= 0),
    UNIQUE KEY shop_items_page_name_uniq (page_id, name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE purchase_orders (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    page_id BIGINT NOT NULL,
    bid VARCHAR(255) NOT NULL,
    user_id VARCHAR(255) NOT NULL,
    nickname VARCHAR(255) NULL,
    comment_no BIGINT NOT NULL,
    raw_comment TEXT NOT NULL,
    total_price INT NOT NULL DEFAULT 0,
    status ENUM('fulfilled','rejected') NOT NULL,
    reject_reason TEXT NULL,
    commented_at DATETIME(6) NULL,
    settled_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    CONSTRAINT purchase_orders_page_fk FOREIGN KEY (page_id) REFERENCES shop_pages(id) ON DELETE CASCADE,
    UNIQUE KEY purchase_orders_comment_uniq (page_id, comment_no),
    INDEX purchase_orders_page_idx (page_id, settled_at),
    INDEX purchase_orders_user_idx (bid, user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE purchase_lines (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    order_id BIGINT NOT NULL,
    item_id BIGINT NULL,
    item_name VARCHAR(255) NOT NULL,
    quantity INT NOT NULL,
    unit_price INT NOT NULL,
    CONSTRAINT purchase_lines_order_fk FOREIGN KEY (order_id) REFERENCES purchase_orders(id) ON DELETE CASCADE,
    CONSTRAINT purchase_lines_item_fk FOREIGN KEY (item_id) REFERENCES shop_items(id) ON DELETE SET NULL,
    CONSTRAINT purchase_lines_quantity_chk CHECK (quantity > 0),
    INDEX purchase_lines_order_idx (order_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
