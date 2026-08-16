# 확장 ↔ 서버 규약

저장소가 셋으로 나뉘어 있으므로, 이 문서가 **세 저장소를 묶는 유일한 계약**입니다.
여기 적힌 것을 바꾸면 확장 저장소도 함께 고쳐 배포해야 합니다.

| 저장소 | 역할 |
|---|---|
| `sooptalent-server` | 이 저장소. 규약의 정본 |
| `sooptalent-streamer` | 스트리머 확장 — 랭킹 동기화, 게시판 달란트 버튼, 쪽지 |
| `sooptalent-viewer` | 시청자 확장 — 달란트 시장 구매 신청 |

확장은 브라우저에서 도는 코드라 **사용자가 언제든 고칠 수 있습니다.**
그래서 확장이 보낸 값은 신뢰 대상이 아닙니다. 아래 표의 "신뢰" 열을 보세요.

---

## 인증 모델

세션 토큰 → 계정 → 관리자가 승인한 `streamer_claims` → **bid**.

**bid는 절대 요청 본문에서 읽지 않습니다.** 서버가 세션에서 유도합니다.
확장이 `{"bid":"남의아이디"}`를 보내도 자기 계정에 승인된 bid로만 처리됩니다.

미들웨어는 세 단계입니다.

| 단계 | 요구 조건 | 실패 시 |
|---|---|---|
| `h` | 없음 (공개) | — |
| `session` | 유효한 세션 토큰 | 401 |
| `approved` | 세션 + 승인된 bid | 403 |
| `admin` | 세션 + 관리자 계정 | 403 |

---

## 스트리머 확장이 쓰는 엔드포인트

| 메서드 | 경로 | 단계 | 신뢰 |
|---|---|---|---|
| GET | `/api/oauth/{provider}/start` | 공개 | OAuth 로그인 시작 |
| GET | `/api/oauth/{provider}/callback` | 공개 | OAuth 로그인 완료 |
| GET | `/api/oauth/providers` | 공개 | — |
| GET | `/api/oauth/{provider}/start` | 공개 | `redirect_uri`는 `*.chromiumapp.org` 또는 허용 목록만 |
| POST | `/api/logout` | session | — |
| GET | `/api/me` | session | 승인된 bid·신청 상태를 **서버가** 알려줌 |
| POST | `/api/claim` | session | 신청할 SOOP 아이디 (승인 전까지 아무 권한 없음) |
| GET | `/api/rules` · PUT `/api/rules` · POST `/api/rules/reset` | approved | 달란트 지급 기준 |
| POST | `/api/rank/ingest` | approved | **랭킹 행만.** 대상 bid는 세션에서 결정 |
| POST | `/api/award` | approved | `userId`, `kind`, `refId`(멱등키), `amount` |
| GET | `/api/award/status?refIds=a,b,c` | approved | 이미 준 refId 조회 — 버튼 상태 표시용 |
| GET | `/api/summary` | approved | 대시보드 요약 |
| GET | `/api/status` | approved | 연결 상태 — 서버·세션·채팅 연동·방송 |
| PUT | `/api/collector/chat` | approved | `{"enabled":true\|false}` — 채팅 연동 스위치 |

### 연결 상태 (`GET /api/status`)

**문구와 색은 서버가 정합니다.** 대시보드와 확장이 각자 판정하면
같은 상황을 서로 다르게 부르게 됩니다. 클라이언트는 `tone`/`label`을 그대로 그리세요.

```json
{
  "bid": "ecvhao",
  "serverTime": "2026-08-12T01:30:00+09:00",
  "database": { "tone": "ok", "label": "정상", "latencyMs": 0.5 },
  "session":  { "tone": "ok", "label": "로그인 중",
                "email": "…", "expiresAt": "…", "daysLeft": 13 },
  "chat": {
    "enabled": true, "running": true, "phase": "connected",
    "tone": "ok", "label": "수집 중", "detail": "채팅을 받고 있습니다.",
    "broadNo": "296281607", "since": "…", "connectedAt": "…",
    "lastPacketAt": "…", "packets": 6059, "lastError": "", "retries": 0
  },
  "live": true,
  "overall": "ok"
}
```

`tone`은 `ok` · `warn` · `bad` · `off` 네 가지입니다.
**`off`는 고장이 아니라 사람이 꺼 둔 것**이므로 빨강으로 칠하지 마세요.
`overall`은 넷 중 가장 나쁜 값이며, 배지 하나만 그릴 때 씁니다.

`chat.phase`는 `connecting` · `connected` · `offline`(방송 꺼짐) ·
`retrying` · `stopped` · `off` · `unknown` 입니다.
`lastError`는 **다시 붙은 뒤에도 남습니다** — "아까 왜 끊겼지"에 답하기 위한 것입니다.

값이 없는 시각 필드(`connectedAt` 등)는 응답에서 **아예 빠집니다.**
Go의 제로값(`0001-01-01`)이 화면에 새어 나가지 않게 하기 위해서입니다.

### 채팅 연동 스위치 (`PUT /api/collector/chat`)

끄면 SOOP 채팅 소켓을 닫습니다. **시청 체류·채팅·후원·구독 달란트가 멈춥니다.**
랭킹 동기화와 게시판 직접 지급은 확장이 API로 올리는 것이라 영향받지 않습니다.

설정은 `streamers.chat_enabled`에 남아 **서버를 다시 켜도 유지됩니다.**
이미 쌓인 달란트는 건드리지 않습니다. 응답은 `/api/status`의 `chat` 객체와 같은 모양입니다.

### 랭킹 업로드에서 주의할 점

확장은 최근 **7일치**를 매번 함께 올립니다. 서버 멱등키가
`rank:{kind}:{date}:{user}` 라서 같은 날을 여러 번 올려도 중복 적립되지 않습니다.
**이 키 형식을 바꾸면 과거 적립이 다시 나갑니다.**

`kind`는 `watch`(참여시간) / `chat`(채팅입력) 두 가지입니다.

---

## 시청자 확장이 쓰는 엔드포인트

시청자 확장은 **로그인하지 않습니다.** 아래 것만 부릅니다.

| 메서드 | 경로 | 응답 |
|---|---|---|
| GET | `/api/viewer/markets?bid=…` | `{"bid":"…","titleNos":[203551799]}` — 지금 열려 있는 시장 글 번호. 60초 캐시 |
| GET | `/api/viewer/shop?titleNo=…&userId=…` | 보상 목록 + 해당 아이디의 잔액. 열린 페이지가 없으면 404 |
| GET | `/api/shop/images/{id}` | 보상 미리보기 이미지 |
| GET | `/api/health` | `{"ok":true,"time":"…","database":{…}}` — 주소가 맞는지 확인용 |

`/api/health`는 누구나 부를 수 있으므로 **스트리머 정보를 담지 않습니다.**
예전 버전 서버에는 없으니 404면 `/healthz`로 물러서세요 (시청자 확장이 그렇게 합니다).

`userId`는 확장이 보낸 값이라 **표시용일 뿐입니다.**
실제 차감은 스트리머가 페이지를 닫을 때 서버가 SOOP 댓글을 직접 읽어 계산합니다.
확장이 잔액을 조작해도 정산 결과는 달라지지 않습니다.

### 구매 신청 댓글 형식

```
[구매신청] 치킨 기프티콘 x2, 손편지
```

확장은 이 문구를 **댓글 입력창에 채워 줄 뿐**이고, 등록은 사람이 직접 누릅니다.
손으로 같은 형식을 써도 동일하게 접수됩니다 — 차감의 근거는 확장이 아니라
SOOP에 실제로 달린 댓글입니다.

수량 표기는 `x2`, `X 2`, `×2` 를 인식합니다.
보상 이름에 쉼표·줄바꿈·수량 표기를 넣으면 등록 단계에서 거부됩니다.

---

## 확장이 직접 부르는 SOOP API

서버를 거치지 않고 브라우저의 SOOP 세션 쿠키로 부릅니다.
**비공개 API라 예고 없이 바뀔 수 있습니다.** 깨지면 여기부터 확인하세요.

| 도메인 | 용도 | 쓰는 곳 |
|---|---|---|
| `afevent2.sooplive.com/api/get_private_info.php` | 현재 로그인 계정 확인 | 양쪽 |
| `broadstatistic.sooplive.com/api/nonlive_statistic.php` | 일별 시청자 랭킹 (상위 100위) | 스트리머 |
| `api-channel.sooplive.com/v1.1/channel` | 게시글·댓글 조회 | 스트리머 |
| `note.sooplive.com/api/note_api.php` | 쪽지 발송 (수동 버튼) | 스트리머 |

랭킹 모듈명은 `RankViewTimeData`(참여시간), `RankChatData`(채팅입력) 입니다.

---

## 바꿀 때 순서

규약을 깨는 변경은 **서버를 먼저** 배포하고 확장을 나중에 올립니다.
확장은 사용자가 언제 갱신할지 알 수 없으므로, 서버가 구버전 확장을 한동안 받아 줘야 합니다.

1. 서버에 새 경로/필드를 **추가**하고 배포 (기존 것은 그대로 둠)
2. 확장을 새 것으로 올림
3. 충분히 지난 뒤 서버에서 옛 경로 제거

한 번에 갈아치우면, 갱신하지 않은 시청자의 화면에서 구매 버튼이 사라집니다.
