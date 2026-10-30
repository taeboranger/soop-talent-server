-- 기존 잔액·시장·지급 내역은 변경하지 않는다. 행이 없으면 기본 달란트/미설정이다.
-- 별도 테이블을 사용해 기존 MySQL 테이블의 열·콜레이션 변경 없이 적용한다.
CREATE TABLE IF NOT EXISTS streamer_reward_units (
    bid VARCHAR(255) NOT NULL PRIMARY KEY,
    unit VARCHAR(64) NOT NULL,
    configured_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
