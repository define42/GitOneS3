import { expect, test, type Page } from "@playwright/test";
import { accountControl, openAccountMenu } from "./header-helpers";

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
  await expect(accountControl(page)).toHaveAccessibleName(`Account: ${namespace}`);
  await expect(accountControl(page)).toBeVisible();
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
      const missing = `missing-${suffix}`;
      await page.getByLabel("Invite username", { exact: true }).fill(missing);
      await page
        .getByRole("button", { name: "Send invitation", exact: true })
        .click();
      await expect(page.getByRole("alert")).toContainText(
        `No GitOne user named ${missing} was found`,
      );
      await expect(page.getByLabel("Invite username", { exact: true }))
        .toHaveAttribute("aria-invalid", "true");
      await expect(page.getByLabel("Invite username", { exact: true }))
        .toBeFocused();
      await page.getByLabel("Invite username", { exact: true }).fill(bob);
      await expect(page.getByLabel("Invite username", { exact: true }))
        .toHaveAttribute("aria-invalid", "false");
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
      await expect(row.getByText(bob, { exact: true })).toBeVisible();
      const groupView = await (await page.request.get(`/api/v1/groups/${group}`)).json();
      expect(groupView.memberUsernames[memberSession.userId]).toBe(bob);
      await row.getByRole("combobox").selectOption("developer");
      await row
        .getByRole("button", { name: `Update role for ${bob}`, exact: true })
        .click();
      await expect
        .poll(
          async () =>
            (await (await member.request.get(`/api/v1/groups/${group}`)).json())
              .role,
        )
        .toBe("developer");
      page.once("dialog", (dialog) => dialog.accept());
      await row.getByRole("button", { name: `Remove ${bob} from group`, exact: true }).click();
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
        .getByRole("button", { name: `Update role for ${alice} (you)`, exact: true })
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
        .getByRole("button", { name: `Cancel invitation for ${bob}`, exact: true })
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
      await owner.getByRole("button", { name: `Remove ${alice} (you) from group`, exact: true }).click();
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
      await openAccountMenu(page);
      await page.getByRole("button", { name: /sign out/i }).click();
      await expect(page).toHaveURL(/\/auth\/login\?signedOut=1$/);
      const signedOut = await (
        await page.request.get("/api/v1/session")
      ).json();
      expect(signedOut.authenticated).toBe(false);
      await page.goto("/auth/login");
      await page.getByLabel("Username", { exact: true }).fill(alice);
      const authorizationRequest = page.waitForRequest(
        (request) => {
          const url = new URL(request.url());
          return (
            request.isNavigationRequest() &&
            url.hostname.startsWith("keycloak.") &&
            url.pathname.endsWith("/protocol/openid-connect/auth")
          );
        },
        { timeout: 5_000 },
      );
      await page.getByRole("button", { name: /continue/i }).click();
      const authorizationURL = new URL((await authorizationRequest).url());
      expect(authorizationURL.searchParams.getAll("prompt")).toEqual(["login"]);
      await expect(page.getByLabel("Password", { exact: true })).toBeVisible({
        timeout: 5_000,
      });
      const restartLogin = page.getByRole("button", {
        name: "Restart login",
        exact: true,
      });
      if (await restartLogin.isVisible()) {
        await restartLogin.click();
      }
      const providerUsername = page.getByLabel("Username or email", {
        exact: true,
      });
      await expect(providerUsername).toBeEditable({ timeout: 5_000 });
      await providerUsername.fill("alice");
      await page
        .getByLabel("Password", { exact: true })
        .fill("alice-dev-password");
      await page
        .getByRole("button", { name: "Sign In", exact: true })
        .click();
      await expect(page).toHaveURL(new RegExp(`/${alice}/?$`));
      await expect(accountControl(page)).toHaveAccessibleName(`Account: ${alice}`);
      await expect(accountControl(page)).toBeVisible();
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
