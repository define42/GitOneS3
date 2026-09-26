import { generateKeyPairSync } from "node:crypto";
import { expect, test, type Page } from "@playwright/test";
import { openAccountMenu } from "./header-helpers";

function publicKey() {
  const { publicKey: key } = generateKeyPairSync("ed25519");
  const bytes = Buffer.from(key.export({ format: "jwk" }).x!, "base64url");
  const type = Buffer.from("ssh-ed25519");
  const blob = Buffer.alloc(8 + type.length + bytes.length);
  blob.writeUInt32BE(type.length, 0);
  type.copy(blob, 4);
  blob.writeUInt32BE(bytes.length, 4 + type.length);
  bytes.copy(blob, 8 + type.length);
  return `ssh-ed25519 ${blob.toString("base64")} browser-test`;
}

async function register(page: Page, username: string) {
  await page.goto("/auth/register");
  await page.getByLabel("Username", { exact: true }).fill(username);
  await page.getByRole("button", { name: /continue/i }).click();
  await page.waitForURL((url) => url.hostname.startsWith("keycloak."));
  await page.getByLabel("Username or email", { exact: true }).fill("alice");
  await page.getByLabel("Password", { exact: true }).fill("alice-dev-password");
  await page.getByRole("button", { name: "Sign In", exact: true }).click();
  await expect(page).toHaveURL(new RegExp(`/${username}/?$`));
}

test("register and revoke SSH keys; clone personal and group repositories with own username", async ({
  page,
  baseURL,
}) => {
  test.setTimeout(120000);
  const suffix = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 5)}`;
  const username = `ssh-${suffix}`;
  const group = `ssh-team-${suffix}`;
  const key = publicKey();
  await register(page, username);
  const session = await (await page.request.get("/api/v1/session")).json();
  expect(session.sshURL).toBeTruthy();
  const headers = {
    Origin: new URL(baseURL!).origin,
    "X-CSRF-Token": session.csrfToken,
  };
  try {
    await openAccountMenu(page);
    await page
      .getByRole("banner")
      .getByRole("link", { name: "Settings", exact: true })
      .click();
    await page.getByRole("link", { name: "SSH keys", exact: true }).click();
    await expect(page).toHaveURL(`${baseURL}/auth/ssh-keys`);
    await expect(
      page.getByText("No SSH keys yet", { exact: true }),
    ).toBeVisible();
    await page
      .getByRole("button", { name: "Add SSH key", exact: true })
      .click();
    await expect(page.getByLabel("Key name", { exact: true })).toBeFocused();
    await page
      .getByRole("button", { name: "Save SSH key", exact: true })
      .click();
    await expect(page.getByRole("alert")).toBeFocused();
    await expect(page.getByLabel("Key name", { exact: true })).toHaveAttribute(
      "aria-invalid",
      "true",
    );
    await page.getByLabel("Key name", { exact: true }).fill("Work laptop");
    await page.getByLabel("Public key", { exact: true }).fill(key);
    await page
      .getByRole("button", { name: "Save SSH key", exact: true })
      .click();
    const row = page
      .getByTestId("ssh-key-row")
      .filter({ hasText: "Work laptop" });
    await expect(row.getByText("Active", { exact: true })).toBeVisible();
    await expect(row).toContainText("SHA256:");
    await page.reload();
    await row.getByText("View public key", { exact: true }).click();
    await expect(row.locator("pre")).toContainText(key.split(" ")[1]);
    expect(
      (await page.request.get(`/api/v1/users/${username}/ssh-keys`)).status(),
    ).toBe(200);

    expect(
      (
        await page.request.post(`/api/v1/groups/${group}`, { headers })
      ).status(),
    ).toBe(201);
    for (const namespace of [username, group]) {
      expect(
        (
          await page.request.post(`/api/v1/repos/${namespace}`, {
            headers,
            data: { name: "project", initializeReadme: namespace === group },
          })
        ).status(),
      ).toBe(201);
      await page.goto(`/${namespace}/project`);
      const clone = page.locator(".clone-panel");
      if (namespace === group)
        await clone.getByText("Clone with HTTPS", { exact: true }).click();
      await clone.getByRole("button", { name: "SSH", exact: true }).click();
      const expected = new URL(session.sshURL);
      expected.username = username;
      expected.pathname = `/${namespace}/project.git`;
      await expect(
        clone.getByLabel("SSH clone URL", { exact: true }),
      ).toHaveValue(expected.toString());
      await expect(clone).toContainText(`Use your GitOne username ${username}`);
      if (namespace === username)
        await expect(
          page.getByRole("region", { name: "Repository quick setup" }),
        ).toContainText(expected.toString());
      await clone.getByRole("button", { name: "HTTPS", exact: true }).click();
      await expect(
        clone.getByLabel("HTTPS clone URL", { exact: true }),
      ).toHaveValue(`${baseURL}/${namespace}/project.git`);
    }

    await page.goto("/auth/ssh-keys");
    await page.setViewportSize({ width: 375, height: 812 });
    await row.getByText("View public key", { exact: true }).click();
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
    page.once("dialog", (dialog) => dialog.dismiss());
    await row.getByRole("button", { name: "Revoke", exact: true }).click();
    await expect(row.getByText("Active", { exact: true })).toBeVisible();
    page.once("dialog", (dialog) => dialog.accept());
    await row.getByRole("button", { name: "Revoke", exact: true }).click();
    await expect(row.getByText("Revoked", { exact: true })).toBeVisible();
    await page.reload();
    await expect(row.getByText("Revoked", { exact: true })).toBeVisible();
    expect(
      (
        await page.request.post(`/api/v1/users/${username}/ssh-keys`, {
          headers,
          data: { name: "Cannot reuse", publicKey: key },
        })
      ).status(),
    ).toBe(409);
  } finally {
    const result = await page.request.get(`/api/v1/users/${username}/ssh-keys`);
    if (result.ok()) {
      for (const item of (await result.json()).keys)
        if (!item.revokedAt)
          await page.request.delete(
            `/api/v1/users/${username}/ssh-keys/${item.id}`,
            { headers },
          );
    }
  }
});

test("SSH settings recover from errors and reject private keys before sending", async ({
  page,
}) => {
  const username = "ssh-ui-user";
  const key = publicKey();
  let listFailed = true;
  let posts = 0;
  await page.route("**/api/v1/session", (route) =>
    route.fulfill({
      json: {
        authenticated: true,
        username,
        csrfToken: "ui-csrf",
        shardCount: 4,
        provider: "oidc",
        sshURL: "ssh://gitone.localhost:2222",
      },
    }),
  );
  await page.route(`**/api/v1/users/${username}/ssh-keys`, async (route) => {
    if (route.request().method() === "POST") {
      posts += 1;
      await route.fulfill({
        status: 409,
        json: { detail: "This public key has already been registered." },
      });
    } else if (listFailed) {
      await route.fulfill({
        status: 503,
        json: { detail: "Key storage is temporarily unavailable." },
      });
    } else await route.fulfill({ json: { keys: [] } });
  });
  await page.goto("/auth/ssh-keys");
  await expect(page.getByRole("alert")).toContainText(
    "temporarily unavailable",
  );
  listFailed = false;
  await page.getByRole("button", { name: "Try again", exact: true }).click();
  await expect(
    page.getByText("No SSH keys yet", { exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Add SSH key", exact: true }).click();
  await page.getByLabel("Key name", { exact: true }).fill("My device");
  // Deliberately not a real private key. No private key is sent to the browser.
  await page
    .getByLabel("Public key", { exact: true })
    .fill("-----BEGIN OPENSSH PRIVATE KEY-----\nnot-a-key");
  await page.getByRole("button", { name: "Save SSH key", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("Keep it on your device");
  await expect(page.getByLabel("Public key", { exact: true })).toHaveAttribute(
    "aria-invalid",
    "true",
  );
  expect(posts).toBe(0);
  await page.getByLabel("Public key", { exact: true }).fill(key);
  await page.getByRole("button", { name: "Save SSH key", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText(
    "already been registered",
  );
  await expect(page.getByRole("alert")).toBeFocused();
  await expect(page.getByLabel("Public key", { exact: true })).toHaveValue(key);
  expect(posts).toBe(1);
  await page.setViewportSize({ width: 375, height: 812 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "Add SSH key", exact: true }),
  ).toBeFocused();
  await expect(page.getByLabel("Public key", { exact: true })).toHaveCount(0);
});

for (const sshEnabled of [true, false]) {
  test(`repository clone choices when SSH is ${sshEnabled ? "enabled" : "disabled"}`, async ({
    page,
    baseURL,
  }) => {
    const username = "ssh-clone-user";
    const namespace = "shared-team";
    const sshURL = "ssh://git.example:2222";
    await page.route("**/api/v1/session", (route) =>
      route.fulfill({
        json: {
          authenticated: true,
          username,
          csrfToken: "ui-csrf",
          shardCount: 4,
          provider: "oidc",
          ...(sshEnabled ? { sshURL } : {}),
        },
      }),
    );
    await page.route(`**/api/v1/repos/${namespace}/project`, (route) =>
      route.fulfill({
        json: {
          id: "project",
          namespace,
          name: "project",
          description: "",
          defaultBranch: "main",
          createdAt: new Date().toISOString(),
          createdBy: "owner",
          visibility: "private",
          empty: true,
          role: "developer",
          canWrite: true,
        },
      }),
    );
    await page.route(`**/api/v1/repos/${namespace}/project/branches`, (route) =>
      route.fulfill({ json: { branches: [] } }),
    );
    await page.goto(`/${namespace}/project`);
    await expect(
      page.getByLabel("HTTPS clone URL", { exact: true }),
    ).toHaveValue(`${baseURL}/${namespace}/project.git`);
    if (sshEnabled) {
      const sshButton = page.getByRole("button", { name: "SSH", exact: true });
      await sshButton.click();
      await expect(sshButton).toHaveAttribute("aria-pressed", "true");
      await expect(
        page.getByLabel("SSH clone URL", { exact: true }),
      ).toHaveValue(
        `ssh://${username}@git.example:2222/${namespace}/project.git`,
      );
      await expect(
        page.getByRole("region", { name: "Repository quick setup" }),
      ).toContainText(
        `ssh://${username}@git.example:2222/${namespace}/project.git`,
      );
      await expect(
        page.getByRole("link", {
          name: "Add your public SSH key",
          exact: true,
        }),
      ).toHaveAttribute("href", "/auth/ssh-keys");
      await page.setViewportSize({ width: 375, height: 812 });
      expect(
        await page.evaluate(
          () => document.documentElement.scrollWidth <= innerWidth,
        ),
      ).toBe(true);
      await page.getByRole("button", { name: "HTTPS", exact: true }).click();
      await expect(
        page.getByLabel("HTTPS clone URL", { exact: true }),
      ).toHaveValue(`${baseURL}/${namespace}/project.git`);
    } else
      await expect(
        page.getByRole("button", { name: "SSH", exact: true }),
      ).toHaveCount(0);
  });
}
