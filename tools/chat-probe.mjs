#!/usr/bin/env node
// SOOP 채팅 프로토콜 탐침 — 미확인 svc 코드(구독/후원/팬가입) 발굴용
// 사용: node tools/chat-probe.mjs <bid> [outDir]
// 의존성 없음 (Node 22+ 내장 WebSocket 사용)

import { appendFileSync, mkdirSync } from "node:fs";
import { join } from "node:path";
import WebSocket from "ws";

// SOOP 채팅 서버는 브라우저와 동일한 핸드셰이크 헤더를 요구한다.
// Origin이 없으면 소켓은 열리지만 아무 패킷도 내려오지 않는다.
const WS_HEADERS = {
  Origin: "https://play.sooplive.com",
  "User-Agent":
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
    "(KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36",
};

const BID = process.argv[2];
const OUT_DIR = process.argv[3] ?? "./probe-out";
if (!BID) {
  console.error("사용법: node tools/chat-probe.mjs <bid> [outDir]");
  process.exit(1);
}
mkdirSync(OUT_DIR, { recursive: true });

const ESC = "\x1b\t";
const F = "\x0c";
const p4 = (n) => String(n).padStart(4, "0");
const p6 = (n) => String(n).padStart(6, "0");

// 이미 의미를 아는 코드 — 나머지가 발굴 대상.
// 2026-08-10 천양 방송에서 0013/0018/0087/0109를 해독해 추가했다.
// 0054/0090/0094/0110은 접속 직후 1회씩만 오는 제어성 패킷이라 알림에서 뺀다.
const KNOWN = new Set([
  "0000", "0001", "0002", "0004", "0005",
  "0012", // 입장
  "0013", // 퇴장
  "0018", // 별풍선 후원
  "0054", "0090", "0094", "0110", // 접속 직후 제어 패킷 (의미 미상, 저빈도)
  "0087", // 애드벌룬 후원
  "0104", // 공지
  "0109", // 이모티콘 채팅
  "0127", // 팬클럽 정보
]);
// 양이 많아 원본 저장에서 제외 (통계만)
const NOISY = new Set(["0004", "0127"]);

const RAW = join(OUT_DIR, `raw-${BID}.jsonl`);
const HITS = join(OUT_DIR, `unknown-${BID}.jsonl`);

const stats = Object.create(null);
let totalPackets = 0;
let reconnects = 0;
let startedAt = Date.now();

function pack(svc, body) {
  const b = Buffer.from(body, "utf8");
  const head = Buffer.from(ESC + p4(svc) + p6(b.length) + "00", "utf8");
  return Buffer.concat([head, b]);
}

async function getChannel(bid) {
  const body = new URLSearchParams({
    bid, type: "live", pwd: "", player_type: "html5",
    stream_type: "common", quality: "HD", mode: "landing",
    from_api: "0", is_revive: "false",
  });
  const r = await fetch(`https://live.sooplive.com/afreeca/player_live_api.php?bjid=${bid}`, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body,
  });
  if (!r.ok) throw new Error(`player_live_api HTTP ${r.status}`);
  const j = await r.json();
  return j.CHANNEL ?? {};
}

function report() {
  const mins = ((Date.now() - startedAt) / 60000).toFixed(1);
  const rows = Object.entries(stats).sort((a, b) => b[1] - a[1])
    .map(([svc, n]) => `${KNOWN.has(svc) ? " " : "*"}${svc}:${n}`)
    .join("  ");
  console.log(`[${mins}분] 총 ${totalPackets} 재접속 ${reconnects} | ${rows}`);
}

function connect(ch) {
  const url = `wss://${ch.CHDOMAIN}:${Number(ch.CHPT) + 1}/Websocket/${BID}`;
  console.log(`접속: ${url}  CHATNO=${ch.CHATNO}  BNO=${ch.BNO}  "${ch.TITLE}"`);

  const ws = new WebSocket(url, "chat", { headers: WS_HEADERS });
  let ping = null;

  ws.on("open", () => {
    console.log("소켓 열림 — CONNECT 전송");
    ws.send(pack(1, F + F + "16" + F + F + F));
  });

  ws.on("message", (data) => {
    const buf = Buffer.isBuffer(data) ? data : Buffer.from(data);
    const s = buf.toString("utf8");
    const svc = s.slice(2, 6);
    const parts = s.slice(14).split(F);

    totalPackets++;
    stats[svc] = (stats[svc] ?? 0) + 1;

    if (svc === "0001") {
      ws.send(pack(2, F + ch.CHATNO + F + F + F + F + F));
      ping = setInterval(() => {
        try { ws.send(pack(0, F)); } catch { /* 다음 재접속에서 복구 */ }
      }, 20000);
    }

    if (!NOISY.has(svc)) {
      const rec = { t: new Date().toISOString(), bno: ch.BNO, svc, parts };
      appendFileSync(RAW, JSON.stringify(rec) + "\n");
      if (!KNOWN.has(svc)) {
        appendFileSync(HITS, JSON.stringify({ ...rec, hex: buf.toString("hex") }) + "\n");
        console.log(`\n*** 미확인 svc=${svc} ***\n${JSON.stringify(parts)}\n`);
      }
    }
  });

  let closed = false;
  const retry = (why) => {
    if (closed) return;
    closed = true;
    clearInterval(ping);
    console.log(`끊김(${why}) — 5초 후 재접속`);
    reconnects++;
    setTimeout(main, 5000);
  };
  ws.on("error", (e) => retry(`error=${e.message}`));
  ws.on("close", (code) => retry(`code=${code}`));
}

async function main() {
  try {
    const ch = await getChannel(BID);
    if (Number(ch.RESULT) !== 1) {
      console.log("방송 중이 아님 — 60초 후 재확인");
      return setTimeout(main, 60000);
    }
    connect(ch);
  } catch (e) {
    console.log(`채널 조회 실패(${e.message}) — 15초 후 재시도`);
    setTimeout(main, 15000);
  }
}

setInterval(report, 60000);
process.on("SIGINT", () => { report(); process.exit(0); });

console.log(`탐침 시작: bid=${BID}  출력=${OUT_DIR}`);
console.log(`  전체 원본 → ${RAW}`);
console.log(`  미확인만 → ${HITS}`);
main();
