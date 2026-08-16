SET @limit_column_exists := (SELECT COUNT(*) FROM information_schema.columns
  WHERE table_schema = DATABASE() AND table_name = 'shop_items' AND column_name = 'per_user_limit');
SET @limit_column_sql := IF(@limit_column_exists = 0,
  'ALTER TABLE shop_items ADD COLUMN per_user_limit INT NULL', 'DO 0');
PREPARE sooptalent_stmt FROM @limit_column_sql;
EXECUTE sooptalent_stmt;
DEALLOCATE PREPARE sooptalent_stmt;

SET @limit_check_exists := (SELECT COUNT(*) FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE() AND table_name = 'shop_items' AND constraint_name = 'shop_items_per_user_limit_chk');
SET @limit_check_sql := IF(@limit_check_exists = 0,
  'ALTER TABLE shop_items ADD CONSTRAINT shop_items_per_user_limit_chk CHECK (per_user_limit IS NULL OR per_user_limit > 0)', 'DO 0');
PREPARE sooptalent_stmt FROM @limit_check_sql;
EXECUTE sooptalent_stmt;
DEALLOCATE PREPARE sooptalent_stmt;
