-- 경고: soop-talent 데이터베이스의 SoopTalent 데이터 전체를 지운다.
-- MySQL 관리자 또는 이 DB에 DROP 권한이 있는 계정으로 실행한다.

SET FOREIGN_KEY_CHECKS = 0;
DROP TABLE IF EXISTS purchase_lines;
DROP TABLE IF EXISTS purchase_orders;
DROP TABLE IF EXISTS shop_images;
DROP TABLE IF EXISTS shop_items;
DROP TABLE IF EXISTS shop_pages;
DROP TABLE IF EXISTS oauth_states;
DROP TABLE IF EXISTS oauth_identities;
DROP TABLE IF EXISTS streamer_claims;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS accounts;
DROP TABLE IF EXISTS scoring_rules;
DROP TABLE IF EXISTS presence;
DROP TABLE IF EXISTS point_events;
DROP TABLE IF EXISTS raw_chat_events;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS broadcasts;
DROP TABLE IF EXISTS streamers;
DROP TABLE IF EXISTS schema_migrations;
SET FOREIGN_KEY_CHECKS = 1;
