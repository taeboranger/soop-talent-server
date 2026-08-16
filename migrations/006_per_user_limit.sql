-- 보상별 인당 구매 제한
--
-- stock은 페이지 전체에 걸린 총 수량이고,
-- per_user_limit은 한 사람이 그 보상을 최대 몇 개까지 가져갈 수 있는지다.
-- 둘 다 NULL이면 제한 없음이며, 함께 걸면 더 빡빡한 쪽이 먼저 걸린다.

ALTER TABLE shop_items
    ADD COLUMN IF NOT EXISTS per_user_limit INTEGER;

ALTER TABLE shop_items DROP CONSTRAINT IF EXISTS shop_items_per_user_limit_chk;
ALTER TABLE shop_items ADD CONSTRAINT shop_items_per_user_limit_chk
    CHECK (per_user_limit IS NULL OR per_user_limit > 0);
