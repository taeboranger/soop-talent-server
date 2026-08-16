# SoopTalent 서버 실행 스크립트
#
#   .\run.ps1                     기본 설정으로 실행
#   .\run.ps1 -Streamers "abc"    다른 스트리머 수집
#   .\run.ps1 -Build              실행 전에 다시 빌드
#   .\run.ps1 -Stop               DB까지 정리하고 종료
#
# Go가 PATH에 없으면 설치 위치에서 자동으로 찾아 쓴다.

param(
    [string]$Streamers = "",          # 승인 절차 없이 강제 수집 (개발용)
    [string]$Listen = ":8099",
    [string]$AdminEmails = "admin@sooptalent.local",
    [switch]$Build,
    [switch]$Stop
)

$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

# ── Go 준비 ────────────────────────────────────────────────────────────────
$goExe = "go"
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    $local = "$env:USERPROFILE\.local\go\bin"
    if (Test-Path "$local\go.exe") {
        $env:Path = "$env:Path;$local"
        Write-Host "Go 경로 추가: $local" -ForegroundColor DarkGray
    } else {
        Write-Host "Go를 찾을 수 없습니다. 새 터미널을 열거나 Go를 설치하세요." -ForegroundColor Red
        exit 1
    }
}

# ── 종료 ───────────────────────────────────────────────────────────────────
if ($Stop) {
    Get-CimInstance Win32_Process -Filter "Name = 'sooptalent.exe'" |
        ForEach-Object { Stop-Process -Id $_.ProcessId -Force }
    docker compose down
    Write-Host "정리 완료." -ForegroundColor Green
    exit 0
}

# ── DB ─────────────────────────────────────────────────────────────────────
$dbID = docker compose ps -q db
if (-not $dbID) {
    Write-Host "MySQL 시작 중..." -ForegroundColor Cyan
    docker compose up -d db
    if (-not $?) { Write-Host "MySQL을 시작하지 못했습니다. Docker Desktop 상태를 확인하세요." -ForegroundColor Red; exit 1 }
    $dbID = docker compose ps -q db
}
if (-not $dbID) {
    Write-Host "MySQL 컨테이너를 찾지 못했습니다." -ForegroundColor Red
    exit 1
}
if ($dbID) {
    foreach ($i in 1..40) {
        Start-Sleep -Seconds 2
        $s = docker inspect --format='{{.State.Health.Status}}' $dbID 2>$null
        if ($s -eq 'healthy') { break }
        if ($i -eq 40) { Write-Host "DB가 준비되지 않았습니다." -ForegroundColor Red; exit 1 }
    }
}
Write-Host "DB 준비됨 (localhost:3307)" -ForegroundColor Green

# ── 빌드 ───────────────────────────────────────────────────────────────────
if ($Build -or -not (Test-Path "bin\sooptalent.exe")) {
    Write-Host "빌드 중..." -ForegroundColor Cyan
    go build -o bin/sooptalent.exe ./cmd/sooptalent
    if (-not $?) { Write-Host "빌드 실패." -ForegroundColor Red; exit 1 }
    Write-Host "빌드 완료" -ForegroundColor Green

    # 가이드가 바뀌었을 수 있으므로 GitHub Pages용 사본도 다시 뽑는다.
    # Node가 없으면 서버 빌드는 이미 끝났으니 알리기만 하고 넘어간다.
    if (Get-Command node -ErrorAction SilentlyContinue) {
        node tools/build-docs.mjs
        if (-not $?) { Write-Host "docs 생성 실패 — 가이드 링크 규칙을 확인하세요." -ForegroundColor Yellow }
    } else {
        Write-Host "Node가 없어 docs/를 건너뜁니다." -ForegroundColor DarkGray
    }
}

# ── 실행 ───────────────────────────────────────────────────────────────────
$env:DATABASE_URL   = "sooptalent:sooptalent@tcp(localhost:3307)/soop-talent?parseTime=true&charset=utf8mb4&loc=Asia%2FSeoul"
$env:LISTEN         = $Listen
$env:PUBLIC_URL     = "http://localhost$Listen"
$env:MIGRATIONS     = "./migrations"
$env:LOG_LEVEL      = "info"
$env:ADMIN_EMAILS   = $AdminEmails
$env:STREAMERS      = $Streamers

Write-Host ""
Write-Host "서버 주소:   http://localhost$Listen" -ForegroundColor Cyan
Write-Host "관리자 화면: http://localhost$Listen/admin" -ForegroundColor Cyan
Write-Host "관리자 이메일: $AdminEmails" -ForegroundColor DarkGray
if ($Streamers) { Write-Host "강제 수집:   $Streamers (개발용)" -ForegroundColor Yellow }
Write-Host "중지하려면 Ctrl+C" -ForegroundColor DarkGray
Write-Host ""

& ".\bin\sooptalent.exe"
