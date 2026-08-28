-- 006이 이미 적용된 설치도 브라우저 User-Agent 값을 남기지 않도록 정리한다.
UPDATE sessions SET user_agent = NULL;
