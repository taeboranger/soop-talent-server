-- 스트리머가 여러 달란트 시장을 동시에 공유할 수 있게 한다.
-- open_bid 생성 열은 과거 설치와의 호환을 위해 남기되, "열린 시장은 하나"를
-- 강제하던 유니크 인덱스만 제거한다. 이 마이그레이션은 한 번만 적용된다.
ALTER TABLE shop_pages DROP INDEX shop_pages_one_open_per_bid;
