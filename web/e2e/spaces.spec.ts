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
  await expect(page.getByRole("button", { name: /sign out/i })).toBeVisible();
}

test("register, share a group, accept, change roles, revoke access, and sign out/in", async ({
  page,
  browser,
  baseURL,
}) => {
  const suffix = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 7)}`;
  const alice = `ui-alice-${suffix}`;
  const bob = `ui-bob-${suffix}`;
  const group = `ui-team-${suffix}`;
  const memberContext = await browser.newContext({
    baseURL,
    ignoreHTTPSErrors: true,
  });
  const member = await memberContext.newPage();
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  member.on("pageerror", (error) => errors.push(error.message));
  try {
    await test.step("register two usernames through real Keycloak code flow", async () => {
      await register(page, alice, "alice");
      await register(member, bob, "bob");
    });

    await test.step("reject an occupied username", async () => {
      await page.goto("/auth/register");
      await page.getByLabel("Username", { exact: true }).fill(alice);
      await page.getByRole("button", { name: /continue/i }).click();
      await expect(page.getByRole("alert")).toContainText(
        /taken|claimed|available|registered/i,
      );
    });

    await test.step("create a group and invite by username", async () => {
      await page.goto("/auth/new-group");
      await page.getByLabel("Group name", { exact: true }).fill(group);
      await page
        .getByRole("button", { name: "Create group", exact: true })
        .click();
      await expect(page).toHaveURL(new RegExp(`/${group}(?:/settings)?/?$`));
      await page.goto(`/${group}/settings`);
      await page.getByLabel("Invite username", { exact: true }).fill(bob);
      await page
        .getByLabel("Invitation role", { exact: true })
        .selectOption("reader");
      await page
        .getByRole("button", { name: "Send invitation", exact: true })
        .click();
      await expect(
        page.getByText("Invitation sent", { exact: false }),
      ).toBeVisible();
      const before = await member.request.get(`/api/v1/groups/${group}`);
      expect(before.status()).toBe(403);
    });

    await test.step("discover and accept the invitation", async () => {
      await member.goto("/");
      const invitation = member
        .locator(`a[href="/${group}/invitations/accept"]`)
        .first();
      await expect(invitation).toBeVisible();
      await invitation.click();
      await member
        .getByRole("button", { name: "Accept invitation", exact: true })
        .click();
      await expect(member).toHaveURL(new RegExp(`/${group}/?$`));
      const view = await (
        await member.request.get(`/api/v1/groups/${group}`)
      ).json();
      expect(view.role).toBe("reader");
      await expect(
        member.getByRole("button", { name: "Send invitation", exact: true }),
      ).toHaveCount(0);
    });

    const memberSession = await (
      await member.request.get("/api/v1/session")
    ).json();
    await test.step("change the role and revoke group access", async () => {
      await page.reload();
      const row = page
        .getByTestId("member-row")
        .filter({
          has: page.locator(`[data-user-id="${memberSession.userId}"]`),
        });
      await row.getByRole("combobox").selectOption("developer");
      await row
        .getByRole("button", { name: "Update role", exact: true })
        .click();
      await expect
        .poll(
          async () =>
            (await (await member.request.get(`/api/v1/groups/${group}`)).json())
              .role,
        )
        .toBe("developer");
      page.once("dialog", (dialog) => dialog.accept());
      await row.getByRole("button", { name: "Remove", exact: true }).click();
      await expect
        .poll(async () =>
          (await member.request.get(`/api/v1/groups/${group}`)).status(),
        )
        .toBe(403);
      await member.reload();
      await expect(member.getByRole("alert")).toBeVisible();
    });

    await test.step("protect the last owner and cancel an invitation", async () => {
      const owner = page
        .getByTestId("member-row")
        .filter({ hasText: `${alice} (you)` });
      await owner.getByRole("combobox").selectOption("reader");
      await owner
        .getByRole("button", { name: "Update role", exact: true })
        .click();
      await expect(owner.getByRole("alert")).toContainText(
        "at least one owner",
      );
      await page.getByLabel("Invite username", { exact: true }).fill(bob);
      await page
        .getByRole("button", { name: "Send invitation", exact: true })
        .click();
      const pending = page
        .getByTestId("member-row")
        .filter({
          has: page.locator(`[data-user-id="${memberSession.userId}"]`),
        });
      page.once("dialog", (dialog) => dialog.accept());
      await pending
        .getByRole("button", { name: "Cancel invitation", exact: true })
        .click();
      await expect(pending).toHaveCount(0);
      expect(
        (
          await member.request.get(`/api/v1/groups/${group}/invitation`)
        ).status(),
      ).toBe(404);
    });

    await test.step("allow an owner to leave after another owner joins", async () => {
      await page.getByLabel("Invite username", { exact: true }).fill(bob);
      await page
        .getByLabel("Invitation role", { exact: true })
        .selectOption("owner");
      await page
        .getByRole("button", { name: "Send invitation", exact: true })
        .click();
      await expect(page.getByRole("status")).toContainText("Invitation sent");
      await member.goto(`/${group}/invitations/accept`);
      await member
        .getByRole("button", { name: "Accept invitation", exact: true })
        .click();
      await expect(member).toHaveURL(new RegExp(`/${group}/?$`));
      await page.reload();
      const owner = page
        .getByTestId("member-row")
        .filter({ hasText: `${alice} (you)` });
      page.once("dialog", (dialog) => dialog.accept());
      await owner.getByRole("button", { name: "Remove", exact: true }).click();
      await expect(page).toHaveURL(`${baseURL}/`);
      expect((await page.request.get(`/api/v1/groups/${group}`)).status()).toBe(
        403,
      );
      expect(
        (await (await member.request.get(`/api/v1/groups/${group}`)).json())
          .role,
      ).toBe("owner");
    });

    await test.step("sign out and sign back in using the claimed username", async () => {
      await page.getByRole("button", { name: /sign out/i }).click();
      await expect(page).toHaveURL(/\/auth\/login\?signedOut=1$/);
      const signedOut = await (
        await page.request.get("/api/v1/session")
      ).json();
      expect(signedOut.authenticated).toBe(false);
      await page.goto("/auth/login");
      await page.getByLabel("Username", { exact: true }).fill(alice);
      await page.getByRole("button", { name: /continue/i }).click();
      // The provider may resume SSO or ask for credentials again.
      const providerUsername = page.getByLabel("Username or email", {
        exact: true,
      });
      await Promise.race([
        page.waitForURL(new RegExp(`/${alice}/?$`)),
        providerUsername.waitFor(),
      ]);
      if (await providerUsername.isVisible()) {
        await providerUsername.fill("alice");
        await page
          .getByLabel("Password", { exact: true })
          .fill("alice-dev-password");
        await page
          .getByRole("button", { name: "Sign In", exact: true })
          .click();
      }
      await expect(page).toHaveURL(new RegExp(`/${alice}/?$`));
      await expect(
        page.getByRole("button", { name: /sign out/i }),
      ).toBeVisible();
    });
    expect(errors).toEqual([]);
  } finally {
    await memberContext.close();
  }
});

test("anonymous pages work at mobile width and preserve protected destinations", async ({
  page,
}) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto("/");
  await expect(
    page.getByRole("link", { name: /sign in/i }).first(),
  ).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true);
  await page.goto("/auth/new-group");
  await expect(page).toHaveURL(/\/auth\/login.*returnTo=/);
  await expect(page.getByLabel("Username", { exact: true })).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true);
});
