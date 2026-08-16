# SoopTalent 운영 배포 가이드

서비스 주소는 **https://soop-talent.cagea1.com** 입니다. 스트리머·시청자 확장 프로그램은
이 주소를 코드에 고정해 두었으므로, 설치자가 서버 주소를 입력할 필요가 없습니다.

배포 흐름은 다음과 같습니다.

```text
sooptalent-server main 푸시
  → GitHub Actions: 테스트 · Docker Hub 이미지 빌드
  → config 저장소의 SoopTalent 이미지 SHA 갱신
  → Argo CD: Vault 시크릿 동기화 → initContainer DB 마이그레이션 → 앱 기동
```

## 1. 저장소와 이미지 이름

다음 두 저장소가 필요합니다.

| 용도 | 로컬 경로 | GitHub에서 할 일 |
|---|---|---|
| 애플리케이션 | `SoopTalent/sooptalent-server` | `main` 브랜치에 푸시 |
| GitOps 설정 | `config` | `SoopTalent/` 폴더를 커밋·푸시 |

기본 Docker Hub 이미지 이름은 다음 하나로 맞춰져 있습니다.

```text
lightstone1/soop-talent-server
```

마이그레이션 실행 파일도 이 이미지 안에 함께 들어 있습니다. Deployment의 `initContainer`가
서버 컨테이너보다 먼저 `/app/sooptalent-migrate`를 실행하므로, 별도 migration 이미지는 없습니다.

다른 Docker Hub 계정을 쓸 경우 아래 두 곳의 `lightstone1`을 **같은 값으로** 바꾸세요.

- `sooptalent-server/.github/workflows/container-image.yml`
- `config/SoopTalent/overlays/prod/sooptalent/kustomization.yaml`

## 2. DNS와 TLS

1. Kubernetes Ingress 컨트롤러의 외부 IP 또는 호스트를 확인합니다.
2. DNS 관리 화면에서 `soop-talent.cagea1.com`의 `A` 레코드(또는 환경에 맞는 CNAME)를 Ingress로 연결합니다.
3. `config/SoopTalent`를 Argo CD가 동기화하면 cert-manager가 Let's Encrypt 인증서를 발급합니다.

인증서가 발급되려면 DNS가 먼저 연결되어 있어야 합니다. 인증서와 Ingress 이름은 모두
`sooptalent-tls`, `sooptalent-ingress`로 고정돼 있습니다.

## 3. Vault 시크릿

Vault KV v2의 `kv/soop-talent-server-prod-secret`에 아래 값을 만듭니다. 이 경로는
`vault-static-secret.yaml`의 `path`와 일치해야 합니다.

| 키 | 필수 | 설명 |
|---|---:|---|
| `DATABASE_URL` | 예 | 운영 MySQL 8 DSN |
| `ADMIN_EMAILS` | 예 | OAuth 로그인 뒤 관리자 권한을 줄 이메일. 쉼표 구분 |
| `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` | 선택 | Google 로그인 |
| `NAVER_CLIENT_ID` / `NAVER_CLIENT_SECRET` | 선택 | 네이버 로그인 |
| `KAKAO_CLIENT_ID` / `KAKAO_CLIENT_SECRET` | 선택 | 카카오 로그인 |
| `OAUTH_REDIRECT_ALLOWLIST` | 선택 | 확장 외 웹 리다이렉트 주소 허용 목록 |

`PUBLIC_URL`과 `LISTEN`은 민감 정보가 아니며 Deployment에 고정돼 있습니다. Vault에 넣지 마세요.

Vault UI에서 **JSON 편집**으로 아래를 붙여넣은 뒤, `<...>` 부분만 바꾸면 됩니다.

```json
{
  "DATABASE_URL": "<db-user>:<db-password>@tcp(<db-host>:3306)/<db-name>?parseTime=true&charset=utf8mb4&loc=Asia%2FSeoul",
  "ADMIN_EMAILS": "admin@cagea1.com",

  "GOOGLE_CLIENT_ID": "",
  "GOOGLE_CLIENT_SECRET": "",
  "NAVER_CLIENT_ID": "",
  "NAVER_CLIENT_SECRET": "",
  "KAKAO_CLIENT_ID": "",
  "KAKAO_CLIENT_SECRET": "",

  "OAUTH_REDIRECT_ALLOWLIST": ""
}
```

OAuth 로그인은 필수입니다. 사용할 제공자의 Client ID와 Secret을 실제 값으로 바꾼 뒤 저장하세요.
DB 비밀번호와 OAuth Secret도 실제 값으로 바꿉니다. `ADMIN_EMAILS`에 든 이메일이 OAuth에서 검증돼 로그인하면 자동으로
관리자 권한을 받습니다. Vault는 이 JSON을 Kubernetes의
`soop-talent-server-prod-secret` Secret으로 동기화합니다.

### 새 MySQL 데이터베이스로 시작

MySQL 전환은 **빈 MySQL 8 데이터베이스에서 새로 시작**하는 방식입니다. 기존 PostgreSQL 데이터는
읽거나 옮기지 않습니다. 기존 DB 주소를 `DATABASE_URL`에 넣지 말고, `utf8mb4` 문자셋으로 만든 새 DB를
지정하세요. 배포 시 initContainer가 필요한 테이블을 먼저 만듭니다.

## 4. 소셜 로그인 콜백 등록

사용할 제공자 콘솔마다 아래 콜백 주소를 등록합니다.

```text
https://soop-talent.cagea1.com/api/oauth/google/callback
https://soop-talent.cagea1.com/api/oauth/naver/callback
https://soop-talent.cagea1.com/api/oauth/kakao/callback
```

스트리머 확장은 `chrome.identity`의 `https://<확장-ID>.chromiumapp.org/` 주소로 로그인 완료를
받습니다. 서버는 이 주소 형식만 허용하므로 별도 allowlist 등록은 필요 없습니다.

## 5. GitHub Actions 시크릿

`sooptalent-server` GitHub 저장소의 **Settings → Secrets and variables → Actions**에 다음을 추가합니다.

| 이름 | 값 |
|---|---|
| `DOCKERHUB_USERNAME` | Docker Hub 사용자명 |
| `DOCKERHUB_TOKEN` | Docker Hub access token (Write 권한) |
| `CONFIG_REPOSITORY` | GitOps 저장소, 예: `cagea1/config` |
| `CONFIG_REPOSITORY_TOKEN` | GitOps 저장소에만 Contents Read/Write 권한을 준 fine-grained token |

`CONFIG_REPOSITORY_TOKEN`은 앱 저장소까지 권한을 줄 필요가 없습니다. 만료일을 설정하고,
GitOps 저장소의 Contents 권한만 Read and write로 제한하세요.

## 6. Argo CD Application 등록

이미 config 저장소를 감시하는 ApplicationSet이 있다면, 그 규칙에 `SoopTalent/overlays/prod`가
포함되는지만 확인하면 됩니다. 별도 Application을 만들 경우 아래 값을 사용합니다.

| 항목 | 값 |
|---|---|
| Application 이름 | `sooptalent` |
| Repo URL | config GitHub 저장소 URL |
| Path | `SoopTalent/overlays/prod` |
| Destination namespace | `sooptalent-prod` |
| Sync policy | Automated (prune + self-heal 권장) |

Argo CD CLI 예시입니다. `<CONFIG_REPO_URL>`만 실제 주소로 바꾸세요.

```bash
argocd app create sooptalent \
  --repo <CONFIG_REPO_URL> \
  --path SoopTalent/overlays/prod \
  --dest-server https://kubernetes.default.svc \
  --dest-namespace sooptalent-prod \
  --sync-policy automated \
  --auto-prune \
  --self-heal
```

첫 동기화 전에 Vault에 시크릿이 있어야 합니다. Deployment Pod 안에서는
`initContainer(migrate)`가 성공한 뒤에만 서버 컨테이너가 시작됩니다.

## 7. 첫 배포 순서

1. `config/SoopTalent` 변경을 config 저장소에 커밋·푸시합니다.
2. DNS 레코드를 만듭니다.
3. Vault에 `soop-talent-server-prod-secret`을 등록합니다.
4. Argo CD Application을 생성하고 첫 Sync를 완료합니다.
   - 첫 Sync는 `initial` 이미지 태그 때문에 실패할 수 있습니다. 정상입니다.
5. `sooptalent-server`의 `main` 브랜치에 푸시합니다.
6. GitHub Actions가 실제 SHA 앱 이미지를 올리고, GitOps의 태그를 자동 갱신합니다.
7. Argo CD가 재동기화되어 운영 이미지로 바뀝니다.

GitHub Actions가 config 저장소를 자동으로 갱신하므로, 이후 앱 배포 때 이미지 태그를 직접 편집하지 않아도 됩니다.

## 8. 확장 프로그램 재패키징

고정 주소 변경은 확장 프로그램 코드에 포함돼 있습니다. 다음 변경을 각각 Chrome Web Store에 새 버전으로 올리세요.

- `sooptalent-streamer`: `manifest.json`의 버전을 올린 뒤 ZIP 생성
- `sooptalent-viewer`: `manifest.json`의 버전을 올린 뒤 ZIP 생성

두 확장 모두 `https://soop-talent.cagea1.com/*`만 추가 host permission으로 요청합니다.
심사자용 안내에는 서버 주소 입력 단계 대신, 자동 연결되는 서비스 주소와 테스트 계정을 적으세요.

## 9. 배포 후 확인

다음 주소를 브라우저에서 확인합니다.

```text
https://soop-talent.cagea1.com/healthz      → ok
https://soop-talent.cagea1.com/             → 스트리머 대시보드
https://soop-talent.cagea1.com/admin        → 관리자 로그인 화면
```

그 다음 순서로 확인합니다.

1. 스트리머 확장에서 OAuth 로그인
2. SOOP 스트리머 계정으로 연결 신청
3. 관리자 화면에서 승인
4. 연결 상태에서 채팅 연동 켜기
5. 시청자 확장에서 시장 게시글의 구매 버튼 확인

## 10. 롤백

문제가 생기면 config 저장소의
`SoopTalent/overlays/prod/sooptalent/kustomization.yaml`에서 앱 이미지 태그를
직전 SHA로 되돌려 커밋합니다. Argo CD가 이전 버전으로 동기화합니다.

DB 마이그레이션은 되돌릴 수 없는 변경이 있을 수 있으므로, 운영 배포 전 MySQL 백업은 별도로 유지하세요.
