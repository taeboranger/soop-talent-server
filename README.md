# SoopTalent 서버

SOOP 시청자의 참여 활동을 자동 집계해 **달란트**를 부여하는 수집 서버입니다.
방송 채팅에 직접 붙어 실시간으로 기록하고, 스트리머용 웹 대시보드를 제공합니다.

설계는 [DESIGN.md](DESIGN.md), 확장과의 규약은 [API.md](API.md)를 보세요.

```
cmd/sooptalent/    진입점
internal/          수집·점수 엔진·REST API·웹 대시보드(HTML 임베드)
migrations/        SQL 마이그레이션 (실행할 때마다 자동 적용)
tools/             채팅 프로토콜 탐침, 가이드 정적 사본 생성기
docs/              GitHub Pages용 가이드 사본 (생성물 — 직접 고치지 말 것)
```

## 함께 쓰는 저장소

| 저장소 | 역할 |
|---|---|
| **sooptalent-server** | 이 저장소. 규약의 정본 |
| sooptalent-streamer | 스트리머 확장 — 랭킹 동기화, 게시판 달란트 버튼, 쪽지 |
| sooptalent-viewer | 시청자 확장 — 달란트 시장 구매 신청 |

확장 쪽을 고칠 일이 생기면 **[API.md](API.md)의 "바꿀 때 순서"를 먼저 읽으세요.**
서버를 먼저 배포하고 확장을 나중에 올려야 구버전 확장 사용자가 깨지지 않습니다.

**달란트는 스트리머별로 따로 쌓입니다.** A 스트리머에게 모은 달란트로 B의 보상을 받을 수 없습니다.

| 화면 | 주소 | 용도 |
|---|---|---|
| 스트리머 대시보드 | `/` | 연결 상태·달란트 기준·내역·직접 지급·달란트 시장 |
| 운영자 관리 | `/admin` | 스트리머 연결 신청 승인 |
| 가이드 | `/guide`, `/guide/detail` | 스트리머용 안내 |
| 헬스체크 | `/healthz`, `/api/health` | 인증 없음. `/api/health`는 DB까지 확인 |

---

## 연결 상태

대시보드 **대시보드** 탭 맨 위와 헤더 배지에서 지금 상태를 볼 수 있습니다.
20초마다 갱신하며, 보이지 않는 탭에서는 두드리지 않습니다.

| 항목 | 보는 것 |
|---|---|
| 서버 | DB 왕복 시간. 1초를 넘으면 경고 |
| 로그인 세션 | 만료 시각과 남은 일수. 3일 이하면 경고 |
| 채팅 연동 | SOOP 채팅 소켓 상태 · 방송 번호 · 마지막 수신 · 누적 패킷 |
| 방송 | 지금 방송 중인지 |

**판정과 문구는 서버가 정합니다**(`GET /api/status`). 대시보드와 확장이
각자 판정하면 같은 상황을 다르게 부르게 되기 때문입니다. 자세한 규약은 [API.md](API.md)에 있습니다.

### 채팅 연동 끄기

연결 상태 카드에서 끌 수 있습니다. 끄면 채팅 소켓을 닫아
**시청·채팅·후원·구독 달란트가 멈춥니다.** 랭킹 동기화와 게시판 직접 지급은
확장이 API로 올리는 것이라 영향받지 않습니다.

설정은 `streamers.chat_enabled`에 남아 서버를 다시 켜도 유지됩니다.
이미 쌓인 달란트와 달란트 시장은 그대로입니다.

---

## 빠르게 실행하기

```powershell
.\run.ps1
```

DB 시작 → (필요하면) 빌드 → 서버 실행까지 한 번에 합니다.

```powershell
.\run.ps1 -Build                  # 코드를 고친 뒤 다시 빌드하고 실행
.\run.ps1 -Streamers "abc,def"    # 여러 스트리머 수집 (개발용)
.\run.ps1 -Stop                   # 서버와 DB 모두 정리
```

---

## 직접 실행하기

### 1. 준비물

| 항목 | 확인 |
|---|---|
| Go 1.26+ | `go version` |
| Docker | `docker --version` |

> **`go` 명령을 못 찾는다면** — Go는 `%USERPROFILE%\.local\go`에 설치되어 있고
> 사용자 PATH에 등록돼 있습니다. **이미 열려 있던 터미널에는 반영되지 않으니
> 새 터미널을 여세요.** 그래도 안 되면:
> ```powershell
> $env:Path = "$env:Path;$env:USERPROFILE\.local\go\bin"
> ```

### 2. DB 시작

```powershell
docker compose up -d db
```

MySQL 8이 **3307** 포트로 뜹니다 (로컬 MySQL과 충돌하지 않도록).

새 DB에 스키마가 정상 생성되는지 확인하려면 다음을 한 번 실행합니다.

```powershell
docker compose --profile tools run --rm migrate
```

### 처음부터 다시 만들기

개발 DB를 비우고 다시 시작할 때만 `RESET_MYSQL.sql`을 실행합니다. 이 파일은
SoopTalent 테이블과 마이그레이션 이력을 모두 지우므로, 운영 DB에서는 백업 없이 실행하지 마세요.

```powershell
mysql -h <mysql-host> -u <관리자계정> -p soop-talent < RESET_MYSQL.sql
```

그 뒤 새 서버 이미지를 배포하면 `001_init.sql` 하나가 전체 테이블을 생성합니다.

### 3. 빌드

```powershell
go build -o bin/sooptalent.exe ./cmd/sooptalent
```

### 4. 실행

```powershell
$env:DATABASE_URL   = "sooptalent:sooptalent@tcp(localhost:3307)/soop-talent?parseTime=true&charset=utf8mb4&loc=Asia%2FSeoul"
$env:ADMIN_EMAILS   = "admin@example.com"
$env:LISTEN         = ":8099"
$env:MIGRATIONS     = "./migrations"
.\bin\sooptalent.exe
```

마이그레이션은 실행할 때마다 자동 적용됩니다. 적용 완료한 파일은 DB의
`schema_migrations`에 기록하므로 다시 실행되지 않습니다.

---

## 환경변수

| 이름 | 필수 | 기본값 | 설명 |
|---|---|---|---|
| `DATABASE_URL` | ✅ | — | MySQL 8 DSN |
| `ADMIN_EMAILS` | ✅ | — | 검증된 OAuth 이메일이 일치할 때 관리자 권한을 줄 목록 (쉼표 구분) |
| `LISTEN` | | `:8080` | HTTP 바인드 주소 |
| `PUBLIC_URL` | | `http://localhost{LISTEN}` | 소셜 로그인 콜백 주소를 만드는 데 쓰는 공개 주소 |
| `MIGRATIONS` | | `./migrations` | 마이그레이션 디렉터리 |
| `LOG_LEVEL` | | `info` | `debug` / `info` / `warn` / `error` |
| `STREAMERS` | | — | 승인 절차 없이 강제 수집할 아이디. **개발용** |

### OAuth 로그인

하나 이상의 제공자를 설정해야 사용자가 로그인할 수 있습니다. 설정된 제공자만 확장에 버튼이 표시됩니다.

| 환경변수 | 제공자 콘솔에 등록할 Redirect URI |
|---|---|
| `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` | `{PUBLIC_URL}/api/oauth/google/callback` |
| `NAVER_CLIENT_ID` / `NAVER_CLIENT_SECRET` | `{PUBLIC_URL}/api/oauth/naver/callback` |
| `KAKAO_CLIENT_ID` / `KAKAO_CLIENT_SECRET` | `{PUBLIC_URL}/api/oauth/kakao/callback` |

`OAUTH_REDIRECT_ALLOWLIST`로 추가 리다이렉트 주소를 허용할 수 있습니다(쉼표 구분).
기본적으로 확장의 `https://<확장ID>.chromiumapp.org/` 주소만 허용됩니다 —
이 검증이 없으면 세션 토큰이 임의 사이트로 새는 오픈 리다이렉트가 됩니다.

발급처: [Google Cloud Console](https://console.cloud.google.com/apis/credentials) ·
[네이버 개발자센터](https://developers.naver.com/apps) ·
[카카오 개발자](https://developers.kakao.com/console/app)

---

## 달란트 시장

스트리머가 보상을 걸고, 시청자가 모은 달란트로 신청하는 기능입니다.

```
1. 대시보드 → 달란트 시장 → 페이지 만들기 → 보상·가격·수량 등록
2. 자기 방송국에 안내글 작성 (제목 자유)
3. 그 글 주소를 등록하고 페이지 열기
4. 시청자가 확장으로 보상을 골라 댓글 등록
5. 페이지 닫기 → 서버가 댓글을 읽어 달란트 차감
```

- 페이지는 여러 개 만들 수 있지만 **동시에 열 수 있는 건 하나**입니다.
- **한 번 닫으면 다시 열 수 없습니다.**
- 정산은 **댓글 시각 순 선착순**입니다. 잔액이나 수량이 모자라면 뒤에 온 신청이 거절됩니다.
- 게시글 소유자는 **서버가 SOOP에 직접 확인**합니다. 제목에는 아무 제약이 없습니다.

**차감의 근거는 확장이 아니라 SOOP에 실제로 달린 댓글**입니다.
손으로 `[구매신청] 보상이름 x2` 형식을 써도 동일하게 접수됩니다.

## 운영자: 신청 승인

`{서버주소}/admin`에서 관리자 계정으로 로그인하면 신청 목록이 보입니다.
각 신청에 방송국 링크가 있어 바로 확인할 수 있고, 승인·거절·철회가 가능합니다.

> **수동 승인이 유일한 신원 검증 장치입니다.** 신청자가 정말 그 채널의 주인인지는
> 운영자가 판단해야 합니다. 방송 중 채팅으로 확인 요청을 보내는 등
> 외부 수단으로 교차 확인하는 것을 권합니다.

---

## 개발

```powershell
go test ./...          # 실측 패킷 기반 파서 테스트 + 달란트 규칙 테스트
go vet ./...
```

정산 테스트는 실제 DB를 씁니다. 설정하지 않으면 건너뜁니다.

```powershell
$env:TEST_DATABASE_URL = "sooptalent:sooptalent@tcp(localhost:3307)/soop-talent?parseTime=true&charset=utf8mb4&loc=Asia%2FSeoul"
go test ./internal/shop/
```

### 채팅 프로토콜 탐침

새 `svc` 코드를 발굴할 때 씁니다. 서버 없이 단독으로 돕니다.

```powershell
cd tools
npm install            # 최초 1회 (ws 패키지)
node chat-probe.mjs 243000 ../probe-out
```

미확인 코드가 오면 콘솔에 즉시 출력되고 `probe-out/unknown-*.jsonl`에 쌓입니다.
**이 파일에는 실제 시청자 발언이 그대로 들어 있으니** 커밋하지 마세요 (`.gitignore`에 있습니다).

### 가이드 공개 사본 (GitHub Pages)

가이드는 서버 바이너리에 `//go:embed` 되어 `/guide`, `/guide/detail`로 서비스됩니다.
같은 내용을 서버 없이 볼 수 있도록 `docs/`에 정적 사본을 만듭니다.

```powershell
node tools/build-docs.mjs           # .\run.ps1 -Build 에도 포함되어 있습니다
node tools/build-docs.mjs ../어딘가  # 다른 곳으로 뽑기
```

`internal/api/*.html`이 **유일한 정본**입니다. `docs/`를 직접 고치면 다음 생성 때 덮어써집니다.
스크립트는 서버 경로 링크(`/`, `/guide/detail`)를 파일 이름으로 바꾸고,
바꾸지 못한 절대 경로가 남아 있으면 **빌드를 세웁니다** — 링크가 조용히 깨진 채 배포되는 걸 막기 위함입니다.

배포는 **Settings → Pages → Source: `main` 브랜치 / `/docs` 폴더**로 켜면 됩니다.

> **이 저장소를 private으로 두면 GitHub Pages를 쓸 수 없습니다** (유료 플랜 필요).
> 서버 코드는 감추고 가이드만 공개하려면 문서 전용 공개 저장소를 따로 만들고
> `node tools/build-docs.mjs ../sooptalent-docs` 로 뽑아 넣으세요.

---

## DB 들여다보기

```powershell
docker compose exec db mysql -u sooptalent -psooptalent soop-talent
```

```sql
SELECT kind, COUNT(*), SUM(amount) FROM point_events GROUP BY kind;
SELECT user_id, SUM(amount) AS total
FROM point_events GROUP BY user_id ORDER BY total DESC LIMIT 20;
SELECT svc, COUNT(*) FROM raw_chat_events GROUP BY svc ORDER BY 2 DESC;
```

---

## 알려진 제약

- **구독(`0093`) 해석은 단일 샘플 기반 추정**입니다. 개월 수가 1~600을 벗어나면 적립하지 않습니다.
  원본이 `raw_chat_events`에 보관되므로 해석이 확정되면 소급 재처리할 수 있습니다.
- **팬가입은 이벤트 코드가 없어** 팬클럽 주차의 `-1 → 양수` 전환으로 추론합니다.
  처음 보는 시청자는 기준선으로만 기록하고 적립하지 않습니다.
- SOOP의 비공개 API에 의존하므로 예고 없이 바뀔 수 있습니다.
