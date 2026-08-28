# SoopTalent 서버 실행 스크립트
#
#   .\run.ps1                     기본 설정으로 실행
#   .\run.ps1 -Streamers "abc"    다른 스트리머 수집
#   .\run.ps1 -Build              실행 전에 다시 빌드
#   .\run.ps1 -Build -OfficialChat SOOP 공식 OAuth·Chat SDK까지 로컬 테스트
#   .\run.ps1 -Stop               DB까지 정리하고 종료
#
# Go가 PATH에 없으면 설치 위치에서 자동으로 찾아 쓴다.

param(
    [string]$Streamers = "",          # 승인 절차 없이 강제 수집 (개발용)
    [string]$Listen = ":8099",
    [string]$AdminSOOPIDs = "",
    [switch]$Build,
    [switch]$OfficialChat,
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
    Get-CimInstance Win32_Process -Filter "Name = 'node.exe'" |
        Where-Object { $_.CommandLine -like '*chat-bridge*bridge.mjs*' } |
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
}

# ── 실행 ───────────────────────────────────────────────────────────────────
$env:DATABASE_URL   = "sooptalent:sooptalent@tcp(localhost:3307)/soop-talent?parseTime=true&charset=utf8mb4&loc=Asia%2FSeoul"
$env:LISTEN         = $Listen
$env:PUBLIC_URL     = "http://localhost$Listen"
$env:MIGRATIONS     = "./migrations"
$env:LOG_LEVEL      = "info"
$env:ADMIN_SOOP_IDS = $AdminSOOPIDs
$env:STREAMERS      = $Streamers

# ── 공식 SOOP OAuth·Chat SDK 로컬 시험 ───────────────────────────────────
$bridgeProcess = $null
if ($OfficialChat) {
    # 서버 전용 로컬 설정만 읽는다. 다른 프로젝트의 .env에 의존하지 않는다.
    $localEnv = Join-Path $PSScriptRoot ".env.local"
    if (Test-Path -LiteralPath $localEnv) {
        foreach ($line in Get-Content -LiteralPath $localEnv) {
            if ($line -match '^\s*([A-Za-z_][A-Za-z0-9_]*)=(.*)$') {
                $key, $value = $Matches[1], $Matches[2].Trim().Trim('"')
                if ($key -in @('SOOP_CLIENT_ID','SOOP_CLIENT_SECRET',
                    'SOOP_TOKEN_ENCRYPTION_KEY','CHAT_BRIDGE_SECRET','CHAT_BRIDGE_URL') -and
                    -not (Test-Path "Env:$key")) {
                    Set-Item -Path "Env:$key" -Value $value
                }
            }
        }
    }
    if (-not $env:SOOP_CLIENT_ID -or -not $env:SOOP_CLIENT_SECRET) {
        throw "SOOP_CLIENT_ID/SECRET가 필요합니다. sooptalent-server/.env.local 또는 현재 환경변수에 설정하세요."
    }
    if (-not $env:SOOP_TOKEN_ENCRYPTION_KEY) {
        $bytes = New-Object byte[] 32
        [Security.Cryptography.RandomNumberGenerator]::Fill($bytes)
        $env:SOOP_TOKEN_ENCRYPTION_KEY = [Convert]::ToBase64String($bytes)
        Write-Host "시험용 토큰 암호화 키를 이번 실행에만 생성했습니다." -ForegroundColor DarkGray
    }
    if (-not $env:CHAT_BRIDGE_SECRET) {
        $bytes = New-Object byte[] 32
        [Security.Cryptography.RandomNumberGenerator]::Fill($bytes)
        $env:CHAT_BRIDGE_SECRET = [Convert]::ToBase64String($bytes)
    }
    $env:SERVER_INTERNAL_URL = "http://localhost$Listen"
    # Chat SDK를 실제 앱 Origin에서 실행한다. about:blank Origin은 SOOP API가 거절할 수 있다.
    $env:CHAT_SDK_PAGE_URL = "http://localhost$Listen"
    $env:CHROMIUM_PATH = "C:\Program Files\Google\Chrome\Application\chrome.exe"
    if (-not (Test-Path -LiteralPath $env:CHROMIUM_PATH)) {
        throw "Chrome 실행 파일을 찾지 못했습니다: $env:CHROMIUM_PATH"
    }
    if (-not (Test-Path "chat-bridge\node_modules")) {
        npm --prefix chat-bridge ci --ignore-scripts
        if (-not $?) { throw "Chat SDK 브리지 패키지 설치 실패" }
    }
    $bridgeProcess = Start-Process -FilePath "node" -ArgumentList ".\chat-bridge\bridge.mjs" `
        -WorkingDirectory $PSScriptRoot -WindowStyle Hidden -PassThru `
        -RedirectStandardOutput "chat-bridge.log" -RedirectStandardError "chat-bridge.log.err"
}

Write-Host ""
Write-Host "서버 주소:   http://localhost$Listen" -ForegroundColor Cyan
Write-Host "관리자 화면: http://localhost$Listen/admin" -ForegroundColor Cyan
Write-Host "관리자 SOOP ID: $AdminSOOPIDs" -ForegroundColor DarkGray
if ($Streamers) { Write-Host "강제 수집:   $Streamers (개발용)" -ForegroundColor Yellow }
if ($OfficialChat) {
    Write-Host "공식 채팅:   활성 (로그: chat-bridge.log)" -ForegroundColor Green
    Write-Host "SOOP 콜백:   http://localhost$Listen/api/oauth/soop/callback" -ForegroundColor Cyan
}
Write-Host "중지하려면 Ctrl+C" -ForegroundColor DarkGray
Write-Host ""

try {
    & ".\bin\sooptalent.exe"
} finally {
    if ($bridgeProcess -and -not $bridgeProcess.HasExited) {
        Stop-Process -Id $bridgeProcess.Id -Force
    }
}
