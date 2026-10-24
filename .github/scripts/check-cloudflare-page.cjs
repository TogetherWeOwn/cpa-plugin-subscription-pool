// Render the REAL throwaway host's exported quota page, without upstream traffic.
// The directory is SP_CLOUDFLARE_PAGE_OUT from the host matrix. CI runs:
// NODE_PATH=<dir with playwright installed>/node_modules node check-cloudflare-page.cjs <directory>
const { chromium } = require('playwright');
const fs = require('node:fs');
const path = require('node:path');
const http = require('node:http');
const assert = require('node:assert/strict');

(async () => {
  const root = process.argv[2];
  assert(root, 'fixture directory required');
  const server = http.createServer((req, res) => {
    if (req.url === '/favicon.ico') { res.writeHead(204); res.end(); return; }
    const data = req.url.split('?')[0].endsWith('usage.json');
    res.setHeader('Content-Type', data ? 'application/json' : 'text/html');
    res.end(fs.readFileSync(path.join(root, data ? 'usage.json' : 'quota.html')));
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  let browser;
  const errors = [];
  try {
    browser = await chromium.launch({ headless: true, executablePath: chromium.executablePath(), args: ['--no-sandbox'], timeout: 30000 });
    for (const [name, width, height, theme] of [
      ['desktop-light', 1280, 960, 'light'], ['mobile-dark', 390, 844, 'dark']
    ]) {
      const page = await browser.newPage({ viewport: { width, height }, colorScheme: theme });
      page.on('pageerror', e => errors.push(e.message));
      page.on('console', msg => { if (msg.type() === 'error') errors.push(msg.text()); });
      await page.goto(`http://127.0.0.1:${server.address().port}/status`);
      await page.getByText('Daily neurons (UTC)', { exact: true }).waitFor();
      const card = page.locator('.qcard').filter({ hasText: 'Cloudflare Workers AI' });
      assert.equal(await card.count(), 1, 'one Cloudflare account card');
      const text = await card.innerText();
      for (const wanted of ['Included-only (daily; no rollover)', 'Local debits (conservative)', '9208', 'Calendar daily allowances (not pooled)', '310000', 'not a billing guarantee']) {
        assert(text.includes(wanted), `missing quota text: ${wanted}`);
      }
      assert(!text.includes('100% left'), 'exhausted guard must not advertise a full allowance');
      assert(!(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth + 1)), 'page overflows viewport');
      await card.scrollIntoViewIfNeeded();
      await page.screenshot({ path: path.join(root, `${name}.png`), fullPage: true });
      await card.screenshot({ path: path.join(root, `${name}-card.png`) });
      console.log(`${name}: Cloudflare quota renders with daily boundary, separate debits, calendar caveat; no horizontal overflow`);
      await page.close();
    }
    assert.deepEqual(errors, [], 'browser errors');
    console.log('PASS: desktop/light and mobile/dark quota page; zero browser errors');
  } finally {
    if (browser) await browser.close();
    server.closeAllConnections();
    await new Promise(resolve => server.close(resolve));
  }
})().catch(err => { console.error(err.message); process.exitCode = 1; });
