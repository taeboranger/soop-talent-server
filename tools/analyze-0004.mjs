#!/usr/bin/env node
// svc 0004(시청자 목록) 패킷 구조 검증기.
//
// 서버는 "["", "1", id, nick, flag] 이후 3칸씩 반복" 을 가정하고 유저 ID를 뽑는다.
// 이 가정이 깨지면 닉네임이나 플래그를 유저 ID로 읽어 엉뚱한 계정에 점수가 쌓인다.
// 실제로 "-1" 이 유저로 적립된 사례가 있어 구조를 확인한다.
//
// 사용: node tools/analyze-0004.mjs <bid> [초]

import WebSocket from "ws";

const BID = process.argv[2];
const SECONDS = Number(process.argv[3] ?? 40);
if (!BID) { console.error("사용법: node tools/analyze-0004.mjs <bid> [초]"); process.exit(1); }

const ESC = "\x1b\t", F = "\x0c";
const p4 = (n) => String(n).padStart(4, "0");
const p6 = (n) => String(n).padStart(6, "0");
const pack = (svc, body) => {
  const b = Buffer.from(body, "utf8");
  return Buffer.concat([Buffer.from(ESC + p4(svc) + p6(b.length) + "00", "utf8"), b]);
};

const HEADERS = {
  Origin: "https://play.sooplive.com",
  "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
    "(KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36",
};

// 플래그 칸은 "숫자|숫자" 형태다. 이걸로 정렬이 맞는지 판정한다.
const isFlag = (s) => /^\d+\|\d+$/.test(s);

const stats = {
  packets: 0,
  headerVariants: new Map(), // parts[1] 값 분포
  lenMod: new Map(),         // (len-3) % 3 분포 → 3칸 배수인지
  misaligned: [],            // 정렬이 깨진 패킷 샘플
  suspiciousIDs: new Map(),  // 유저ID 자리에서 나온 이상한 값
};

function analyze(parts) {
  stats.packets++;
  stats.headerVariants.set(parts[1], (stats.headerVariants.get(parts[1]) ?? 0) + 1);

  // 앞 2칸(""·"1") + 뒤 1칸("") 을 뺀 나머지가 3의 배수여야 한다
  const body = parts.length - 3;
  const mod = body % 3;
  stats.lenMod.set(mod, (stats.lenMod.get(mod) ?? 0) + 1);

  // 서버와 같은 방식으로 훑으며 정렬 확인
  for (let i = 2; i + 2 < parts.length; i += 3) {
    const [id, nick, flag] = [parts[i], parts[i + 1], parts[i + 2]];
    if (!isFlag(flag)) {
      if (stats.misaligned.length < 5) {
        stats.misaligned.push({ i, id, nick, flag, len: parts.length });
      }
      break; // 한 번 어긋나면 이후는 전부 의미 없다
    }
    // 유저 ID로 보기 어려운 값
    if (id === "" || /^-?\d+$/.test(id) === false) continue;
    if (id === "-1" || id === "0" || Number(id) < 0) {
      stats.suspiciousIDs.set(id, (stats.suspiciousIDs.get(id) ?? 0) + 1);
    }
  }
}

async function getChannel(bid) {
  const body = new URLSearchParams({
    bid, type: "live", pwd: "", player_type: "html5", stream_type: "common",
    quality: "HD", mode: "landing", from_api: "0", is_revive: "false",
  });
  const r = await fetch(`https://live.sooplive.com/afreeca/player_live_api.php?bjid=${bid}`, {
    method: "POST", headers: { "Content-Type": "application/x-www-form-urlencoded" }, body,
  });
  return (await r.json()).CHANNEL ?? {};
}

const ch = await getChannel(BID);
if (Number(ch.RESULT) !== 1) { console.error("방송 중이 아닙니다"); process.exit(1); }

const ws = new WebSocket(
  `wss://${ch.CHDOMAIN}:${Number(ch.CHPT) + 1}/Websocket/${BID}`, "chat", { headers: HEADERS });

ws.on("open", () => ws.send(pack(1, F + F + "16" + F + F + F)));
ws.on("message", (data) => {
  const s = Buffer.from(data).toString("utf8");
  const svc = s.slice(2, 6);
  if (svc === "0001") {
    ws.send(pack(2, F + ch.CHATNO + F + F + F + F + F));
    setInterval(() => { try { ws.send(pack(0, F)); } catch {} }, 20000);
    return;
  }
  if (svc === "0004") analyze(s.slice(14).split(F));
});
ws.on("error", (e) => { console.error("오류:", e.message); process.exit(1); });

setTimeout(() => {
  console.log(`\n=== ${BID} / 0004 패킷 ${stats.packets}건 분석 ===`);
  console.log("parts[1] 값 분포:", Object.fromEntries(stats.headerVariants));
  console.log("(길이-3)%3 분포 :", Object.fromEntries(stats.lenMod), " ← 0만 나와야 정상");
  console.log("정렬 깨진 샘플  :", stats.misaligned.length ? stats.misaligned : "없음");
  console.log("의심 유저ID     :", stats.suspiciousIDs.size
    ? Object.fromEntries(stats.suspiciousIDs) : "없음");
  process.exit(0);
}, SECONDS * 1000);
