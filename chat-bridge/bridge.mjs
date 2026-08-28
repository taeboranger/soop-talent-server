import crypto from "node:crypto";
import http from "node:http";
import puppeteer from "puppeteer-core";

const server = (process.env.SERVER_INTERNAL_URL || "http://127.0.0.1:8080").replace(/\/$/, "");
// SDK의 OpenAPI 요청에는 페이지 Origin이 포함된다. about:blank(Origin: null)가 아니라
// 실제 SoopTalent 주소의 빈 페이지를 사용한다.
const sdkPageURL = (process.env.CHAT_SDK_PAGE_URL || server).replace(/\/$/, "") + "/internal/chat/sdk-host";
const bridgeSecret = process.env.CHAT_BRIDGE_SECRET || "";
const clientId = process.env.SOOP_CLIENT_ID || "";
const clientSecret = process.env.SOOP_CLIENT_SECRET || "";
const executablePath = process.env.CHROMIUM_PATH || "/usr/bin/chromium";
const sdkURL = "https://static.sooplive.com/asset/app/chat-sdk/sooplive-chat-sdk.js";
const pollMs = 30_000;
const identityHost = process.env.CHAT_BRIDGE_HOST || "127.0.0.1";
const identityPort = Number(process.env.CHAT_BRIDGE_PORT || 8098);

if (!bridgeSecret || !clientId || !clientSecret) {
  throw new Error("CHAT_BRIDGE_SECRET, SOOP_CLIENT_ID, SOOP_CLIENT_SECRET가 모두 필요합니다.");
}

const headers = { Authorization: `Bearer ${bridgeSecret}`, "Content-Type": "application/json" };
const sessions = new Map();
// 달란트 계산 또는 체류 상태에 직접 쓰이는 공식 SDK 이벤트만 내부 API로 전달한다.
// BANNED_WORDS 같은 방송 설정 초기화 이벤트는 연결 성공 신호일 뿐, 저장할 필요가 없다.
const forwardedActions = new Set([
  "IN", "JOIN", "OUT", "QUIT", "MESSAGE", "USERSTATUS_CHANGED", "MANAGER_MESSAGE",
  "BALLOON_GIFTED", "ADBALLOON_GIFTED", "VIDEOBALLOON_GIFTED",
  "BATTLE_MISSION_GIFTED", "CHALLENGE_MISSION_GIFTED",
  "SUBSCRIBED", "SUBSCRIPTION_RENEWED", "SUBSCRIPTION_GIFTED",
]);
let stopping = false;
let browser = null;

function bridgeAuthorized(req) {
  const supplied = String(req.headers.authorization || "").replace(/^Bearer\s+/i, "");
  const expected = Buffer.from(bridgeSecret);
  const provided = Buffer.from(supplied);
  return expected.length === provided.length && crypto.timingSafeEqual(expected, provided);
}

function sendJSON(res, status, value) {
  res.writeHead(status, { "Content-Type": "application/json; charset=utf-8", "Cache-Control": "no-store" });
  res.end(JSON.stringify(value));
}

async function identify(accessToken) {
  if (!browser) throw new Error("Chat SDK 브라우저가 아직 준비되지 않았습니다.");
  const page = await browser.newPage();
  try {
    await page.goto(sdkPageURL, { waitUntil: "domcontentloaded" });
    await page.addScriptTag({ url: sdkURL });
    await page.waitForFunction(() => Boolean(window.SOOP?.ChatSDK), { timeout: 20_000 });
    return await page.evaluate(async ({ clientId, clientSecret, accessToken }) => {
      const sdk = new window.SOOP.ChatSDK(clientId, clientSecret);
      sdk.setAuth(accessToken);
      await sdk.connect();
      return await sdk.getRoomInfo?.() || {};
    }, { clientId, clientSecret, accessToken });
  } finally {
    await page.close().catch(() => {});
  }
}

const identityServer = http.createServer(async (req, res) => {
  if (req.method !== "POST" || new URL(req.url, "http://localhost").pathname !== "/identity") {
    sendJSON(res, 404, { error: "not found" });
    return;
  }
  if (!bridgeAuthorized(req)) {
    sendJSON(res, 401, { error: "unauthorized" });
    return;
  }
  try {
    let raw = "";
    for await (const chunk of req) {
      raw += chunk;
      if (raw.length > 64 * 1024) throw new Error("request too large");
    }
    const { accessToken } = JSON.parse(raw);
    if (typeof accessToken !== "string" || !accessToken || accessToken.length > 16 * 1024) {
      sendJSON(res, 400, { error: "invalid access token" });
      return;
    }
    const room = await identify(accessToken);
    const bjId = String(room?.bjId || "").trim();
    if (!bjId) {
      sendJSON(res, 422, { error: "Chat SDK room has no bjId" });
      return;
    }
    sendJSON(res, 200, { bjId, chatNumber: String(room?.chatNumber || "") });
  } catch (error) {
    console.warn("OAuth 방송국 ID 확인 실패:", error.message);
    if (/(?:^|\D)104(?:\D|$)|진행중인 방송이 없습니다/.test(String(error.message))) {
      sendJSON(res, 409, { error: "live broadcast required" });
      return;
    }
    sendJSON(res, 502, { error: "Chat SDK identity lookup failed" });
  }
});

await new Promise((resolve, reject) => {
  identityServer.once("error", reject);
  identityServer.listen(identityPort, identityHost, resolve);
});

async function request(path, init = {}, retries = 0) {
  let last;
  for (let attempt = 0; attempt <= retries; attempt++) {
    try {
      const response = await fetch(server + path, { ...init, headers: { ...headers, ...(init.headers || {}) } });
      if (!response.ok) {
        // 이벤트 본문에는 채팅이 들어갈 수 있으므로 기록하지 않는다.
        // 서버가 돌려준 검증 사유와 action만으로 연동 규격을 진단한다.
        const detail = (await response.text()).replace(/\s+/g, " ").slice(0, 500);
        throw new Error(`${path}: HTTP ${response.status}${detail ? ` ${detail}` : ""}`);
      }
      return response;
    } catch (error) {
      last = error;
      if (attempt < retries) await new Promise((r) => setTimeout(r, 500 * (attempt + 1)));
    }
  }
  throw last;
}

async function state(bid, phase, broadNo = "", detail = "", error = "") {
  try {
    await request("/internal/chat/state", { method: "POST", body: JSON.stringify({ bid, phase, broadNo, detail, error }) }, 2);
  } catch (err) {
    console.error(`[${bid}] 상태 전송 실패:`, err.message);
  }
}

async function event(bid, broadNo, action, message) {
  const body = {
    bid,
    event: { id: crypto.randomUUID(), action: String(action), broadNo: String(broadNo || ""), message },
  };
  await request("/internal/chat/event", { method: "POST", body: JSON.stringify(body) }, 3);
}

async function closeSession(bid) {
  const session = sessions.get(bid);
  sessions.delete(bid);
  if (!session) return;
  session.closed = true;
  try { await session.page.close(); } catch {}
}

// ChatSDK의 connect()가 내부적으로 호출하는 동일한 공개 API를 먼저 확인한다.
// 토큰·티켓·방 주소는 절대 로그에 남기지 않고, 연결 가능 여부만 상태 화면에 반영한다.
async function chatInfoStatus(accessToken) {
  const response = await fetch("https://openapi.sooplive.com/broad/access/chatinfo", {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded;charset=utf-8" },
    body: new URLSearchParams({ access_token: accessToken }),
  });
  const body = await response.json().catch(() => ({}));
  return {
    httpStatus: response.status,
    result: Number(body?.result || 0),
    message: String(body?.msg || body?.message || "").trim(),
    rooms: Array.isArray(body?.data) ? body.data.length : 0,
  };
}

async function openSession(browser, target) {
  const { bid, accessToken, expiresAt } = target;
  const session = { bid, accessToken, expiresAt, page: null, closed: false, dead: false };
  sessions.set(bid, session);
  await state(bid, "connecting", "", "SOOP 공식 Chat SDK에 접속하는 중입니다.");

  try {
    const info = await chatInfoStatus(accessToken);
    if (info.httpStatus !== 200 || info.result <= 0 || info.rooms === 0) {
      const error = `chatinfo result=${info.result}${info.message ? `: ${info.message}` : ""} (HTTP ${info.httpStatus})`;
      session.dead = true;
      await state(bid, "offline", "", "SOOP이 현재 채팅방 정보를 아직 제공하지 않았습니다. 자동으로 다시 확인합니다.", error);
      console.warn(`[${bid}] 채팅방 정보 대기:`, error);
      return;
    }
    console.log(`[${bid}] 채팅방 정보 확인 (${info.rooms}개)`);
    const page = await browser.newPage();
    session.page = page;
    await page.goto(sdkPageURL, { waitUntil: "domcontentloaded" });
    await page.exposeFunction("__soopTalentEvent", async (action, message, broadNo) => {
      const normalizedAction = String(action || "").toUpperCase();
      if (!forwardedActions.has(normalizedAction)) return;
      try { await event(bid, broadNo, normalizedAction, message); }
      catch (err) { console.error(`[${bid}] 이벤트 전송 실패 (${String(action)}):`, err.message); }
    });
    await page.exposeFunction("__soopTalentState", async (phase, broadNo, detail, error) => {
      await state(bid, phase, broadNo, detail, error);
      // SDK는 연결 직후에도 일시 오류를 알릴 수 있고 자체 재연결한다.
      // retrying만으로 페이지를 버리면 이미 도착한 채팅 이벤트까지 끊긴다.
      if (phase === "stopped") session.dead = true;
    });
    await page.addScriptTag({ url: sdkURL });
    await page.waitForFunction(() => Boolean(window.SOOP?.ChatSDK), { timeout: 20_000 });

    const room = await page.evaluate(async ({ clientId, clientSecret, accessToken }) => {
      const sdk = new window.SOOP.ChatSDK(clientId, clientSecret);
      window.__soopTalentSDK = sdk;
      window.__soopTalentRoom = {};
      window.__soopTalentReady = false;
      let resolveFirstEvent;
      const firstEvent = new Promise((resolve) => { resolveFirstEvent = resolve; });
      sdk.setAuth(accessToken);
      sdk.handleMessageReceived((action, message) => {
        const room = window.__soopTalentRoom;
        // LOGIN 및 초기 상태 이벤트는 모두 채팅 서버가 실제로 메시지를 보낸 증거다.
        // 일부 SDK 버전은 이 후에도 connect Promise를 끝내지 않거나 104를 reject한다.
        resolveFirstEvent();
        window.__soopTalentEvent(action, message, room.chatNumber || "");
      });
      sdk.handleChatClosed?.(() => {
        const room = window.__soopTalentRoom;
        window.__soopTalentState("stopped", room.chatNumber || "", "공식 채팅 연결이 종료되었습니다.", "");
      });
      sdk.handleError?.((code, message) => {
        const room = window.__soopTalentRoom;
        const text = [code, message].filter(Boolean).map(String).join(": ");
        const phase = window.__soopTalentReady ? "connected" : "retrying";
        const detail = window.__soopTalentReady
          ? "공식 채팅을 받고 있습니다. SDK의 일시 상태 알림을 확인 중입니다."
          : "공식 채팅 연결을 다시 확인합니다.";
        window.__soopTalentState(phase, room.chatNumber || "", detail, text);
      });
      const connected = sdk.connect().then(
        () => ({ kind: "connect" }),
        (error) => ({ kind: "error", error }),
      );
      let outcome = await Promise.race([
        connected,
        firstEvent.then(() => ({ kind: "event" })),
        new Promise((resolve) => setTimeout(() => resolve({ kind: "timeout" }), 12_000)),
      ]);
      if (outcome.kind === "error") {
        // 오류가 먼저 와도 잠시 안에 SDK 이벤트가 오면 실제 채팅 연결은 살아 있다.
        outcome = await Promise.race([
          firstEvent.then(() => ({ kind: "event" })),
          new Promise((resolve) => setTimeout(() => resolve(outcome), 750)),
        ]);
      }
      if (outcome.kind === "error") {
        throw outcome.error;
      }
      if (outcome.kind === "timeout") {
        throw new Error("SOOP Chat SDK 연결 시간 초과");
      }
      window.__soopTalentRoom = await sdk.getRoomInfo?.() || {};
      window.__soopTalentReady = true;
      return window.__soopTalentRoom;
    }, { clientId, clientSecret, accessToken });

    const broadNo = String(room?.chatNumber || "");
    await state(bid, "connected", broadNo, "SOOP 공식 Chat SDK로 채팅을 받고 있습니다.");
    console.log(`[${bid}] 공식 채팅 연결${broadNo ? ` (${broadNo})` : ""}`);
  } catch (error) {
    session.dead = true;
    await state(bid, "offline", "", "방송이 꺼져 있거나 채팅방이 아직 준비되지 않았습니다. 자동으로 다시 확인합니다.", error.message);
    console.warn(`[${bid}] 연결 대기:`, error.message);
  }
}

async function targets() {
  const response = await request("/internal/chat/targets", {}, 2);
  const json = await response.json();
  return Array.isArray(json.targets) ? json.targets : [];
}

async function reconcile(browser) {
  const wanted = new Map((await targets()).map((target) => [target.bid, target]));
  for (const bid of sessions.keys()) {
    const session = sessions.get(bid);
    const target = wanted.get(bid);
    if (!target || session.dead || session.accessToken !== target.accessToken) {
      await closeSession(bid);
    }
  }
  for (const target of wanted.values()) {
    if (!sessions.has(target.bid)) await openSession(browser, target);
  }
}

browser = await puppeteer.launch({
  executablePath,
  headless: true,
  args: ["--no-sandbox", "--disable-setuid-sandbox", "--disable-dev-shm-usage", "--disable-gpu"],
});

for (const signal of ["SIGINT", "SIGTERM"]) {
  process.on(signal, () => { stopping = true; });
}

console.log("SOOP 공식 Chat SDK 브리지 시작");
while (!stopping) {
  try { await reconcile(browser); }
  catch (error) { console.error("채팅 대상 동기화 실패:", error.message); }
  await new Promise((resolve) => setTimeout(resolve, pollMs));
}
for (const bid of [...sessions.keys()]) await closeSession(bid);
await browser.close();
await new Promise((resolve) => identityServer.close(resolve));
