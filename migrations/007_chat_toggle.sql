-- 채팅 연동 스위치.
--
-- 끄면 SOOP 채팅 소켓에 붙지 않는다 — 시청 체류·채팅·후원·구독 달란트가 멈춘다.
-- 랭킹 동기화와 게시판 수동 지급은 확장이 API로 올리는 것이므로 영향을 받지 않는다.
--
-- 기본값은 TRUE다. 기존 스트리머는 지금까지와 똑같이 동작한다.
ALTER TABLE streamers
    ADD COLUMN IF NOT EXISTS chat_enabled BOOLEAN NOT NULL DEFAULT TRUE;
