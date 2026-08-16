-- OAuth 전용 계정 / 세션 / 스트리머 연결 신청
CREATE TABLE accounts (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    email VARCHAR(320) NULL,
    password_hash TEXT NULL,
    is_admin BOOLEAN NOT NULL DEFAULT FALSE,
    disabled BOOLEAN NOT NULL DEFAULT FALSE,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    last_login_at DATETIME(6) NULL,
    UNIQUE KEY accounts_email_uniq (email)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE sessions (
    token_hash CHAR(64) PRIMARY KEY,
    account_id BIGINT NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    expires_at DATETIME(6) NOT NULL,
    last_used_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    user_agent VARCHAR(200) NULL,
    CONSTRAINT sessions_account_fk FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE,
    INDEX sessions_account_idx (account_id),
    INDEX sessions_expires_idx (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE streamer_claims (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    account_id BIGINT NOT NULL,
    bid VARCHAR(255) NOT NULL,
    soop_nick VARCHAR(255) NULL,
    status ENUM('pending','approved','rejected') NOT NULL DEFAULT 'pending',
    note TEXT NULL,
    requested_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    decided_at DATETIME(6) NULL,
    decided_by BIGINT NULL,
    approved_bid VARCHAR(255) AS (CASE WHEN status = 'approved' THEN LOWER(bid) ELSE NULL END) STORED,
    CONSTRAINT streamer_claims_account_fk FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE,
    CONSTRAINT streamer_claims_decider_fk FOREIGN KEY (decided_by) REFERENCES accounts(id),
    UNIQUE KEY streamer_claims_account_bid_uniq (account_id, bid),
    UNIQUE KEY streamer_claims_approved_bid_uniq (approved_bid),
    INDEX streamer_claims_status_idx (status, requested_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
