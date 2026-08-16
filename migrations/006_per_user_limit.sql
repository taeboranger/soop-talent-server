ALTER TABLE shop_items ADD COLUMN per_user_limit INT NULL;
ALTER TABLE shop_items ADD CONSTRAINT shop_items_per_user_limit_chk CHECK (per_user_limit IS NULL OR per_user_limit > 0);
