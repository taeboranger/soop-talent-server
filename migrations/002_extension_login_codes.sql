-- 001_init.sql이 이미 적용된 운영 DB도 웹·확장 로그인 동기화를 안전하게
-- 받을 수 있도록 별도 테이블만 재실행 가능한 형태로 추가한다.
CREATE TABLE IF NOT EXISTS extension_login_codes (
    code_hash CHAR(64) NOT NULL,
    account_id BIGINT NOT NULL,
    redirect_uri VARCHAR(1024) NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (code_hash),
    INDEX extension_login_codes_expires_idx (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
