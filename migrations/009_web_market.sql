-- 댓글 정산형 시장을 공유 링크·즉시 결제형 시장으로 전환한다.
-- 이 파일은 schema_migrations에 한 번만 기록된다. 기존 페이지·주문 데이터는
-- 보존하며, 새 주문만 comment_no 없이 request_key로 멱등성을 보장한다.

ALTER TABLE shop_pages
    ADD COLUMN public_token CHAR(43) NULL AFTER status,
    ADD UNIQUE KEY shop_pages_public_token_uniq (public_token);

-- 예전 댓글형으로 열어 둔 시장에는 공유 링크가 없다. 새 결제 흐름과 섞이지
-- 않도록 닫고, 보상·댓글·과거 주문 기록은 그대로 남긴다.
UPDATE shop_pages
   SET status = 'closed', closed_at = COALESCE(closed_at, NOW(6))
 WHERE status = 'open' AND public_token IS NULL;

ALTER TABLE purchase_orders
    MODIFY COLUMN comment_no BIGINT NULL,
    MODIFY COLUMN raw_comment TEXT NULL,
    MODIFY COLUMN settled_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    ADD COLUMN request_key VARCHAR(64) NULL AFTER raw_comment,
    ADD UNIQUE KEY purchase_orders_request_key_uniq (page_id, request_key);
