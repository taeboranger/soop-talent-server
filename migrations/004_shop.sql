-- 페어링 정리 + 점수 사유 + 달란트 시장(상품 페이지)
--
-- 달란트는 스트리머별로 따로 쌓인다. point_events는 이미 (bid, user_id) 스코프이므로
-- A 스트리머에게 모은 달란트로 B 스트리머의 상품을 살 수 없다.

-- ── 페어링 방식 폐기 ────────────────────────────────────────────────────
-- 계정 + 관리자 승인 방식으로 대체되었다 (002_accounts.sql).
DROP TABLE IF EXISTS streamer_identity;

-- ── 점수 사유 ───────────────────────────────────────────────────────────
-- 로그를 "[타임라인] 증감내역 / 증감사유" 형태로 보여주기 위한 자유 서술 필드.
-- 자동 적립은 kind로 사유를 유도할 수 있으므로 비워두고,
-- 수동 지급·구매 차감처럼 설명이 필요한 경우에만 채운다.
ALTER TABLE point_events ADD COLUMN IF NOT EXISTS reason TEXT;

-- ── 상품 페이지 ─────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS shop_pages (
    id            BIGSERIAL   PRIMARY KEY,
    bid           TEXT        NOT NULL REFERENCES streamers(bid) ON DELETE CASCADE,
    title         TEXT        NOT NULL,
    description   TEXT,
    status        TEXT        NOT NULL DEFAULT 'draft',  -- draft|open|closed
    post_url      TEXT,          -- "[달란트 시장]"으로 시작하는 게시글 주소
    post_title_no BIGINT,        -- post_url에서 뽑은 글 번호. 댓글 조회에 쓴다
    opened_at     TIMESTAMPTZ,
    closed_at     TIMESTAMPTZ,
    settled_at    TIMESTAMPTZ,   -- 댓글을 읽어 구매를 확정한 시각
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT shop_pages_status_chk CHECK (status IN ('draft', 'open', 'closed'))
);

-- 스트리머당 열려 있는 페이지는 하나뿐이다.
CREATE UNIQUE INDEX IF NOT EXISTS shop_pages_one_open_per_bid
    ON shop_pages (bid) WHERE status = 'open';

-- 한 게시글이 두 페이지에 연결되면 어느 상품 목록을 쓸지 모호해진다.
CREATE UNIQUE INDEX IF NOT EXISTS shop_pages_post_uniq
    ON shop_pages (post_title_no) WHERE post_title_no IS NOT NULL;

CREATE INDEX IF NOT EXISTS shop_pages_bid_idx ON shop_pages (bid, created_at DESC);

-- ── 상품 ────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS shop_items (
    id         BIGSERIAL PRIMARY KEY,
    page_id    BIGINT    NOT NULL REFERENCES shop_pages(id) ON DELETE CASCADE,
    name       TEXT      NOT NULL,
    price      INTEGER   NOT NULL CHECK (price >= 0),
    stock      INTEGER,                     -- NULL이면 수량 제한 없음
    sort_order INTEGER   NOT NULL DEFAULT 0
);

-- 댓글의 상품명을 이름으로 대조하므로 같은 페이지 안에서 이름이 겹치면 안 된다.
CREATE UNIQUE INDEX IF NOT EXISTS shop_items_page_name_uniq
    ON shop_items (page_id, lower(name));

-- ── 구매 신청 ───────────────────────────────────────────────────────────
-- 댓글 하나가 신청 하나다. comment_no가 멱등키이므로 정산을 여러 번 돌려도
-- 같은 댓글이 두 번 차감되지 않는다.
CREATE TABLE IF NOT EXISTS purchase_orders (
    id            BIGSERIAL   PRIMARY KEY,
    page_id       BIGINT      NOT NULL REFERENCES shop_pages(id) ON DELETE CASCADE,
    bid           TEXT        NOT NULL,
    user_id       TEXT        NOT NULL,
    nickname      TEXT,
    comment_no    BIGINT      NOT NULL,
    raw_comment   TEXT        NOT NULL,   -- 원문 보존. 분쟁 시 근거가 된다
    total_price   INTEGER     NOT NULL DEFAULT 0,
    status        TEXT        NOT NULL,   -- fulfilled|rejected
    reject_reason TEXT,
    commented_at  TIMESTAMPTZ,
    settled_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT purchase_orders_status_chk CHECK (status IN ('fulfilled', 'rejected')),
    CONSTRAINT purchase_orders_comment_uniq UNIQUE (page_id, comment_no)
);
CREATE INDEX IF NOT EXISTS purchase_orders_page_idx ON purchase_orders (page_id, settled_at);
CREATE INDEX IF NOT EXISTS purchase_orders_user_idx ON purchase_orders (bid, user_id);

CREATE TABLE IF NOT EXISTS purchase_lines (
    id         BIGSERIAL PRIMARY KEY,
    order_id   BIGINT    NOT NULL REFERENCES purchase_orders(id) ON DELETE CASCADE,
    item_id    BIGINT    REFERENCES shop_items(id) ON DELETE SET NULL,
    item_name  TEXT      NOT NULL,   -- 상품이 지워져도 내역이 남도록 이름을 복사해 둔다
    quantity   INTEGER   NOT NULL CHECK (quantity > 0),
    unit_price INTEGER   NOT NULL
);
CREATE INDEX IF NOT EXISTS purchase_lines_order_idx ON purchase_lines (order_id);
