-- MySQL 8 초기 스키마. DB 자체는 utf8mb4로 생성한다.
CREATE TABLE streamers (
    bid VARCHAR(255) PRIMARY KEY,
    nickname VARCHAR(255),
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE broadcasts (
    broad_no BIGINT PRIMARY KEY,
    bid VARCHAR(255) NOT NULL,
    title TEXT,
    started_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    ended_at DATETIME(6) NULL,
    CONSTRAINT broadcasts_bid_fk FOREIGN KEY (bid) REFERENCES streamers(bid) ON DELETE CASCADE,
    INDEX broadcasts_bid_started_idx (bid, started_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE users (
    bid VARCHAR(255) NOT NULL,
    user_id VARCHAR(255) NOT NULL,
    nickname VARCHAR(255),
    fanclub_weeks INT NOT NULL DEFAULT -1,
    first_seen DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    last_seen DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (bid, user_id),
    CONSTRAINT users_bid_fk FOREIGN KEY (bid) REFERENCES streamers(bid) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE raw_chat_events (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    bid VARCHAR(255) NOT NULL,
    broad_no BIGINT NULL,
    svc VARCHAR(16) NOT NULL,
    parts JSON NOT NULL,
    raw LONGBLOB NULL,
    recv_at DATETIME(6) NOT NULL,
    INDEX raw_svc_recv_idx (svc, recv_at),
    INDEX raw_bid_recv_idx (bid, recv_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE point_events (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    bid VARCHAR(255) NOT NULL,
    user_id VARCHAR(255) NOT NULL,
    kind VARCHAR(64) NOT NULL,
    amount INT NOT NULL,
    occurred_at DATETIME(6) NOT NULL,
    idem_key VARCHAR(255) NOT NULL,
    broad_no BIGINT NULL,
    meta JSON NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    CONSTRAINT point_events_bid_fk FOREIGN KEY (bid) REFERENCES streamers(bid) ON DELETE CASCADE,
    CONSTRAINT point_events_idem_uniq UNIQUE (bid, idem_key),
    INDEX pe_user_time_idx (bid, user_id, occurred_at),
    INDEX pe_kind_time_idx (bid, kind, occurred_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE presence (
    bid VARCHAR(255) NOT NULL,
    broad_no BIGINT NOT NULL,
    user_id VARCHAR(255) NOT NULL,
    started_at DATETIME(6) NOT NULL,
    last_seen DATETIME(6) NOT NULL,
    ticks_paid INT NOT NULL DEFAULT 0,
    PRIMARY KEY (bid, broad_no, user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE scoring_rules (
    bid VARCHAR(255) NOT NULL,
    `key` VARCHAR(128) NOT NULL,
    value JSON NOT NULL,
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (bid, `key`),
    CONSTRAINT scoring_rules_bid_fk FOREIGN KEY (bid) REFERENCES streamers(bid) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
