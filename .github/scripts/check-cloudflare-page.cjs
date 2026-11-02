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
    // The calendar projection (days in the current UTC month x 10,000 daily
    // neurons) varies by month, so read the expectation from the exported
    // usage.json fixture instead of hardcoding it. The export was written by
    // the host matrix moments earlier; recomputing from the clock here would
    // flake on runs straddling UTC midnight on the 1st. This stays inside the
    // try so a missing or invalid fixture still reaches the finally below
    // that closes the listening server (otherwise Node would hang with an
    // exit code set but the event loop alive).
    const usage = JSON.parse(fs.readFileSync(path.join(root, 'usage.json'), 'utf8'));
    const fixtureCard = (usage.quota || []).find(q => q.provider === 'cloudflare-workers-ai');
    assert(fixtureCard, 'usage.json must contain a cloudflare-workers-ai quota card');
    const allowance = (fixtureCard.metrics || []).find(m => m.key === 'calendar_daily_allowances');
    assert(allowance, 'usage.json cloudflare-workers-ai card must contain a calendar_daily_allowances metric');
    assert(
      Number.isInteger(allowance.value) && allowance.value % 10000 === 0 && allowance.value >= 280000 && allowance.value <= 310000,
      `calendar_daily_allowances must be a whole multiple of 10000 between 280000 and 310000, got ${allowance.value}`
    );
    const expectedAllowance = String(allowance.value);
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
      for (const wanted of ['Included-only (daily; no rollover)', 'Local debits (conservative)', '9208', 'Calendar daily allowances (not pooled)', expectedAllowance, 'not a billing guarantee']) {
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
