import {
  expect,
  test,
  type BrowserContext,
  type Locator,
  type Page,
} from "@playwright/test";

const themeKey = "gitone.theme";
const username = "theme-user";
const namespace = "theme-team";
const commit = "a".repeat(40);
const repository = {
  id: "project",
  namespace,
  name: "project",
  description: "A shared project with readable code in either theme.",
  defaultBranch: "main",
  createdAt: "2026-01-01T00:00:00Z",
  createdBy: username,
  visibility: "private",
  empty: false,
  role: "owner",
  canWrite: true,
};

async function mockAPI(
  target: Page | BrowserContext,
  authenticated = false,
) {
  await target.route("**/api/v1/**", async (route) => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    let json: unknown;
    if (path === "/api/v1/session") {
      json = {
        authenticated,
        ...(authenticated
          ? { username, identity: { email: "alice@example.test" } }
          : {}),
        csrfToken: "theme-test-csrf",
        shardCount: 4,
        provider: "oidc",
        sshURL: "ssh://gitone.localhost:2222",
      };
    } else if (path === "/api/v1/spaces") {
      json = {
        spaces:
          url.searchParams.get("shard") === "0"
            ? [{ name: namespace, type: "group", role: "owner", invited: false }]
            : [],
      };
    } else if (path === `/api/v1/users/${username}/ssh-keys`) {
      json = {
        keys: [
          {
            id: "b".repeat(64),
            name: "Work laptop",
            publicKey: `ssh-ed25519 ${"A".repeat(68)} fixture-public-key`,
            fingerprint: `SHA256:${"b".repeat(43)}`,
            createdAt: "2026-01-01T00:00:00Z",
          },
        ],
      };
    } else if (path === `/api/v1/users/${username}/tokens`) {
      json = { tokens: [] };
    } else if (
      path === `/api/v1/repos/${username}` ||
      path === `/api/v1/repos/${namespace}`
    ) {
      json = { repositories: [repository], role: "owner", canWrite: true };
    } else if (path === `/api/v1/repos/${namespace}/project`) {
      json = repository;
    } else if (path === `/api/v1/repos/${namespace}/project/branches`) {
      json = { branches: [{ name: "main", commit }] };
    } else if (path === `/api/v1/repos/${namespace}/project/tree`) {
      json = {
        ref: "main",
        path: "",
        commit,
        entries: [{ name: "README.md", path: "README.md", type: "file", size: 160 }],
      };
    } else if (path === `/api/v1/repos/${namespace}/project/blob`) {
      json = {
        ref: "main",
        path: "README.md",
        commit,
        content: `# Project\n\nA readable README.\n${"long-code-example-".repeat(20)}\n`,
        size: 160,
        binary: false,
      };
    } else {
      await route.fulfill({
        status: 404,
        json: { detail: `Unexpected theme fixture endpoint: ${path}` },
      });
      return;
    }
    await route.fulfill({ json });
  });
}

function themeControl(page: Page) {
  return page.getByRole("banner").getByRole("combobox", { name: "Theme", exact: true });
}

async function expectTheme(page: Page, effective: "light" | "dark") {
  await expect(page.locator("html")).toHaveAttribute("data-theme", effective);
  await expect(page.locator("html")).toHaveCSS("color-scheme", effective);
}

async function expectHeaderThemePlacement(page: Page) {
  await expect(page.getByRole("banner")).toHaveCount(1);
  await expect(themeControl(page)).toHaveCount(1);
  await expect(page.getByRole("combobox", { name: "Theme", exact: true, includeHidden: true })).toHaveCount(1);
  await expect(page.locator("footer").getByRole("combobox", { name: "Theme", exact: true, includeHidden: true })).toHaveCount(0);
}

async function expectHeaderControlReachable(page: Page, control: Locator) {
  await expect(control).toBeVisible();
  await expect(control).toBeEnabled();
  const box = await control.boundingBox();
  const viewport = page.viewportSize();
  expect(box, `Missing layout box for ${control}`).not.toBeNull();
  expect(viewport).not.toBeNull();
  expect(box!.x, `${control} left edge`).toBeGreaterThanOrEqual(-1);
  expect(box!.y, `${control} top edge`).toBeGreaterThanOrEqual(-1);
  expect(box!.x + box!.width, `${control} right edge`).toBeLessThanOrEqual(viewport!.width + 1);
  expect(box!.y + box!.height, `${control} bottom edge`).toBeLessThanOrEqual(viewport!.height + 1);
  // Trial actions verify hit-testing without navigating or signing out.
  await control.click({ trial: true });
  await control.focus();
  await expect(control).toBeFocused();
  await expectReadable(control);
}

async function expectReadable(locator: Locator, darkSurface = false) {
  await expect(locator).toBeAttached();
  // The root canvas is painted even when the blocked app leaves html with no
  // layout height. All real UI components must also have visible geometry.
  if (await locator.evaluate((element) => element.tagName !== "HTML"))
    await expect(locator).toBeVisible();
  const colors = await locator.evaluate((element) => {
    const components = (value: string) => value.match(/[\d.]+/g)?.map(Number) ?? [];
    const foreground = components(getComputedStyle(element).color);
    let current: Element | null = element;
    let background = [255, 255, 255];
    while (current) {
      const candidate = components(getComputedStyle(current).backgroundColor);
      if (candidate.length === 3 || candidate[3] === 1) {
        background = candidate.slice(0, 3);
        break;
      }
      current = current.parentElement;
    }
    return { foreground: foreground.slice(0, 3), background };
  });
  const luminance = (rgb: number[]) =>
    rgb
      .map((channel) => {
        const value = channel / 255;
        return value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4;
      })
      .reduce((total, value, index) => total + value * [0.2126, 0.7152, 0.0722][index], 0);
  const foreground = luminance(colors.foreground);
  const background = luminance(colors.background);
  const ratio = (Math.max(foreground, background) + 0.05) / (Math.min(foreground, background) + 0.05);
  expect(ratio, `Text contrast for ${locator}: ${JSON.stringify(colors)}`).toBeGreaterThanOrEqual(4.5);
  if (darkSurface)
    expect(background, `Expected a dark surface for ${locator}`).toBeLessThan(0.15);
}

for (const authenticated of [false, true]) {
  for (const mode of ["light", "dark"] as const) {
    test(`${authenticated ? "authenticated" : "public"} top menu keeps the theme selector reachable in ${mode} mode`, async ({ page }) => {
      await mockAPI(page, authenticated);
      await page.emulateMedia({ colorScheme: mode, reducedMotion: "reduce" });
      await page.goto(authenticated ? "/" : "/auth/login");
      await expect(page.getByRole("heading", {
        name: authenticated ? "Your spaces" : "Sign in to GitOne",
        exact: true,
      })).toBeVisible();
      const banner = page.getByRole("banner");
      for (const width of [320, 375, 768, 1024, 1280]) {
        await test.step(`${width}px viewport`, async () => {
          await page.setViewportSize({ width, height: 812 });
          await page.evaluate(() => window.scrollTo(0, 0));
          await expectHeaderThemePlacement(page);
          await expectTheme(page, mode);
          expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1)).toBe(true);
          await expectHeaderControlReachable(page, themeControl(page));
          if (authenticated) {
            await expectHeaderControlReachable(page, banner.getByRole("link", { name: "Settings", exact: true }));
            await expectHeaderControlReachable(page, banner.getByRole("button", { name: "Sign out", exact: true }));
          } else {
            await expectHeaderControlReachable(page, banner.getByRole("link", { name: "Sign in", exact: true }));
            await expectHeaderControlReachable(page, banner.getByRole("link", { name: "Create account", exact: true }));
          }
        });
      }
    });
  }
}

test("top menu theme selector remains usable while session loads and after an API error", async ({ page }) => {
  await mockAPI(page);
  await page.setViewportSize({ width: 320, height: 812 });
  await page.emulateMedia({ colorScheme: "light" });
  let releaseSession!: () => void;
  const pendingSession = new Promise<void>((resolve) => { releaseSession = resolve; });
  await page.route("**/api/v1/session", async (route) => {
    await pendingSession;
    await route.fulfill({ status: 503, json: { detail: "Session service is temporarily unavailable." } });
  });
  try {
    await page.goto("/auth/login");
    await expect(page.getByRole("status")).toContainText("Connecting to GitOne");
    await expectHeaderThemePlacement(page);
    await expectHeaderControlReachable(page, themeControl(page));
    await themeControl(page).selectOption("dark");
    await expectTheme(page, "dark");
    releaseSession();
    await expect(page.getByRole("heading", { name: "GitOne is unavailable", exact: true })).toBeVisible();
    await expectHeaderThemePlacement(page);
    await expect(themeControl(page)).toHaveValue("dark");
    await expectHeaderControlReachable(page, themeControl(page));
    await themeControl(page).selectOption("light");
    await expectTheme(page, "light");
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1)).toBe(true);
  } finally {
    releaseSession();
  }
});

test("theme defaults to system and tracks operating-system changes", async ({ page }) => {
  await mockAPI(page);
  await page.emulateMedia({ colorScheme: "light" });
  await page.goto("/auth/login");
  const theme = themeControl(page);
  await expect(theme).toHaveJSProperty("tagName", "SELECT");
  await expect(theme).toHaveValue("system");
  await expect(theme.locator("option")).toHaveText(["System", "Light", "Dark"]);
  await expectTheme(page, "light");
  await page.emulateMedia({ colorScheme: "dark" });
  await expectTheme(page, "dark");
  await expect(theme).toHaveValue("system");
  await page.emulateMedia({ colorScheme: "light" });
  await expectTheme(page, "light");
  await theme.focus();
  await expect(theme).toBeFocused();
  await page.keyboard.press("End");
  await page.keyboard.press("Enter");
  await expect(theme).toHaveValue("dark");
  await expectTheme(page, "dark");
});

test("explicit preference survives reload and navigation independently of the OS", async ({ page }) => {
  await mockAPI(page);
  await page.emulateMedia({ colorScheme: "light" });
  await page.goto("/auth/login");
  await themeControl(page).selectOption("dark");
  await expectTheme(page, "dark");
  expect(await page.evaluate((key) => localStorage.getItem(key), themeKey)).toBe("dark");
  await page.reload();
  await expectTheme(page, "dark");
  await expect(themeControl(page)).toHaveValue("dark");
  await page.getByRole("link", { name: "Create an account", exact: true }).click();
  await expect(page).toHaveURL(/\/auth\/register$/);
  await expectTheme(page, "dark");
  await expect(themeControl(page)).toHaveValue("dark");
  await page.emulateMedia({ colorScheme: "dark" });
  await themeControl(page).selectOption("light");
  await expectTheme(page, "light");
  await page.emulateMedia({ colorScheme: "light" });
  await page.emulateMedia({ colorScheme: "dark" });
  await expectTheme(page, "light");
  await page.reload();
  await expectTheme(page, "light");
  await themeControl(page).selectOption("system");
  await expectTheme(page, "dark");
});

for (const saved of ["light", "dark", "invalid-theme"]) {
  test(`initial stored preference ${saved} is validated before display`, async ({ page }) => {
    await mockAPI(page);
    await page.emulateMedia({ colorScheme: "dark" });
    await page.addInitScript(({ key, value }) => localStorage.setItem(key, value), {
      key: themeKey,
      value: saved,
    });
    await page.goto("/auth/login");
    await expectTheme(page, saved === "light" ? "light" : "dark");
    await expect(themeControl(page)).toHaveValue(saved === "invalid-theme" ? "system" : saved);
  });
}

test("theme selection syncs between tabs including removed and cleared storage", async ({ page, context }) => {
  await mockAPI(context);
  await page.emulateMedia({ colorScheme: "light" });
  const other = await context.newPage();
  await other.emulateMedia({ colorScheme: "light" });
  await Promise.all([page.goto("/auth/login"), other.goto("/auth/login")]);
  await themeControl(page).selectOption("dark");
  await expectTheme(other, "dark");
  await expect(themeControl(other)).toHaveValue("dark");
  await themeControl(other).selectOption("light");
  await expectTheme(page, "light");
  await expect(themeControl(page)).toHaveValue("light");
  await themeControl(other).selectOption("dark");
  await expectTheme(page, "dark");
  await other.evaluate((key) => localStorage.removeItem(key), themeKey);
  await expectTheme(page, "light");
  await expect(themeControl(page)).toHaveValue("system");
  await other.reload();
  await themeControl(other).selectOption("dark");
  await expectTheme(page, "dark");
  await other.evaluate(() => localStorage.clear());
  await expectTheme(page, "light");
  await expect(themeControl(page)).toHaveValue("system");
  await other.reload();
  await themeControl(page).selectOption("dark");
  await expectTheme(other, "dark");
  await other.evaluate(() => localStorage.setItem("unrelated-setting", "light"));
  await expectTheme(page, "dark");
  await other.close();
});

test("queued storage events respect the latest stored theme preference", async ({ page }) => {
  await mockAPI(page);
  await page.emulateMedia({ colorScheme: "light" });
  await page.goto("/auth/login");
  await themeControl(page).selectOption("dark");
  await expectTheme(page, "dark");
  for (const stale of [
    { key: themeKey, newValue: "light" },
    { key: themeKey, newValue: null },
    { key: null, newValue: null },
  ]) {
    // A delayed save, removal or clear event must not overwrite a newer
    // selection whose value is already present in shared localStorage.
    await page.evaluate((event) => {
      window.dispatchEvent(new StorageEvent("storage", {
        ...event,
        oldValue: "system",
        storageArea: localStorage,
        url: location.href,
      }));
    }, stale);
    await expectTheme(page, "dark");
    await expect(themeControl(page)).toHaveValue("dark");
    expect(await page.evaluate((key) => localStorage.getItem(key), themeKey)).toBe("dark");
  }
  // If a different tab has saved yet another value, consume that current
  // value even when the delivered event describes an older write.
  await page.evaluate((key) => {
    localStorage.setItem(key, "light");
    window.dispatchEvent(new StorageEvent("storage", {
      key,
      oldValue: "system",
      newValue: "dark",
      storageArea: localStorage,
      url: location.href,
    }));
  }, themeKey);
  await expectTheme(page, "light");
  await expect(themeControl(page)).toHaveValue("light");
});

for (const blocked of ["access", "write"]) {
  test(`unavailable localStorage ${blocked} does not break appearance controls`, async ({ page }) => {
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await mockAPI(page);
    await page.emulateMedia({ colorScheme: "dark" });
    await page.addInitScript((failure) => {
      const fail = () => { throw new DOMException("Storage is unavailable", "SecurityError"); };
      if (failure === "access")
        Object.defineProperty(window, "localStorage", { configurable: true, get: fail });
      else {
        Storage.prototype.setItem = fail;
        Storage.prototype.removeItem = fail;
      }
    }, blocked);
    await page.goto("/auth/login");
    await expect(page.getByRole("heading", { name: "Sign in to GitOne", exact: true })).toBeVisible();
    await expectTheme(page, "dark");
    await themeControl(page).selectOption("light");
    await expectTheme(page, "light");
    await expect(themeControl(page)).toHaveValue("light");
    await page.emulateMedia({ colorScheme: "light" });
    await page.emulateMedia({ colorScheme: "dark" });
    await expectTheme(page, "light");
    await themeControl(page).selectOption("system");
    await expectTheme(page, "dark");
    await page.reload();
    await expectTheme(page, "dark");
    expect(errors).toEqual([]);
  });
}

test("saved dark appearance applies before the application module runs", async ({ page }) => {
  await mockAPI(page);
  await page.emulateMedia({ colorScheme: "light" });
  await page.addInitScript((key) => localStorage.setItem(key, "dark"), themeKey);
  await page.route("**/*", async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    if (request.resourceType() === "script" && !/\/theme-[\w-]+\.js$/.test(pathname))
      await route.abort();
    else await route.fallback();
  });
  await page.goto("/auth/login", { waitUntil: "domcontentloaded" });
  const boot = page.locator('head script:not([type="module"])');
  await expect(boot).toHaveAttribute("src", /\/gitone\/assets\/theme-[\w-]+\.js$/);
  await expect(boot).not.toHaveAttribute("defer", /.*/);
  await expect(boot).not.toHaveAttribute("async", /.*/);
  await expectTheme(page, "dark");
  await expect(page.locator("#root")).toBeEmpty();
  await expectReadable(page.locator("html"), true);
});

test("application theme controls recover when the early bootstrap request fails", async ({ page }) => {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await mockAPI(page);
  await page.emulateMedia({ colorScheme: "dark" });
  await page.addInitScript((key) => localStorage.setItem(key, "light"), themeKey);
  await page.route(/\/gitone\/assets\/theme-[\w-]+\.js$/, (route) => route.abort());
  await page.goto("/auth/login");
  await expect(page.getByRole("heading", { name: "Sign in to GitOne", exact: true })).toBeVisible();
  await expect(themeControl(page)).toHaveValue("light");
  await expectTheme(page, "light");
  await themeControl(page).selectOption("dark");
  await expectTheme(page, "dark");
  expect(errors).toEqual([]);
});

for (const mode of ["light", "dark"] as const) {
  test(`public sign-in colors and native fields remain readable in ${mode} mode`, async ({ page }) => {
    await mockAPI(page);
    await page.emulateMedia({ colorScheme: mode });
    await page.goto("/auth/login?signedOut=1&error=Test%20error%20message");
    await expectTheme(page, mode);
    for (const selector of ["html", ".auth-card", ".field-help", ".notice.success", ".notice.error"])
      await expectReadable(page.locator(selector).first(), mode === "dark");
    await expectReadable(page.getByLabel("Username", { exact: true }), mode === "dark");
    await expectReadable(themeControl(page), mode === "dark");
    await expectReadable(page.getByRole("button", { name: /continue with/i }));
    await expectReadable(page.getByRole("link", { name: "Create an account", exact: true }));
  });
}

test("dark dashboard, personal settings and repository code fit a 375px viewport", async ({ page }) => {
  await mockAPI(page, true);
  await page.setViewportSize({ width: 375, height: 812 });
  await page.emulateMedia({ colorScheme: "dark", reducedMotion: "reduce" });
  const noOverflow = async () => {
    await expectTheme(page, "dark");
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1)).toBe(true);
    await expectReadable(themeControl(page), true);
  };
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Your spaces", exact: true })).toBeVisible();
  await expect(page.locator(".repository-row")).toBeVisible();
  await expectReadable(page.locator(".personal-card"), true);
  await expectReadable(page.locator(".repository-row"), true);
  await noOverflow();
  await page.goto("/auth/ssh-keys");
  await expect(page.getByTestId("ssh-key-row")).toBeVisible();
  await page.getByText("View public key", { exact: true }).click();
  await expectReadable(page.getByTestId("ssh-key-row").locator("pre"), true);
  await expectReadable(page.getByText("Active", { exact: true }), true);
  await page.getByRole("button", { name: "Add SSH key", exact: true }).click();
  await expectReadable(page.getByLabel("Key name", { exact: true }), true);
  await expectReadable(page.getByLabel("Public key", { exact: true }), true);
  await page.getByRole("button", { name: "Save SSH key", exact: true }).click();
  await expectReadable(page.getByRole("alert"), true);
  await noOverflow();
  await page.goto("/auth/tokens");
  await expect(page.getByRole("heading", { name: "Access tokens", exact: true })).toBeVisible();
  await expectReadable(page.locator(".settings-aside"), true);
  await noOverflow();
  await page.goto(`/${namespace}/project`);
  await expect(page.getByRole("region", { name: "README", exact: true })).toBeVisible();
  await expectReadable(page.locator(".file-list"), true);
  await expectReadable(page.getByRole("region", { name: "README", exact: true }).locator("pre"), true);
  await expectReadable(page.getByLabel("Branch", { exact: true }), true);
  await page.locator(".clone-panel summary").click();
  await expectReadable(page.getByLabel("HTTPS clone URL", { exact: true }), true);
  await noOverflow();
});
