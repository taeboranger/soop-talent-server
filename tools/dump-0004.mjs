#!/usr/bin/env node
// svc 0004의 변종(parts[1] != "1")을 원문 그대로 찍는다.
// 사용: node tools/dump-0004.mjs <bid> [초]

import WebSocket from "ws";

const BID = process.argv[2];
const SECONDS = Number(process.argv[3] ?? 40);
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

const variants = new Map(); // parts[1] -> 샘플 배열
const lengths = new Map();  // parts[1] -> 길이 분포

const body = new URLSearchParams({
  bid: BID, type: "live", pwd: "", player_type: "html5", stream_type: "common",
  quality: "HD", mode: "landing", from_api: "0", is_revive: "false",
});
const ch = (await (await fetch(
  `https://live.sooplive.com/afreeca/player_live_api.php?bjid=${BID}`,
  { method: "POST", headers: { "Content-Type": "application/x-www-form-urlencoded" }, body },
)).json()).CHANNEL;

const ws = new WebSocket(
  `wss://${ch.CHDOMAIN}:${Number(ch.CHPT) + 1}/Websocket/${BID}`, "chat", { headers: HEADERS });

ws.on("open", () => ws.send(pack(1, F + F + "16" + F + F + F)));
ws.on("message", (data) => {
  const s = Buffer.from(data).toString("utf8");
  if (s.slice(2, 6) === "0001") {
    ws.send(pack(2, F + ch.CHATNO + F + F + F + F + F));
    setInterval(() => { try { ws.send(pack(0, F)); } catch {} }, 20000);
    return;
  }
  if (s.slice(2, 6) !== "0004") return;

  const parts = s.slice(14).split(F);
  const key = parts[1];
  const lk = `${key}/len${parts.length}`;
  lengths.set(lk, (lengths.get(lk) ?? 0) + 1);

  if (!variants.has(key)) variants.set(key, []);
  const arr = variants.get(key);
  if (arr.length < 4) arr.push(parts);
});

setTimeout(() => {
  console.log(`\n=== ${BID} / 0004 변종 ===`);
  console.log("변종/길이 분포:", Object.fromEntries(lengths));
  for (const [key, samples] of variants) {
    console.log(`\n--- parts[1] = ${JSON.stringify(key)} ---`);
    for (const p of samples.slice(0, 3)) {
      console.log(`  길이 ${p.length}`);
      p.forEach((v, i) => { if (v !== "") console.log(`    [${i}] ${v}`); });
      console.log("");
    }
  }
  process.exit(0);
}, SECONDS * 1000);
