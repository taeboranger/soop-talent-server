// 서버에 박혀 있는 가이드를 GitHub Pages용 정적 파일로 뽑아낸다.
//
//   node tools/build-docs.mjs [출력경로]
//
// 원본(internal/api/*.html)이 유일한 정본이다. 생성물을 손으로 고치면 다음 실행 때 덮어써진다.
//
// 이 저장소는 private이라 GitHub Pages를 쓸 수 없어(유료 플랜 필요),
// 공개용 문서는 옆의 sooptalent-docs 저장소로 뽑는다. 그래서 기본 출력이 ../sooptalent-docs다.
// 그쪽 저장소는 이 파일들만 담고 있으므로 Pages Source를 `main` / `/(root)`로 켜면 된다.
//
// 가이드는 인라인 CSS·SVG로 자체 포함되어 있어 손댈 곳은 링크뿐이다.
// 서버에서는 "/guide", "/guide/detail" 이지만 Pages에서는 파일 이름이 된다.

import { readFile, writeFile, mkdir } from "node:fs/promises";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const src = join(root, "internal", "api");
const out = process.argv[2] ? resolve(process.argv[2]) : join(root, "..", "sooptalent-docs");

/**
 * 각 페이지의 링크 치환 규칙.
 *
 * `required: true`인 규칙이 하나도 안 걸리면 빌드를 세운다.
 * 원본이 바뀌어 규칙이 헛돌면 링크가 조용히 깨진 채로 배포되기 때문이다.
 */
const pages = [
  {
    from: "guide.html",
    to: "index.html",
    rules: [
      // 공개 사본에는 대시보드가 없다 — 스트리머마다 서버가 다르므로 가리킬 주소가 없다.
      // 대신 자세한 안내로 보낸다. 그쪽 헤더에도 되돌아오는 링크가 있어 짝이 맞는다.
      {
        find: '<a href="/">대시보드로</a>',
        replace: '<a href="detail.html">자세한 안내 →</a>',
        required: true,
      },
      { find: 'href="/guide/detail"', replace: 'href="detail.html"', required: true },
    ],
  },
  {
    from: "guide-detail.html",
    to: "detail.html",
    rules: [
      { find: 'href="/guide"', replace: 'href="index.html"', required: true, all: true },
    ],
  },
];

function applyRule(html, rule, label) {
  const count = html.split(rule.find).length - 1;
  if (count === 0) {
    if (rule.required) {
      throw new Error(
        `${label}: ${JSON.stringify(rule.find)} 을(를) 찾지 못했습니다. ` +
          `원본이 바뀐 것 같습니다 — tools/build-docs.mjs 의 규칙을 함께 고쳐 주세요.`);
    }
    return html;
  }
  if (count > 1 && !rule.all) {
    throw new Error(`${label}: ${JSON.stringify(rule.find)} 이(가) ${count}군데입니다. ` +
      `의도한 것이면 규칙에 all: true 를 넣어 주세요.`);
  }
  return html.split(rule.find).join(rule.replace);
}

// 남은 절대 경로 링크를 잡아낸다. //example.com 같은 외부 주소는 건드리지 않는다.
function findServerLinks(html) {
  return [...html.matchAll(/(?:href|src)="(\/[^/"][^"]*|\/)"/g)].map((m) => m[1]);
}

await mkdir(out, { recursive: true });

for (const page of pages) {
  let html = await readFile(join(src, page.from), "utf8");
  for (const rule of page.rules) html = applyRule(html, rule, page.from);

  const leftover = findServerLinks(html);
  if (leftover.length) {
    throw new Error(`${page.from}: 서버 경로 링크가 남았습니다 — ${leftover.join(", ")}`);
  }

  await writeFile(join(out, page.to), html);
  console.log(`${page.from} → ${join(out, page.to)} (${html.length.toLocaleString()}자)`);
}

// GitHub Pages의 Jekyll 처리를 끈다. 밑줄로 시작하는 파일이 없어도 켜 두면
// 빌드 단계가 붙어 배포가 느려지고, 나중에 파일을 늘릴 때 걸린다.
await writeFile(join(out, ".nojekyll"), "");
console.log(join(out, ".nojekyll"));
