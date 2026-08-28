-- 001_init.sql 이전 버전에서 이미 만들어진 streamers 테이블은
-- CREATE TABLE IF NOT EXISTS로 새 열을 받지 않는다. MySQL 8 호환을 위해
-- ADD COLUMN IF NOT EXISTS 대신 information_schema를 보고 조건부 DDL을 실행한다.
-- DDL 뒤에 프로세스가 재시작돼도 다음 실행에서는 SELECT 1만 수행하므로 안전하다.
SET @sooptalent_chat_enabled_exists := (
    SELECT COUNT(*)
      FROM information_schema.columns
     WHERE table_schema = DATABASE()
       AND table_name = 'streamers'
       AND column_name = 'chat_enabled'
);

SET @sooptalent_chat_enabled_sql := IF(
    @sooptalent_chat_enabled_exists = 0,
    'ALTER TABLE streamers ADD COLUMN chat_enabled BOOLEAN NOT NULL DEFAULT TRUE AFTER enabled',
    'SELECT 1'
);

PREPARE sooptalent_chat_enabled_stmt FROM @sooptalent_chat_enabled_sql;
EXECUTE sooptalent_chat_enabled_stmt;
DEALLOCATE PREPARE sooptalent_chat_enabled_stmt;
