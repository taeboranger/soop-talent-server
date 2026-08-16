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
SET @image_column_exists := (SELECT COUNT(*) FROM information_schema.columns
  WHERE table_schema = DATABASE() AND table_name = 'shop_items' AND column_name = 'image_id');
SET @image_column_sql := IF(@image_column_exists = 0,
  'ALTER TABLE shop_items ADD COLUMN image_id BIGINT NULL', 'DO 0');
PREPARE sooptalent_stmt FROM @image_column_sql;
EXECUTE sooptalent_stmt;
DEALLOCATE PREPARE sooptalent_stmt;

SET @image_fk_exists := (SELECT COUNT(*) FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE() AND table_name = 'shop_items' AND constraint_name = 'shop_items_image_fk');
SET @image_fk_sql := IF(@image_fk_exists = 0,
  'ALTER TABLE shop_items ADD CONSTRAINT shop_items_image_fk FOREIGN KEY (image_id) REFERENCES shop_images(id) ON DELETE SET NULL', 'DO 0');
PREPARE sooptalent_stmt FROM @image_fk_sql;
EXECUTE sooptalent_stmt;
DEALLOCATE PREPARE sooptalent_stmt;
