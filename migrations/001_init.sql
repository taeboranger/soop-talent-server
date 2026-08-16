-- SoopTalent 초기 스키마
-- 설계 근거는 DESIGN.md §7 참조.

-- ---------------------------------------------------------------------------
-- 스트리머 / 인증
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS streamers (
    bid         TEXT PRIMARY KEY,           -- SOOP 로그인 ID
    nickname    TEXT,
    enabled     BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS streamer_identity (
    bid           TEXT PRIMARY KEY REFERENCES streamers(bid) ON DELETE CASCADE,
    token_hash    TEXT,                     -- 페어링으로 발급된 장기 토큰 (SHA-256)
    paired_at     TIMESTAMPTZ,
    pairing_code  TEXT,                     -- 1회용, 사용 시 NULL
    code_expires  TIMESTAMPTZ
);

-- ---------------------------------------------------------------------------
-- 방송 / 시청자
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS broadcasts (
    broad_no    BIGINT PRIMARY KEY,
    bid         TEXT NOT NULL REFERENCES streamers(bid) ON DELETE CASCADE,
    title       TEXT,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS broadcasts_bid_started_idx ON broadcasts (bid, started_at DESC);

CREATE TABLE IF NOT EXISTS users (
    bid            TEXT NOT NULL REFERENCES streamers(bid) ON DELETE CASCADE,
    user_id        TEXT NOT NULL,           -- (2) 접미사 제거된 정규화 ID
    nickname       TEXT,
    fanclub_weeks  INTEGER NOT NULL DEFAULT -1,
    first_seen     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (bid, user_id)
);

-- ---------------------------------------------------------------------------
-- 원본 로그 — 미확인 svc 코드의 소급 처리를 위한 안전장치
-- 후원·구독 코드가 특정되면 이 테이블을 재처리해 점수를 소급 적립한다.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS raw_chat_events (
    id        BIGSERIAL   PRIMARY KEY,
    bid       TEXT        NOT NULL,
    broad_no  BIGINT,
    svc       TEXT        NOT NULL,
    parts     JSONB       NOT NULL,
    raw       BYTEA,
    recv_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS raw_svc_recv_idx  ON raw_chat_events (svc, recv_at DESC);
CREATE INDEX IF NOT EXISTS raw_bid_recv_idx  ON raw_chat_events (bid, recv_at DESC);

-- ---------------------------------------------------------------------------
-- 점수 — 이벤트 소싱. 잔액을 직접 고치지 않는다.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS point_events (
    id           BIGSERIAL   PRIMARY KEY,
    bid          TEXT        NOT NULL REFERENCES streamers(bid) ON DELETE CASCADE,
    user_id      TEXT        NOT NULL,
    kind         TEXT        NOT NULL,
    amount       INTEGER     NOT NULL,
    occurred_at  TIMESTAMPTZ NOT NULL,
    idem_key     TEXT        NOT NULL,
    broad_no     BIGINT,
    meta         JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT point_events_idem_uniq UNIQUE (bid, idem_key)
);
CREATE INDEX IF NOT EXISTS pe_user_time_idx ON point_events (bid, user_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS pe_kind_time_idx ON point_events (bid, kind, occurred_at DESC);

-- ---------------------------------------------------------------------------
-- 체류 세션 (기능 ②)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS presence (
    bid         TEXT        NOT NULL,
    broad_no    BIGINT      NOT NULL,
    user_id     TEXT        NOT NULL,
    started_at  TIMESTAMPTZ NOT NULL,
    last_seen   TIMESTAMPTZ NOT NULL,
    ticks_paid  INTEGER     NOT NULL DEFAULT 0,
    PRIMARY KEY (bid, broad_no, user_id)
);

-- ---------------------------------------------------------------------------
-- 점수 규칙 — 전 항목 기본값 존재. 미설정 시 코드의 기본값으로 동작.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS scoring_rules (
    bid         TEXT        NOT NULL REFERENCES streamers(bid) ON DELETE CASCADE,
    key         TEXT        NOT NULL,
    value       JSONB       NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (bid, key)
);

-- 잔액 조회용
CREATE OR REPLACE VIEW user_points AS
SELECT bid,
       user_id,
       SUM(amount)::BIGINT AS total,
       MAX(occurred_at)    AS last_earned
FROM point_events
GROUP BY bid, user_id;
