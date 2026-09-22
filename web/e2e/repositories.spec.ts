import { expect, test, type Page } from "@playwright/test";

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

async function createRepository(
  page: Page,
  namespace: string,
  name: string,
  readme = true,
) {
  await page.goto(`/auth/new-repository?namespace=${namespace}`);
  await expect(page.getByLabel("Owner", { exact: true })).toHaveValue(
    namespace,
  );
  await page.getByLabel("Repository name", { exact: true }).fill(name);
  await page
    .getByLabel("Description", { exact: true })
    .fill("A project shared through GitOne.");
  await page.getByLabel("Add a README", { exact: true }).setChecked(readme);
  await page
    .getByRole("button", { name: "Create repository", exact: true })
    .click();
  await expect(page).toHaveURL(new RegExp(`/${namespace}/${name}$`));
}

test("create and browse private repositories with inherited group access", async ({
  page,
  browser,
  baseURL,
}) => {
  test.setTimeout(150000);
  const suffix = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`;
  const alice = `repo-alice-${suffix}`;
  const bob = `repo-bob-${suffix}`;
  const group = `repo-team-${suffix}`;
  const memberContext = await browser.newContext({
    baseURL,
    ignoreHTTPSErrors: true,
  });
  const member = await memberContext.newPage();
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  member.on("pageerror", (error) => errors.push(error.message));
  try {
    await test.step("register real OIDC accounts", async () => {
      await register(page, alice, "alice");
      await register(member, bob, "bob");
    });
    await test.step("create a personal repository and browse its README and real commit", async () => {
      await createRepository(page, alice, "hello-world");
      await expect(
        page.getByRole("region", { name: "README", exact: true }),
      ).toContainText("A project shared through GitOne.");
      await expect(page.getByLabel("Branch", { exact: true })).toHaveValue(
        "main",
      );
      await page.getByRole("link", { name: /^README\.md/ }).click();
      await expect(page).toHaveURL(/path=README.md/);
      await expect(
        page.getByRole("region", { name: "File contents", exact: true }),
      ).toContainText("hello-world");
      await page.getByRole("link", { name: "Commits", exact: true }).click();
      await expect(
        page.getByRole("heading", { name: "Commit history", exact: true }),
      ).toBeVisible();
      const history = await (
        await page.request.get(
          `/api/v1/repos/${alice}/hello-world/commits?ref=main`,
        )
      ).json();
      expect(history.commits).toHaveLength(1);
      expect(history.commits[0].id).toMatch(/^[0-9a-f]{40}$/);
      await expect(page.locator(".commit-row")).toHaveCount(1);
      await page.reload();
      await expect(page.locator(".commit-row")).toHaveCount(1);
      expect(
        (
          await member.request.get(`/api/v1/repos/${alice}/hello-world`)
        ).status(),
      ).toBe(403);
    });
    await test.step("reject duplicate names and create an empty repository", async () => {
      await page.goto(`/auth/new-repository?namespace=${alice}`);
      await page
        .getByLabel("Repository name", { exact: true })
        .fill("hello-world");
      await page
        .getByRole("button", { name: "Create repository", exact: true })
        .click();
      await expect(page.getByRole("alert")).toContainText(
        /exists|taken|available/i,
      );
      await expect(page.locator("#repository-error")).toBeFocused();
      await createRepository(page, alice, "empty-project", false);
      await expect(
        page.getByRole("heading", {
          name: "This repository is empty",
          exact: true,
        }),
      ).toBeVisible();
      await page.goto(`/${alice}`);
      const repositories = page.getByRole("region", {
        name: `Repositories in ${alice}`,
        exact: true,
      });
      await expect(
        repositories.getByRole("link", { name: "hello-world", exact: true }),
      ).toBeVisible();
      await expect(
        repositories.getByRole("link", { name: "empty-project", exact: true }),
      ).toBeVisible();
    });
    await test.step("create a shared group repository and accept a reader invitation", async () => {
      await page.goto("/auth/new-group");
      await page.getByLabel("Group name", { exact: true }).fill(group);
      await page
        .getByRole("button", { name: "Create group", exact: true })
        .click();
      await expect(page).toHaveURL(new RegExp(`/${group}/?$`));
      await createRepository(page, group, "team-project");
      expect(
        (
          await member.request.get(`/api/v1/repos/${group}/team-project`)
        ).status(),
      ).toBe(403);
      await page.goto(`/${group}/settings`);
      await page.getByLabel("Invite username", { exact: true }).fill(bob);
      await page
        .getByLabel("Invitation role", { exact: true })
        .selectOption("reader");
      await page
        .getByRole("button", { name: "Send invitation", exact: true })
        .click();
      await expect(page.getByRole("status")).toContainText("Invitation sent");
      await member.goto(`/${group}/invitations/accept`);
      await member
        .getByRole("button", { name: "Accept invitation", exact: true })
        .click();
      await expect(member).toHaveURL(new RegExp(`/${group}/?$`));
      const repositories = member.getByRole("region", {
        name: `Repositories in ${group}`,
        exact: true,
      });
      await expect(
        repositories.getByRole("link", { name: "team-project", exact: true }),
      ).toBeVisible();
      await expect(
        repositories.getByRole("link", { name: "New repository", exact: true }),
      ).toHaveCount(0);
      await repositories
        .getByRole("link", { name: "team-project", exact: true })
        .click();
      await expect(
        member.getByRole("region", { name: "README", exact: true }),
      ).toBeVisible();
      await expect(
        member.getByText("You have read-only access to this space.", {
          exact: false,
        }),
      ).toBeVisible();
      const session = await (
        await member.request.get("/api/v1/session")
      ).json();
      const denied = await member.request.post(`/api/v1/repos/${group}`, {
        headers: {
          Origin: new URL(baseURL!).origin,
          "X-CSRF-Token": session.csrfToken,
        },
        data: { name: "reader-cannot-create", initializeReadme: true },
      });
      expect(denied.status()).toBe(403);
    });
    await test.step("render file browsing and creation at a small mobile width", async () => {
      await member.setViewportSize({ width: 375, height: 812 });
      await member.goto(`/${group}/team-project?ref=main&path=README.md`);
      await expect(
        member.getByRole("region", { name: "File contents", exact: true }),
      ).toBeVisible();
      expect(
        await member.evaluate(
          () => document.documentElement.scrollWidth <= window.innerWidth,
        ),
      ).toBe(true);
      await page.setViewportSize({ width: 375, height: 812 });
      await page.goto(`/auth/new-repository?namespace=${group}`);
      await expect(page.getByLabel("Owner", { exact: true })).toHaveValue(
        group,
      );
      expect(
        await page.evaluate(
          () => document.documentElement.scrollWidth <= window.innerWidth,
        ),
      ).toBe(true);
    });
    expect(errors).toEqual([]);
  } finally {
    await memberContext.close();
  }
});

test("repository deep links preserve branch and file through the login screen", async ({
  page,
}) => {
  const destination = "/example/project?ref=main&path=README.md";
  await page.goto(destination);
  await expect(page).toHaveURL(/\/auth\/login\?returnTo=/);
  expect(new URL(page.url()).searchParams.get("returnTo")).toBe(destination);
  await expect(page.getByLabel("Username", { exact: true })).toBeVisible();
});
