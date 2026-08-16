-- 기존 1회 코드 테이블은 배포된 스키마와 호환을 위해 남기고, PKCE와 클라이언트
-- state를 포함한 v2 테이블을 별도로 만든다. ALTER 없이 재실행 가능하다.
CREATE TABLE IF NOT EXISTS extension_login_codes_v2 (
    code_hash CHAR(64) NOT NULL,
    account_id BIGINT NOT NULL,
    redirect_uri VARCHAR(1024) NOT NULL,
    code_challenge VARCHAR(128) NOT NULL,
    client_state VARCHAR(128) NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (code_hash),
    INDEX extension_login_codes_v2_expires_idx (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- OAuth를 확장에서 시작한 경우, 제공자 왕복 전에 PKCE 정보를 보관한다.
CREATE TABLE IF NOT EXISTS extension_oauth_requests (
    oauth_state_hash CHAR(64) NOT NULL,
    code_challenge VARCHAR(128) NOT NULL,
    client_state VARCHAR(128) NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (oauth_state_hash),
    INDEX extension_oauth_requests_expires_idx (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
