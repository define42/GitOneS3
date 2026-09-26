import { expect, test, type Page } from "@playwright/test";
import type { Session } from "../src/api";
import { accountControl, openAccountMenu } from "./header-helpers";

async function beginAuthentication(
  page: Page,
  namespace: string,
  mode: "register" | "login",
) {
  await page.goto(`/auth/${mode}`);
  await page.getByLabel("Username", { exact: true }).fill(namespace);
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
  // Keycloak initially offers re-authentication for its existing SSO account.
  // Its restart control lets the user choose a different account explicitly.
  const restartLogin = page.getByRole("button", {
    name: "Restart login",
    exact: true,
  });
  if (await restartLogin.isVisible()) {
    await restartLogin.click();
  }
  await expect(
    page.getByLabel("Username or email", { exact: true }),
  ).toBeEditable({ timeout: 5_000 });
  return `${authorizationURL.origin}${authorizationURL.pathname}`;
}

async function chooseAccount(page: Page, account: "alice" | "bob") {
  await page.getByLabel("Username or email", { exact: true }).fill(account);
  await page
    .getByLabel("Password", { exact: true })
    .fill(`${account}-dev-password`);
  await page.getByRole("button", { name: "Sign In", exact: true }).click();
}

async function expectAccount(
  page: Page,
  namespace: string,
  account: "alice" | "bob",
): Promise<Session> {
  await expect(page).toHaveURL(new RegExp(`/${namespace}/?$`));
  await expect(accountControl(page)).toHaveAccessibleName(`Account: ${namespace}`);
  await expect(accountControl(page)).toBeVisible();
  const response = await page.request.get("/api/v1/session");
  expect(response.ok()).toBe(true);
  const session = (await response.json()) as Session;
  expect(session.authenticated).toBe(true);
  expect(session.username).toBe(namespace);
  expect(session.identity?.email).toBe(`${account}@example.test`);
  expect(session.identity?.subject).toBeTruthy();
  expect(session.userId).toBeTruthy();
  return session;
}

async function signOutPreservingProviderSession(page: Page, providerURL: string) {
  const providerCookies = (await page.context().cookies(providerURL)).filter(
    (cookie) => cookie.name.startsWith("KEYCLOAK_"),
  );
  expect(providerCookies.length).toBeGreaterThan(0);
  await openAccountMenu(page);
  await page
    .getByRole("banner")
    .getByRole("button", { name: /sign out/i })
    .click();
  await expect(page).toHaveURL(/\/auth\/login\?signedOut=1$/);
  const session = (await (
    await page.request.get("/api/v1/session")
  ).json()) as Session;
  expect(session.authenticated).toBe(false);
  const remainingCookies = await page.context().cookies(providerURL);
  for (const original of providerCookies) {
    // Compare without including provider cookie values in assertion output.
    expect(
      remainingCookies.some(
        (cookie) =>
          cookie.name === original.name &&
          cookie.domain === original.domain &&
          cookie.path === original.path &&
          cookie.value === original.value,
      ),
      `GitOne logout preserves the provider's ${original.name} cookie`,
    ).toBe(true);
  }
}

test("switch real Keycloak accounts in one browser without changing username ownership", async ({
  page,
}) => {
  const suffix = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 7)}`;
  const alice = `ui-switch-alice-${suffix}`;
  const bob = `ui-switch-bob-${suffix}`;
  test.info().annotations.push({
    type: "registered usernames",
    description: `Alice: ${alice}; Bob: ${bob}`,
  });
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));

  let providerURL = await beginAuthentication(page, alice, "register");
  await chooseAccount(page, "alice");
  const aliceSession = await expectAccount(page, alice, "alice");

  await test.step("switch from Alice to Bob while retaining provider cookies", async () => {
    await signOutPreservingProviderSession(page, providerURL);
    providerURL = await beginAuthentication(page, bob, "register");
    await chooseAccount(page, "bob");
  });
  const bobSession = await expectAccount(page, bob, "bob");
  expect(bobSession.identity?.subject).not.toBe(aliceSession.identity?.subject);
  expect(bobSession.userId).not.toBe(aliceSession.userId);

  await test.step("reject Alice signing into Bob's claimed username", async () => {
    await signOutPreservingProviderSession(page, providerURL);
    providerURL = await beginAuthentication(page, bob, "login");
    await chooseAccount(page, "alice");
    await expect(page).toHaveURL(/\/auth\/login\?/);
    await expect(page.getByRole("alert")).toHaveText(
      "The selected provider account does not own this username. Try another account.",
    );
    const session = (await (
      await page.request.get("/api/v1/session")
    ).json()) as Session;
    expect(session.authenticated).toBe(false);
  });

  await test.step("retry as Bob and preserve Bob's original identity", async () => {
    providerURL = await beginAuthentication(page, bob, "login");
    await chooseAccount(page, "bob");
    const session = await expectAccount(page, bob, "bob");
    expect(session.identity).toEqual(bobSession.identity);
    expect(session.userId).toBe(bobSession.userId);
  });

  await test.step("Alice can still sign into her original username", async () => {
    await signOutPreservingProviderSession(page, providerURL);
    await beginAuthentication(page, alice, "login");
    await chooseAccount(page, "alice");
    const session = await expectAccount(page, alice, "alice");
    expect(session.identity).toEqual(aliceSession.identity);
    expect(session.userId).toBe(aliceSession.userId);
  });
  expect(errors).toEqual([]);
  console.info(`Account-switching usernames: Alice=${alice} Bob=${bob}`);
});
