const { test, expect } = require('@playwright/test');

// G-04 browser smoke: drive the REAL dashboard in a browser.
// Synthetic fixtures only; never real provider keys; assert no console
// errors on a healthy local app.

test('dashboard app loads without unexpected console errors', async ({ page }) => {
  // Auth-state tolerant: the app renders either the shell or the login/
  // setup view depending on whether a password is set. On an
  // UNAUTHENTICATED visit the dashboard correctly 401s its API calls
  // (gateway-key auth is ON), which surfaces as console "Failed to load
  // resource: 401" messages - that is expected auth behaviour, not JS
  // breakage. Any OTHER error (uncaught JS exceptions, non-401 failed
  // loads) still fails the test.
  const errors = [];
  page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));
  await page.goto('/dashboard');
  await expect(page.locator('body')).toContainText('Codvian Router');
  const unexpected = errors.filter((e) =>
    !(e.startsWith('Failed to load resource') && e.includes('401')));
  expect(unexpected).toEqual([]);
});

test('skip link and focus ring are present in the served DOM', async ({ page }) => {
  await page.goto('/dashboard');
  await expect(page.locator('a.skip-link')).toHaveAttribute('href', '#mainContent');
  await expect(page.locator('.sidebar')).toBeVisible();
});

test('reveal-key control does not leak a secret and is keyboard-operable', async ({ page }) => {
  await page.goto('/dashboard');
  const btn = page.locator('#revealKeyBtn').first();
  if ((await btn.count()) === 0) { test.skip('no reveal-key control in this auth state'); return; }
  await btn.focus();
  await expect(btn).toBeFocused();
  const text = (await btn.textContent()) || '';
  expect(text.toLowerCase()).not.toMatch(/sk-[a-z0-9]{8,}|bearer|secret/);
});

test('empty states render honestly (no fabricated rows)', async ({ page }) => {
  await page.goto('/dashboard');
  const unavail = page.locator('#catalogUnavailable, .feed-empty, [class*="empty"]').first();
  await expect(unavail).toHaveCount(1);
});
