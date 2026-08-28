-- SOOP 로그인 계정은 이메일 대신 SOOP ID와 표시용 닉네임만 사용한다.
-- 이 파일은 schema_migrations로 한 번만 실행된다. ALTER TABLE은 MySQL에서
-- 자동 커밋되므로 중간 실패를 피하려고 단일 변경만 수행한다.

ALTER TABLE oauth_identities
    ADD COLUMN nickname VARCHAR(255) NULL AFTER provider_user_id;

-- 기존 설치에 남아 있을 수 있는 이메일·비밀번호 정보는 이 전환 시점에 비운다.
UPDATE accounts SET email = NULL, password_hash = NULL;
UPDATE oauth_identities SET email = NULL, email_verified = FALSE;
UPDATE sessions SET user_agent = NULL;
