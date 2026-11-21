const { defineConfig } = require('@playwright/test');
// G-04 browser smoke. Base URL injected by CI (real server on :20128).
// Chromium only; synthetic fixtures only; no real provider keys.
module.exports = defineConfig({
  testDir: '.',
  timeout: 60 * 1000,
  retries: 1,
  use: {
    baseURL: process.env.BASE_URL || 'http://127.0.0.1:20128',
    trace: 'off',
    screenshot: 'only-on-failure',
  },
  projects: [{ name: 'chromium', use: { browserName: 'chromium' } }],
});
