-- 보상 미리보기 이미지
--
-- 이미지는 상품(보상)과 수명이 다르다.
-- SetItems는 목록을 통째로 지우고 다시 넣으므로, 이미지를 상품 행에 담으면
-- 목록을 저장할 때마다 사라진다. 그래서 따로 보관하고 참조만 한다.

CREATE TABLE IF NOT EXISTS shop_images (
    id         BIGSERIAL   PRIMARY KEY,
    bid        TEXT        NOT NULL REFERENCES streamers(bid) ON DELETE CASCADE,
    mime       TEXT        NOT NULL,
    bytes      BYTEA       NOT NULL,
    byte_size  INTEGER     NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS shop_images_bid_idx ON shop_images (bid, created_at DESC);

-- 이미지를 지워도 보상 자체는 남아야 한다.
ALTER TABLE shop_items
    ADD COLUMN IF NOT EXISTS image_id BIGINT REFERENCES shop_images(id) ON DELETE SET NULL;
