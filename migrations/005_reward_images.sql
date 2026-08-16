CREATE TABLE IF NOT EXISTS shop_images (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    bid VARCHAR(255) NOT NULL,
    mime VARCHAR(64) NOT NULL,
    bytes LONGBLOB NOT NULL,
    byte_size INT NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    CONSTRAINT shop_images_bid_fk FOREIGN KEY (bid) REFERENCES streamers(bid) ON DELETE CASCADE,
    INDEX shop_images_bid_idx (bid, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
ALTER TABLE shop_items ADD COLUMN IF NOT EXISTS image_id BIGINT NULL;
ALTER TABLE shop_items ADD CONSTRAINT shop_items_image_fk FOREIGN KEY (image_id) REFERENCES shop_images(id) ON DELETE SET NULL;
