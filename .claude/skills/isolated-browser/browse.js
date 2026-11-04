// コンテナの中で動かす (直接ホストで動かさない)。browse.sh が呼ぶ。
// 環境変数: URL, AUTH (user:pass, 任意), STEPS (steps.json のパス, 任意), SCRIPT (カスタム script のパス, 任意), OUT (出力先)
// SCRIPT は `module.exports = async ({ page, ctx, browser, url, out, steps }) => { ... }`。
//   定番の処理 (steps の各 do) は `steps(list)` で呼べる。page / ctx は Playwright のもの。
//   out(name, ext) は成果物のパスを返す。localhost の URL は page.goto 時に host.docker.internal へ書き換わる。
// steps.json は [{ "do": "click", "selector": "text=Login" }, ...]。
//   goto url | click selector | fill selector value | press selector key | waitFor selector
//   wait ms | screenshot name | snapshot name (ARIA ツリー) | text name (body の innerText)
// steps なしなら goto → networkidle → screenshot "page" + snapshot "page"。
const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');

(async () => {
  const { AUTH, STEPS, SCRIPT, OUT = 'out' } = process.env;
  const fix = (u) => (u || '').replace(/\/\/(localhost|127\.0\.0\.1)(?=[:/]|$)/, '//host.docker.internal');
  const url = fix(process.env.URL);
  fs.mkdirSync(OUT, { recursive: true });
  const b = await chromium.launch({ channel: 'chrome', chromiumSandbox: false }); // コンテナ自体が隔離
  const ctx = await b.newContext({
    viewport: { width: 1700, height: 1200 },
    extraHTTPHeaders: AUTH ? { Authorization: 'Basic ' + Buffer.from(AUTH).toString('base64') } : {},
  });
  const p = await ctx.newPage();
  const out = (n, ext) => path.join(OUT, n + ext);
  const run = async (list) => { for (const s of list) {
    switch (s.do) {
      case 'goto': await p.goto(fix(s.url) || url, { waitUntil: 'networkidle', timeout: 60000 }); await p.waitForTimeout(s.settle ?? 3000); break;
      case 'click': await p.click(s.selector); break;
      case 'fill': await p.fill(s.selector, s.value); break;
      case 'press': await p.press(s.selector, s.key); break;
      case 'waitFor': await p.waitForSelector(s.selector, { timeout: 30000 }); break;
      case 'wait': await p.waitForTimeout(s.ms); break;
      case 'screenshot': await p.screenshot({ path: out(s.name, '.png'), fullPage: true }); break;
      case 'snapshot': fs.writeFileSync(out(s.name, '.aria.txt'), await p.locator('body').ariaSnapshot()); break;
      case 'text': fs.writeFileSync(out(s.name, '.txt'), await p.innerText('body')); break;
      default: throw new Error('unknown step: ' + s.do);
    }
  } };
  if (SCRIPT) await require(path.resolve(SCRIPT))({ page: p, ctx, browser: b, url, out, steps: run });
  else await run(STEPS ? JSON.parse(fs.readFileSync(STEPS, 'utf8')) : [
    { do: 'goto', url }, { do: 'screenshot', name: 'page' }, { do: 'snapshot', name: 'page' },
  ]);
  console.log('chrome ' + b.version() + ' url=' + p.url());
  await b.close();
})().catch((e) => { console.error(e); process.exit(1); });
