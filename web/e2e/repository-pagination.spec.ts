import { expect, test, type Page } from "@playwright/test";

const username = "catalog-user";

function repository(name: string) {
  return {
    id: name,
    namespace: username,
    name,
    description: "",
    defaultBranch: "main",
    visibility: "private",
    empty: false,
    role: "owner",
    canWrite: true,
    createdAt: "2026-01-01T00:00:00Z",
    createdBy: username,
  };
}

async function mockSession(page: Page) {
  await page.route("**/api/v1/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/v1/session") {
      await route.fulfill({ json: {
        authenticated: true, username, csrfToken: "catalog-csrf",
        shardCount: 1, provider: "oidc",
      } });
    } else if (path === "/api/v1/spaces") {
      await route.fulfill({ json: { spaces: [] } });
    } else if (path === `/api/v1/users/${username}/tokens`) {
      await route.fulfill({ json: { tokens: [] } });
    } else {
      await route.fulfill({ status: 404, json: { detail: "Not found" } });
    }
  });
}

test("repository catalog loads later pages, preserves rows on failure, and deduplicates retries", async ({ page }) => {
  await mockSession(page);
  let fail = true;
  await page.route(`**/api/v1/repos/${username}*`, async (route) => {
    const after = new URL(route.request().url()).searchParams.get("after");
    if (!after) {
      await route.fulfill({ json: {
        repositories: [repository("alpha")], role: "owner", canWrite: true, nextCursor: "alpha",
      } });
    } else if (fail) {
      expect(after).toBe("alpha");
      await route.fulfill({ status: 503, json: { detail: "Catalog temporarily unavailable." } });
    } else {
      expect(after).toBe("alpha");
      await route.fulfill({ json: {
        repositories: [repository("alpha"), repository("omega")], role: "owner", canWrite: true,
      } });
    }
  });
  await page.goto("/");
  const catalog = page.getByRole("region", { name: `Repositories in ${username}` });
  await expect(catalog.getByRole("link", { name: "alpha", exact: true })).toBeVisible();
  await expect(catalog.getByRole("link", { name: "omega", exact: true })).toHaveCount(0);
  await catalog.getByRole("button", { name: "Load more repositories", exact: true }).click();
  await expect(catalog.getByRole("alert")).toContainText("Catalog temporarily unavailable");
  await expect(catalog.getByRole("link", { name: "alpha", exact: true })).toBeVisible();
  fail = false;
  await catalog.getByRole("button", { name: "Retry loading more repositories", exact: true }).click();
  await expect(catalog.getByRole("link", { name: "omega", exact: true })).toBeVisible();
  await expect(catalog.getByRole("link", { name: "alpha", exact: true })).toHaveCount(1);
  await expect(catalog.getByRole("button", { name: "Load more repositories", exact: true })).toHaveCount(0);
  await expect(catalog.getByRole("alert")).toHaveCount(0);
});

test("token repository scopes follow every catalog page and publish the complete deduplicated result", async ({ page }) => {
  await mockSession(page);
  let finish!: () => void;
  const pending = new Promise<void>((resolve) => { finish = resolve; });
  let requestedLastPage = false;
  await page.route(`**/api/v1/repos/${username}*`, async (route) => {
    const after = new URL(route.request().url()).searchParams.get("after");
    if (!after) {
      await route.fulfill({ json: {
        repositories: [repository("alpha")], role: "owner", canWrite: true, nextCursor: "alpha",
      } });
    } else {
      expect(after).toBe("alpha");
      requestedLastPage = true;
      await pending;
      await route.fulfill({ json: {
        repositories: [repository("alpha"), repository("omega")], role: "owner", canWrite: true,
      } });
    }
  });
  try {
    await page.goto("/auth/tokens");
    await page.getByRole("button", { name: "Generate new token", exact: true }).click();
    await expect.poll(() => requestedLastPage).toBe(true);
    await expect(page.getByRole("checkbox", { name: `${username}/alpha`, exact: true })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Generate token", exact: true })).toBeDisabled();
    finish();
    await expect(page.getByRole("checkbox", { name: `${username}/alpha`, exact: true })).toHaveCount(1);
    await expect(page.getByRole("checkbox", { name: `${username}/omega`, exact: true })).toBeVisible();
    await expect(page.getByRole("alert")).toHaveCount(0);
  } finally {
    finish();
  }
});

test("token repository scopes reject repeated pagination cursors", async ({ page }) => {
  await mockSession(page);
  await page.route(`**/api/v1/repos/${username}*`, (route) => route.fulfill({ json: {
    repositories: [repository("alpha")], role: "owner", canWrite: true, nextCursor: "alpha",
  } }));
  await page.goto("/auth/tokens");
  await page.getByRole("button", { name: "Generate new token", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("repeated pagination cursor");
  await expect(page.getByRole("checkbox", { name: `${username}/alpha`, exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Generate token", exact: true })).toBeDisabled();
});

test("closing the token form aborts a pending repository page", async ({ page }) => {
  await mockSession(page);
  let finish!: () => void;
  const pending = new Promise<void>((resolve) => { finish = resolve; });
  let requestedLastPage = false;
  await page.route(`**/api/v1/repos/${username}*`, async (route) => {
    const after = new URL(route.request().url()).searchParams.get("after");
    if (!after) {
      await route.fulfill({ json: {
        repositories: [repository("alpha")], role: "owner", canWrite: true, nextCursor: "alpha",
      } });
    } else {
      requestedLastPage = true;
      await pending;
      await route.fulfill({ status: 503, json: { detail: "Canceled catalog failure." } });
    }
  });
  try {
    await page.goto("/auth/tokens");
    await page.getByRole("button", { name: "Generate new token", exact: true }).click();
    await expect.poll(() => requestedLastPage).toBe(true);
    const canceled = page.waitForEvent("requestfailed", {
      predicate: (request) => new URL(request.url()).searchParams.get("after") === "alpha",
    });
    await page.getByRole("button", { name: "Cancel", exact: true }).click();
    await canceled;
    finish();
    await expect(page.getByRole("alert")).toHaveCount(0);
    await expect(page.getByRole("region", { name: "Generate access token" })).toHaveCount(0);
  } finally {
    finish();
  }
});
