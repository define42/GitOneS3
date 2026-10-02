import { defineConfig } from "@playwright/test";
import pagination from "./playwright.pagination.config";

// Run mocked UI coverage without the OIDC/S3 compose stack. The SSH lifecycle
// test remains in the full integration suite; only clone controls are mocked.
export default defineConfig({
  ...pagination,
  webServer: {
    command: "npm run build && node e2e/serve-ui.mjs",
    url: "http://127.0.0.1:4175",
    reuseExistingServer: false,
    timeout: 30_000,
  },
  projects: [
    {
      name: "repository-ui",
      testMatch: ["repository-pagination.spec.ts", "repository-browse.spec.ts", "lfs-browser.spec.ts"],
    },
    { name: "theme-ui", testMatch: "themes.spec.ts" },
    { name: "ssh-ui", testMatch: "ssh-keys.spec.ts", grep: /repository clone choices/ },
  ],
});
