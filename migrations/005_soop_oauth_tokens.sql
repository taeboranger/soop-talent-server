-- SOOP 공식 Chat SDK를 서버에서 지속적으로 사용하기 위한 OAuth 토큰 저장소.
-- 토큰 값은 애플리케이션에서 AES-256-GCM으로 암호화한 뒤 저장한다.
-- 기존 설치에도 안전하게 추가되며 외래 키에 의존하지 않는다.

CREATE TABLE IF NOT EXISTS soop_oauth_tokens (
    account_id BIGINT NOT NULL,
    bid VARCHAR(255) NOT NULL,
    access_token_enc VARBINARY(4096) NOT NULL,
    refresh_token_enc VARBINARY(4096) NULL,
    scope TEXT NULL,
    expires_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
        ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (account_id),
    UNIQUE KEY soop_oauth_tokens_bid_uniq (bid),
    INDEX soop_oauth_tokens_expiry_idx (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
