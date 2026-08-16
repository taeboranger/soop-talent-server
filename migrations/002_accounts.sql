-- 멀티테넌트 전환: 계정 / 세션 / 스트리머 연결 신청
--
-- 신뢰 모델: 서버는 확장이 보낸 SOOP 아이디를 믿지 않는다.
-- 세션 토큰 → 계정 → 승인된 연결(streamer_claims)로 소유자를 유도한다.
-- 관리자가 승인한 연결만 유효하다.

CREATE TABLE IF NOT EXISTS accounts (
    id            BIGSERIAL   PRIMARY KEY,
    email         TEXT        NOT NULL,
    password_hash TEXT        NOT NULL,   -- argon2id, 파라미터가 문자열에 포함됨
    is_admin      BOOLEAN     NOT NULL DEFAULT FALSE,
    disabled      BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ
);

-- 대소문자를 구분하지 않는 유일성. 가입 시 소문자로 정규화해 저장한다.
CREATE UNIQUE INDEX IF NOT EXISTS accounts_email_uniq ON accounts (lower(email));

CREATE TABLE IF NOT EXISTS sessions (
    token_hash  TEXT        PRIMARY KEY,  -- SHA-256. 원문은 저장하지 않는다
    account_id  BIGINT      NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    user_agent  TEXT
);
CREATE INDEX IF NOT EXISTS sessions_account_idx ON sessions (account_id);
CREATE INDEX IF NOT EXISTS sessions_expires_idx ON sessions (expires_at);

-- 스트리머 연결 신청. 관리자가 승인해야 수집이 시작된다.
CREATE TABLE IF NOT EXISTS streamer_claims (
    id           BIGSERIAL   PRIMARY KEY,
    account_id   BIGINT      NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    bid          TEXT        NOT NULL,          -- 신청자가 주장하는 SOOP 아이디
    soop_nick    TEXT,                          -- 신청 시점의 SOOP 닉네임 (판단 참고용)
    status       TEXT        NOT NULL DEFAULT 'pending',  -- pending|approved|rejected
    note         TEXT,                          -- 관리자 메모 / 거절 사유
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at   TIMESTAMPTZ,
    decided_by   BIGINT      REFERENCES accounts(id),
    CONSTRAINT streamer_claims_status_chk
        CHECK (status IN ('pending', 'approved', 'rejected'))
);

-- 같은 계정이 같은 아이디를 중복 신청하지 못하게 한다.
CREATE UNIQUE INDEX IF NOT EXISTS streamer_claims_account_bid_uniq
    ON streamer_claims (account_id, lower(bid));

-- 한 SOOP 아이디는 한 계정에만 승인될 수 있다.
-- 승인 건에만 걸리는 부분 유니크 인덱스라, 거절된 신청은 다시 시도할 수 있다.
CREATE UNIQUE INDEX IF NOT EXISTS streamer_claims_approved_bid_uniq
    ON streamer_claims (lower(bid)) WHERE status = 'approved';

CREATE INDEX IF NOT EXISTS streamer_claims_status_idx
    ON streamer_claims (status, requested_at DESC);
