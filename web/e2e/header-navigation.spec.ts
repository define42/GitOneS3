import { expect, test } from "@playwright/test";
import { accountControl, openAccountMenu } from "./header-helpers";

for (const width of [375, 1280]) {
  test(`account navigation supports keyboard, dismissal and sign-out at ${width}px`, async ({ page }) => {
    const username = "navigation-user";
    const csrfToken = "navigation-test-csrf";
    const logoutError = "Sign out is temporarily unavailable. Please try again.";
    let authenticated = true;
    let logoutRequests = 0;
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await page.route("**/api/v1/**", async (route) => {
      const request = route.request();
      const path = new URL(request.url()).pathname;
      if (path === "/api/v1/session") {
        await route.fulfill({
          json: { authenticated, username, csrfToken, shardCount: 1, provider: "oidc" },
        });
      } else if (path === "/api/v1/spaces") {
        await route.fulfill({ json: { spaces: [] } });
      } else if (path === `/api/v1/repos/${username}`) {
        await route.fulfill({
          json: { repositories: [], role: "owner", canWrite: true },
        });
      } else if (path === `/api/v1/users/${username}/tokens`) {
        await route.fulfill({ json: { tokens: [] } });
      } else if (path === "/api/v1/logout") {
        expect(request.method()).toBe("POST");
        expect(request.headers()["x-csrf-token"]).toBe(csrfToken);
        logoutRequests += 1;
        if (logoutRequests === 1) {
          await route.fulfill({ status: 503, json: { detail: logoutError } });
          return;
        }
        authenticated = false;
        await route.fulfill({ status: 204 });
      } else {
        await route.fulfill({ status: 404, json: { detail: `Unexpected endpoint: ${path}` } });
      }
    });
    await page.setViewportSize({ width, height: 812 });
    await page.goto("/");
    const banner = page.getByRole("banner");
    const account = accountControl(page);
    const spaces = banner.getByRole("link", { name: "Your spaces", exact: true });
    const settings = banner.getByRole("link", { name: "Settings", exact: true });
    await expect(account).toBeVisible();
    await expect(account).toHaveAccessibleName(`Account: ${username}`);
    await expect(account).toHaveAttribute("aria-expanded", "false");
    await expect(settings).toBeHidden();
    await expect(banner.getByRole("link", { name: /New (repository|group)/ })).toHaveCount(0);
    await expect(banner.getByRole("combobox", { name: "Theme", exact: true })).toBeVisible();

    await test.step("open by keyboard and return focus on Escape", async () => {
      await account.focus();
      await page.keyboard.press("Enter");
      await expect(account).toHaveAttribute("aria-expanded", "true");
      const controlledId = await account.getAttribute("aria-controls");
      expect(controlledId).toBeTruthy();
      await expect(page.locator(`[id="${controlledId}"]`)).toBeVisible();
      await expect(banner.getByRole("navigation", { name: "Account navigation" })
        .getByRole("link")).toHaveText(["Your spaces", "Settings"]);
      await page.keyboard.press("Tab");
      await expect(spaces).toBeFocused();
      await page.keyboard.press("Escape");
      await expect(settings).toBeHidden();
      await expect(account).toHaveAttribute("aria-expanded", "false");
      await expect(account).toBeFocused();
    });

    await test.step("dismiss on an outside click", async () => {
      await openAccountMenu(page);
      await page.getByRole("heading", { name: "Your spaces", exact: true })
        .click({ position: { x: 4, y: 4 } });
      await expect(account).toHaveAttribute("aria-expanded", "false");
      await expect(settings).toBeHidden();
    });

    await test.step("reach account destinations from the compact header", async () => {
      await openAccountMenu(page);
      const menuBox = await settings.boundingBox();
      expect(menuBox).not.toBeNull();
      expect(menuBox!.x).toBeGreaterThanOrEqual(0);
      expect(menuBox!.x + menuBox!.width).toBeLessThanOrEqual(width);
      await settings.focus();
      await page.keyboard.press("Enter");
      await expect(page).toHaveURL(/\/auth\/tokens$/);
      await expect(page.getByRole("heading", { name: "Access tokens", exact: true })).toBeVisible();
      await expect(account).toHaveAttribute("aria-expanded", "false");
      await openAccountMenu(page);
      await spaces.click();
      await expect(page).toHaveURL(/\/$/);
      await expect(account).toHaveAttribute("aria-expanded", "false");
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    });

    await test.step("recover from a failed sign-out without obscuring the error", async () => {
      await openAccountMenu(page);
      await banner.getByRole("button", { name: "Sign out", exact: true }).click();
      await expect(page.getByRole("alert")).toHaveText(logoutError);
      await expect(page.getByRole("alert")).toBeInViewport();
      await expect(account).toHaveAttribute("aria-expanded", "false");
      await expect(account).toBeFocused();
      await expect(settings).toBeHidden();
      expect(logoutRequests).toBe(1);
    });

    await test.step("retry sign-out through the disclosure", async () => {
      await openAccountMenu(page);
      await banner.getByRole("button", { name: "Sign out", exact: true }).click();
      await expect(page).toHaveURL(/\/auth\/login\?signedOut=1$/);
      await expect(banner.getByRole("link", { name: "Sign in", exact: true })).toBeVisible();
      await expect(account).toHaveCount(0);
      expect(logoutRequests).toBe(2);
    });
    expect(errors).toEqual([]);
  });
}
