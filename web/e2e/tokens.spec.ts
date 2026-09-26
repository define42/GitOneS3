import { expect, test, type Page } from "@playwright/test";
import { openAccountMenu } from "./header-helpers";

// Token responses and the one-time reveal must never enter Playwright artifacts.
test.use({ trace: "off", screenshot: "off", video: "off" });
test.afterEach(async ({ page }) => {
  await page
    .evaluate(() => {
      const input =
        document.querySelector<HTMLInputElement>("#new-access-token");
      if (input) input.remove();
    })
    .catch(() => {});
});

async function register(
  page: Page,
  namespace: string,
  account: "alice" | "bob",
) {
  await page.goto("/auth/register");
  await page.getByLabel("Username", { exact: true }).fill(namespace);
  await page.getByRole("button", { name: /continue/i }).click();
  await page.waitForURL((url) => url.hostname.startsWith("keycloak."));
  await page.getByLabel("Username or email", { exact: true }).fill(account);
  await page
    .getByLabel("Password", { exact: true })
    .fill(`${account}-dev-password`);
  await page.getByRole("button", { name: "Sign In", exact: true }).click();
  await expect(page).toHaveURL(new RegExp(`/${namespace}/?$`));
}

async function session(page: Page) {
  return (await page.request.get("/api/v1/session")).json() as Promise<{
    csrfToken: string;
    userId: string;
  }>;
}

async function createRepository(page: Page, namespace: string, name: string) {
  const current = await session(page);
  const response = await page.request.post(`/api/v1/repos/${namespace}`, {
    headers: {
      Origin: new URL(page.url()).origin,
      "X-CSRF-Token": current.csrfToken,
    },
    data: { name, initializeReadme: true },
  });
  expect(response.status()).toBe(201);
}

test("generate scoped tokens, use Git authentication, hide secrets, and revoke", async ({
  page,
  context,
  playwright,
  baseURL,
}) => {
  test.setTimeout(120000);
  const username = `pat-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`;
  let createdIds: string[] = [];
  await register(page, username, "alice");
  const current = await session(page);
  const origin = new URL(baseURL!).origin;
  try {
    await createRepository(page, username, "selected-project");
    await createRepository(page, username, "other-project");
    await context.grantPermissions(["clipboard-read", "clipboard-write"]);
    await page.goto("/auth/tokens");
    await page
      .getByRole("button", { name: "Generate new token", exact: true })
      .click();
    await expect(page.getByLabel("Permission", { exact: true })).toHaveValue(
      "read",
    );
    await expect(page.getByLabel("Expiration", { exact: true })).toHaveValue(
      "30",
    );
    await page.getByLabel("Token name", { exact: true }).fill("Work laptop");
    await expect(
      page.getByRole("checkbox", {
        name: `${username}/selected-project`,
        exact: true,
      }),
    ).toBeVisible();
    await page
      .getByRole("button", { name: "Generate token", exact: true })
      .click();
    await expect(page.getByRole("alert")).toContainText(
      "Select at least one repository",
    );
    await expect(page.getByRole("alert")).toBeFocused();
    await page
      .getByRole("checkbox", {
        name: `${username}/selected-project`,
        exact: true,
      })
      .check();
    await page.evaluate(() =>
      navigator.clipboard.writeText("unchanged-until-copy"),
    );
    await page
      .getByRole("button", { name: "Generate token", exact: true })
      .click();
    let secret = await page
      .getByLabel("Your new access token", { exact: true })
      .inputValue();
    const didNotAutoCopy =
      (await page.evaluate(() => navigator.clipboard.readText())) ===
      "unchanged-until-copy";
    await page.getByRole("button", { name: "Copy token", exact: true }).click();
    await page
      .getByText("Token copied. Store it securely, then close this message.", {
        exact: true,
      })
      .waitFor();
    const copiedCorrectly =
      (await page.evaluate(() => navigator.clipboard.readText())) === secret;
    await page
      .getByRole("button", { name: "I’ve saved it — close", exact: true })
      .click();
    // Assert only booleans after hiding the secret; failure output must not print it.
    expect(secret.length > 20).toBe(true);
    expect(didNotAutoCopy).toBe(true);
    expect(copiedCorrectly).toBe(true);
    await page.evaluate(() => navigator.clipboard.writeText(""));
    await expect(
      page.getByLabel("Your new access token", { exact: true }),
    ).toHaveCount(0);
    await page.reload();
    await expect(
      page.getByTestId("token-row").filter({ hasText: "Work laptop" }),
    ).toBeVisible();
    const listing = await (
      await page.request.get(`/api/v1/users/${username}/tokens`)
    ).json();
    createdIds = listing.tokens.map((token: { id: string }) => token.id);
    expect(JSON.stringify(listing).includes(secret)).toBe(false);
    expect(
      listing.tokens.some((token: Record<string, unknown>) =>
        Object.keys(token).some((key) =>
          /secret|hash|digest|^token$/i.test(key),
        ),
      ),
    ).toBe(false);
    expect((await page.content()).includes(secret)).toBe(false);
    expect(page.url().includes(secret)).toBe(false);
    const persisted = await page.evaluate(
      (value) =>
        JSON.stringify(localStorage).includes(value) ||
        JSON.stringify(sessionStorage).includes(value),
      secret,
    );
    expect(persisted).toBe(false);

    const git = await playwright.request.newContext({
      baseURL,
      ignoreHTTPSErrors: true,
      extraHTTPHeaders: {
        Authorization: `Basic ${Buffer.from(`${username}:${secret}`).toString("base64")}`,
      },
    });
    try {
      expect(
        (
          await git.get(
            `/${username}/selected-project.git/info/refs?service=git-upload-pack`,
          )
        ).status(),
      ).toBe(200);
      expect(
        (
          await git.get(
            `/${username}/other-project.git/info/refs?service=git-upload-pack`,
          )
        ).status(),
      ).toBe(403);
      expect(
        (
          await git.get(
            `/${username}/selected-project.git/info/refs?service=git-receive-pack`,
          )
        ).status(),
      ).toBe(403);
      const row = page
        .getByTestId("token-row")
        .filter({ hasText: "Work laptop" });
      page.once("dialog", (dialog) => dialog.dismiss());
      await row.getByRole("button", { name: "Revoke", exact: true }).click();
      await expect(row.getByText("Active", { exact: true })).toBeVisible();
      page.once("dialog", (dialog) => dialog.accept());
      await row.getByRole("button", { name: "Revoke", exact: true }).click();
      await expect(row.getByText("Revoked", { exact: true })).toBeVisible();
      expect(
        (
          await git.get(
            `/${username}/selected-project.git/info/refs?service=git-upload-pack`,
          )
        ).status(),
      ).toBe(401);
    } finally {
      await git.dispose();
    }
    secret = "";

    await page
      .getByRole("button", { name: "Generate new token", exact: true })
      .click();
    await page.getByLabel("Token name", { exact: true }).fill("Two projects");
    await page.getByLabel("Permission", { exact: true }).selectOption("write");
    await page.getByLabel("Expiration", { exact: true }).selectOption("7");
    await page
      .getByRole("checkbox", {
        name: `${username}/selected-project`,
        exact: true,
      })
      .check();
    await page
      .getByRole("checkbox", { name: `${username}/other-project`, exact: true })
      .check();
    await page
      .getByRole("button", { name: "Generate token", exact: true })
      .click();
    await page.getByLabel("Your new access token", { exact: true }).waitFor();
    // Navigate away without closing the reveal, then return through browser history.
    await openAccountMenu(page);
    await page
      .getByRole("banner")
      .getByRole("link", { name: "Your spaces", exact: true })
      .click();
    await expect(page).toHaveURL(`${baseURL}/`);
    await page.goBack();
    await expect(page).toHaveURL(`${baseURL}/auth/tokens`);
    await expect(
      page.getByLabel("Your new access token", { exact: true }),
    ).toHaveCount(0);
    const secondRow = page
      .getByTestId("token-row")
      .filter({ hasText: "Two projects" });
    await expect(secondRow).toContainText("Read and write");
    await expect(secondRow).toContainText("2 selected repositories");
    await page.reload();
    await expect(
      page.getByLabel("Your new access token", { exact: true }),
    ).toHaveCount(0);
    await page.setViewportSize({ width: 375, height: 812 });
    await expect(secondRow).toBeVisible();
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
    await page.goto(`/${username}/selected-project`);
    await page.getByText("Clone with HTTPS", { exact: true }).click();
    await expect(
      page.getByLabel("HTTPS clone URL", { exact: true }),
    ).toHaveValue(`${baseURL}/${username}/selected-project.git`);
    expect(
      await page
        .getByLabel("HTTPS clone URL", { exact: true })
        .inputValue()
        .then((url) => url.includes("@")),
    ).toBe(false);
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
  } finally {
    await page
      .evaluate(() => {
        const input =
          document.querySelector<HTMLInputElement>("#new-access-token");
        if (input) input.remove();
      })
      .catch(() => {});
    const response = await page.request.get(`/api/v1/users/${username}/tokens`);
    if (response.ok())
      createdIds = (await response.json()).tokens.map(
        (token: { id: string }) => token.id,
      );
    for (const id of createdIds) {
      await page.request.delete(`/api/v1/users/${username}/tokens/${id}`, {
        headers: { Origin: origin, "X-CSRF-Token": current.csrfToken },
      });
    }
  }
});

test("read-only group repositories cannot be selected for a write token", async ({
  page,
  browser,
  baseURL,
}) => {
  const suffix = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 5)}`;
  const ownerName = `pat-owner-${suffix}`;
  const readerName = `pat-reader-${suffix}`;
  const group = `pat-team-${suffix}`;
  const readerContext = await browser.newContext({
    baseURL,
    ignoreHTTPSErrors: true,
  });
  const reader = await readerContext.newPage();
  try {
    await register(page, ownerName, "alice");
    await register(reader, readerName, "bob");
    const owner = await session(page);
    const member = await session(reader);
    const ownerHeaders = {
      Origin: new URL(baseURL!).origin,
      "X-CSRF-Token": owner.csrfToken,
    };
    const readerHeaders = {
      Origin: new URL(baseURL!).origin,
      "X-CSRF-Token": member.csrfToken,
    };
    expect(
      (
        await page.request.post(`/api/v1/groups/${group}`, {
          headers: ownerHeaders,
        })
      ).status(),
    ).toBe(201);
    await createRepository(page, group, "shared-project");
    expect(
      (
        await page.request.post(`/api/v1/groups/${group}/invitations`, {
          headers: ownerHeaders,
          data: { userId: member.userId, role: "reader" },
        })
      ).status(),
    ).toBe(200);
    expect(
      (
        await reader.request.post(
          `/api/v1/groups/${group}/invitations/accept`,
          { headers: readerHeaders },
        )
      ).status(),
    ).toBe(200);
    await reader.setViewportSize({ width: 375, height: 812 });
    await reader.goto("/auth/tokens");
    await reader
      .getByRole("button", { name: "Generate new token", exact: true })
      .click();
    const choice = reader.getByRole("checkbox", {
      name: `${group}/shared-project`,
      exact: true,
    });
    await expect(choice).toBeEnabled();
    await choice.check();
    await reader
      .getByLabel("Permission", { exact: true })
      .selectOption("write");
    await expect(choice).toBeDisabled();
    await expect(choice).not.toBeChecked();
    await expect(reader.getByRole("status")).toContainText(
      "Read-only repositories were removed",
    );
    expect(
      await reader.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
    await reader.getByLabel("Permission", { exact: true }).selectOption("read");
    await expect(choice).toBeEnabled();
  } finally {
    await readerContext.close();
  }
});

test("access-token settings require sign-in and preserve the destination", async ({
  page,
}) => {
  await page.goto("/auth/tokens");
  await expect(page).toHaveURL(/\/auth\/login\?returnTo=/);
  expect(new URL(page.url()).searchParams.get("returnTo")).toBe("/auth/tokens");
});

test("all-repository tokens cover future repositories but still enforce current access", async ({
  page,
  browser,
  playwright,
  baseURL,
}) => {
  test.setTimeout(120000);
  const suffix = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 5)}`;
  const username = `all-alice-${suffix}`;
  const otherUsername = `all-bob-${suffix}`;
  const group = `all-team-${suffix}`;
  const otherContext = await browser.newContext({
    baseURL,
    ignoreHTTPSErrors: true,
  });
  const other = await otherContext.newPage();
  const gitClients: Awaited<
    ReturnType<typeof playwright.request.newContext>
  >[] = [];
  await register(page, username, "alice");
  const current = await session(page);
  const origin = new URL(baseURL!).origin;
  const currentHeaders = { Origin: origin, "X-CSRF-Token": current.csrfToken };
  try {
    await register(other, otherUsername, "bob");
    const otherSession = await session(other);
    const otherHeaders = {
      Origin: origin,
      "X-CSRF-Token": otherSession.csrfToken,
    };
    expect(
      (await (await page.request.get(`/api/v1/repos/${username}`)).json())
        .repositories.length,
    ).toBe(0);
    await page.goto("/auth/tokens");
    // Repository discovery fails deliberately; all mode must not depend on that listing.
    await page.route("**/api/v1/spaces?*", (route) => route.abort("failed"));
    await page
      .getByRole("button", { name: "Generate new token", exact: true })
      .click();
    await expect(
      page.getByRole("radio", { name: "Selected repositories", exact: true }),
    ).toBeChecked();
    await expect(page.getByLabel("Permission", { exact: true })).toHaveValue(
      "read",
    );
    await expect(page.getByLabel("Expiration", { exact: true })).toHaveValue(
      "30",
    );
    await page
      .getByLabel("Token name", { exact: true })
      .fill("All repositories reader");
    await page
      .getByRole("radio", {
        name: "All repositories I have access to",
        exact: true,
      })
      .check();
    await expect(
      page.getByRole("radio", { name: "Selected repositories", exact: true }),
    ).not.toBeChecked();
    await expect(
      page.getByRole("button", { name: "Generate token", exact: true }),
    ).toBeEnabled();
    await expect(page.getByRole("note")).toContainText("future repositories");
    await expect(page.getByRole("checkbox")).toHaveCount(0);
    await page.setViewportSize({ width: 375, height: 812 });
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
    const readRequest = page.waitForRequest(
      (request) =>
        request.method() === "POST" &&
        new URL(request.url()).pathname === `/api/v1/users/${username}/tokens`,
    );
    await page
      .getByRole("button", { name: "Generate token", exact: true })
      .click();
    const readBody = (await readRequest).postDataJSON();
    let readSecret = await page
      .getByLabel("Your new access token", { exact: true })
      .inputValue();
    const allScopeShown =
      (await page
        .getByRole("region", { name: "New access token", exact: true })
        .getByText("All repositories I have access to", { exact: true })
        .count()) === 1;
    await page
      .getByRole("button", { name: "I’ve saved it — close", exact: true })
      .click();
    expect(allScopeShown).toBe(true);
    expect(readBody.allRepositories).toBe(true);
    expect(readBody.repositories.length).toBe(0);
    await page.unroute("**/api/v1/spaces?*");
    const readerGit = await playwright.request.newContext({
      baseURL,
      ignoreHTTPSErrors: true,
      extraHTTPHeaders: {
        Authorization: `Basic ${Buffer.from(`${username}:${readSecret}`).toString("base64")}`,
      },
    });
    gitClients.push(readerGit);
    readSecret = "";
    await page.reload();
    await expect(
      page
        .getByTestId("token-row")
        .filter({ hasText: "All repositories reader" })
        .getByText("All repositories I have access to", { exact: true }),
    ).toBeVisible();
    await createRepository(page, username, "future-project");
    await createRepository(other, otherUsername, "private-project");
    expect(
      (
        await readerGit.get(
          `/${username}/future-project.git/info/refs?service=git-upload-pack`,
        )
      ).status(),
    ).toBe(200);
    expect(
      (
        await readerGit.get(
          `/${username}/future-project.git/info/refs?service=git-receive-pack`,
        )
      ).status(),
    ).toBe(403);
    expect(
      (
        await readerGit.get(
          `/${otherUsername}/private-project.git/info/refs?service=git-upload-pack`,
        )
      ).status(),
    ).toBe(403);

    await page
      .getByRole("button", { name: "Generate new token", exact: true })
      .click();
    const selectedMode = page.getByRole("radio", {
      name: "Selected repositories",
      exact: true,
    });
    const allMode = page.getByRole("radio", {
      name: "All repositories I have access to",
      exact: true,
    });
    await expect(selectedMode).toBeChecked();
    await page
      .getByLabel("Token name", { exact: true })
      .fill("All repositories writer");
    const futureScope = page.getByRole("checkbox", {
      name: `${username}/future-project`,
      exact: true,
    });
    await expect(futureScope).toBeVisible();
    await allMode.check();
    await selectedMode.check();
    await page
      .getByRole("button", { name: "Generate token", exact: true })
      .click();
    await expect(page.getByRole("alert")).toContainText(
      "Select at least one repository",
    );
    await futureScope.check();
    await page.getByLabel("Permission", { exact: true }).selectOption("write");
    await allMode.check();
    await expect(page.getByRole("alert")).toHaveCount(0);
    const writeRequest = page.waitForRequest(
      (request) =>
        request.method() === "POST" &&
        new URL(request.url()).pathname === `/api/v1/users/${username}/tokens`,
    );
    await page
      .getByRole("button", { name: "Generate token", exact: true })
      .click();
    const writeBody = (await writeRequest).postDataJSON();
    let writeSecret = await page
      .getByLabel("Your new access token", { exact: true })
      .inputValue();
    await page
      .getByRole("button", { name: "I’ve saved it — close", exact: true })
      .click();
    expect(writeBody.allRepositories).toBe(true);
    expect(writeBody.repositories.length).toBe(0);
    const writerGit = await playwright.request.newContext({
      baseURL,
      ignoreHTTPSErrors: true,
      extraHTTPHeaders: {
        Authorization: `Basic ${Buffer.from(`${username}:${writeSecret}`).toString("base64")}`,
      },
    });
    gitClients.push(writerGit);
    writeSecret = "";
    expect(
      (
        await writerGit.get(
          `/${username}/future-project.git/info/refs?service=git-receive-pack`,
        )
      ).status(),
    ).toBe(200);
    expect(
      (
        await writerGit.get(
          `/${otherUsername}/private-project.git/info/refs?service=git-upload-pack`,
        )
      ).status(),
    ).toBe(403);

    // These tokens also predate the shared group and its membership grant.
    expect(
      (
        await other.request.post(`/api/v1/groups/${group}`, {
          headers: otherHeaders,
        })
      ).status(),
    ).toBe(201);
    await createRepository(other, group, "shared-project");
    expect(
      (
        await writerGit.get(
          `/${group}/shared-project.git/info/refs?service=git-upload-pack`,
        )
      ).status(),
    ).toBe(403);
    expect(
      (
        await other.request.post(`/api/v1/groups/${group}/invitations`, {
          headers: otherHeaders,
          data: { userId: current.userId, role: "reader" },
        })
      ).status(),
    ).toBe(200);
    expect(
      (
        await page.request.post(`/api/v1/groups/${group}/invitations/accept`, {
          headers: currentHeaders,
        })
      ).status(),
    ).toBe(200);
    expect(
      (
        await readerGit.get(
          `/${group}/shared-project.git/info/refs?service=git-upload-pack`,
        )
      ).status(),
    ).toBe(200);
    expect(
      (
        await writerGit.get(
          `/${group}/shared-project.git/info/refs?service=git-upload-pack`,
        )
      ).status(),
    ).toBe(200);
    expect(
      (
        await writerGit.get(
          `/${group}/shared-project.git/info/refs?service=git-receive-pack`,
        )
      ).status(),
    ).toBe(403);
    expect(
      (
        await other.request.put(`/api/v1/groups/${group}/members`, {
          headers: otherHeaders,
          data: { userId: current.userId, role: "developer" },
        })
      ).status(),
    ).toBe(200);
    expect(
      (
        await writerGit.get(
          `/${group}/shared-project.git/info/refs?service=git-receive-pack`,
        )
      ).status(),
    ).toBe(200);
    expect(
      (
        await readerGit.get(
          `/${group}/shared-project.git/info/refs?service=git-receive-pack`,
        )
      ).status(),
    ).toBe(403);
    expect(
      (
        await other.request.delete(`/api/v1/groups/${group}/members`, {
          headers: otherHeaders,
          data: { userId: current.userId },
        })
      ).status(),
    ).toBe(200);
    expect(
      (
        await writerGit.get(
          `/${group}/shared-project.git/info/refs?service=git-upload-pack`,
        )
      ).status(),
    ).toBe(403);
    const readerRow = page
      .getByTestId("token-row")
      .filter({ hasText: "All repositories reader" });
    page.once("dialog", (dialog) => dialog.accept());
    await readerRow
      .getByRole("button", { name: "Revoke", exact: true })
      .click();
    await expect(readerRow.getByText("Revoked", { exact: true })).toBeVisible();
    expect(
      (
        await readerGit.get(
          `/${username}/future-project.git/info/refs?service=git-upload-pack`,
        )
      ).status(),
    ).toBe(401);
  } finally {
    await page
      .evaluate(() => document.querySelector("#new-access-token")?.remove())
      .catch(() => {});
    const response = await page.request.get(`/api/v1/users/${username}/tokens`);
    if (response.ok()) {
      const tokens = (await response.json()).tokens as { id: string }[];
      for (const token of tokens)
        await page.request.delete(
          `/api/v1/users/${username}/tokens/${token.id}`,
          { headers: currentHeaders },
        );
    }
    for (const client of gitClients) await client.dispose();
    await otherContext.close();
  }
});

test("token pagination keeps prior pages and filters all loaded records", async ({
  page,
}) => {
  // This isolated UI test uses metadata-only pages; the lifecycle tests above use the real API.
  const username = "pagination-user";
  const createdAt = new Date().toISOString();
  const expiresAt = new Date(
    Date.now() + 30 * 24 * 60 * 60 * 1000,
  ).toISOString();
  const first = {
    id: "a".repeat(32),
    name: "First page token",
    username,
    permission: "read",
    repositories: ["pagination-user/project"],
    createdAt,
    expiresAt,
    revokedAt: createdAt,
  };
  const second = {
    id: "b".repeat(32),
    name: "Second page token",
    username,
    permission: "read",
    repositories: ["pagination-user/project"],
    createdAt,
    expiresAt,
    revokedAt: undefined as string | undefined,
  };
  await page.route("**/api/v1/session", (route) =>
    route.fulfill({
      json: {
        authenticated: true,
        username,
        csrfToken: "ui-test-csrf",
        shardCount: 4,
        provider: "oidc",
      },
    }),
  );
  await page.route(`**/api/v1/users/${username}/tokens**`, async (route) => {
    const request = route.request();
    if (request.method() === "DELETE") {
      second.revokedAt = new Date().toISOString();
      await route.fulfill({ status: 204 });
      return;
    }
    const after = new URL(request.url()).searchParams.get("after");
    await route.fulfill({
      json:
        after === first.id
          ? { tokens: [second] }
          : { tokens: [first], nextCursor: first.id },
    });
  });
  await page.goto("/auth/tokens");
  const firstRow = page
    .getByTestId("token-row")
    .filter({ hasText: "First page token" });
  const secondRow = page
    .getByTestId("token-row")
    .filter({ hasText: "Second page token" });
  await expect(firstRow).toBeVisible();
  await page
    .getByRole("button", { name: "Load more tokens", exact: true })
    .click();
  await expect(firstRow).toBeVisible();
  await expect(secondRow).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Load more tokens", exact: true }),
  ).toHaveCount(0);
  await page.getByLabel("Show", { exact: true }).selectOption("active");
  await expect(firstRow).toHaveCount(0);
  await expect(secondRow).toBeVisible();
  await page.getByLabel("Show", { exact: true }).selectOption("all");
  page.once("dialog", (dialog) => dialog.accept());
  await secondRow.getByRole("button", { name: "Revoke", exact: true }).click();
  await expect(secondRow.getByText("Revoked", { exact: true })).toBeVisible();
  await expect(firstRow).toBeVisible();
  await expect(page.getByTestId("token-row")).toHaveCount(2);
});
