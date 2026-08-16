CREATE TABLE oauth_identities (
    provider VARCHAR(32) NOT NULL,
    provider_user_id VARCHAR(255) NOT NULL,
    account_id BIGINT NOT NULL,
    email VARCHAR(320) NULL,
    email_verified BOOLEAN NOT NULL DEFAULT FALSE,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    last_login_at DATETIME(6) NULL,
    PRIMARY KEY (provider, provider_user_id),
    CONSTRAINT oauth_identities_account_fk FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE,
    UNIQUE KEY oauth_identities_account_provider_uniq (account_id, provider),
    INDEX oauth_identities_account_idx (account_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE oauth_states (
    state CHAR(48) PRIMARY KEY,
    provider VARCHAR(32) NOT NULL,
    redirect_uri TEXT NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    expires_at DATETIME(6) NOT NULL,
    INDEX oauth_states_expires_idx (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
