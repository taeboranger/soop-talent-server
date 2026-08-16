SET @chat_column_exists := (SELECT COUNT(*) FROM information_schema.columns
  WHERE table_schema = DATABASE() AND table_name = 'streamers' AND column_name = 'chat_enabled');
SET @chat_column_sql := IF(@chat_column_exists = 0,
  'ALTER TABLE streamers ADD COLUMN chat_enabled BOOLEAN NOT NULL DEFAULT TRUE', 'DO 0');
PREPARE sooptalent_stmt FROM @chat_column_sql;
EXECUTE sooptalent_stmt;
DEALLOCATE PREPARE sooptalent_stmt;
