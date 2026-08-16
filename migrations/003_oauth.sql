-- 소셜 로그인 (Google / Naver / Kakao)
--
-- OAuth로만 가입한 계정은 비밀번호가 없고, 제공자가 이메일을 주지 않을 수도 있다
-- (카카오는 이메일 제공이 선택 동의). 그래서 두 컬럼의 NOT NULL을 푼다.
-- lower(email) 유니크 인덱스는 PostgreSQL에서 NULL을 중복으로 보지 않으므로 그대로 둔다.

ALTER TABLE accounts ALTER COLUMN password_hash DROP NOT NULL;
ALTER TABLE accounts ALTER COLUMN email         DROP NOT NULL;

-- 비밀번호도 이메일도 없는 계정은 로그인할 방법이 없다.
-- 최소한 하나는 있어야 한다는 것을 스키마로 강제한다.
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_login_method_chk;
ALTER TABLE accounts ADD CONSTRAINT accounts_login_method_chk
    CHECK (password_hash IS NOT NULL OR email IS NOT NULL);

CREATE TABLE IF NOT EXISTS oauth_identities (
    provider         TEXT   NOT NULL,          -- google | naver | kakao
    provider_user_id TEXT   NOT NULL,          -- 제공자가 주는 고유 식별자
    account_id       BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    email            TEXT,                     -- 제공자가 알려준 이메일 (참고용)
    email_verified   BOOLEAN NOT NULL DEFAULT FALSE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at    TIMESTAMPTZ,
    PRIMARY KEY (provider, provider_user_id)
);
CREATE INDEX IF NOT EXISTS oauth_identities_account_idx ON oauth_identities (account_id);

-- 한 계정에 같은 제공자를 두 번 연결할 수 없다.
CREATE UNIQUE INDEX IF NOT EXISTS oauth_identities_account_provider_uniq
    ON oauth_identities (account_id, provider);

-- CSRF 방지용 state. 짧은 수명이며 1회 사용 후 삭제된다.
CREATE TABLE IF NOT EXISTS oauth_states (
    state        TEXT        PRIMARY KEY,
    provider     TEXT        NOT NULL,
    redirect_uri TEXT        NOT NULL,   -- 확장으로 돌려보낼 주소
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS oauth_states_expires_idx ON oauth_states (expires_at);
