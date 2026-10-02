import { defineConfig, devices } from "@playwright/test";

// These browser regressions mock API responses and need only the application
// shell. The full OIDC/Git lifecycle suite still uses playwright.config.ts.
export default defineConfig({
  testDir: "./e2e",
  testMatch: "repository-pagination.spec.ts",
  workers: 1,
  timeout: 30_000,
  expect: { timeout: 10_000 },
  reporter: "list",
  use: {
    ...devices["Desktop Chrome"],
    baseURL: "http://127.0.0.1:4175",
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  webServer: {
    command: "npx vite --host 127.0.0.1 --port 4175 --strictPort --base=/",
    url: "http://127.0.0.1:4175",
    reuseExistingServer: false,
    timeout: 30_000,
  },
});
