import { expect, test } from "@playwright/test";

test("group settings identify people and explain unknown invite usernames", async ({ page }) => {
  let invitationRequests = 0;
  await page.route("**/api/v1/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/v1/session") {
      await route.fulfill({
        json: {
          authenticated: true,
          username: "alice",
          userId: "google:alice",
          csrfToken: "group-test-csrf",
          shardCount: 1,
          provider: "oidc",
        },
      });
    } else if (path === "/api/v1/groups/team" && route.request().method() === "GET") {
      await route.fulfill({
        json: {
          name: "team",
          type: "group",
          creatorUserId: "google:alice",
          role: "owner",
          members: {
            "google:alice": "owner",
            "google:bob": "developer",
            "google:legacy": "reader",
          },
          memberUsernames: {
            "google:alice": "alice",
            "google:bob": "bob",
          },
          invitations: { "google:carol": "reader" },
          invitationUsernames: { "google:carol": "carol" },
          csrfToken: "group-test-csrf",
        },
      });
    } else if (path === "/api/v1/users/missing") {
      await route.fulfill({
        status: 404,
        json: { detail: "group, user, member, or invitation not found" },
      });
    } else if (path === "/api/v1/groups/team/invitations") {
      invitationRequests += 1;
      await route.fulfill({ status: 500, json: { detail: "Unexpected invitation" } });
    } else {
      await route.fulfill({ status: 404, json: { detail: `Unexpected endpoint: ${path}` } });
    }
  });

  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto("/team/settings");
  const members = page.getByTestId("member-row");
  const bob = members.filter({ has: page.locator('[data-user-id="google:bob"]') });
  await expect(bob.getByText("bob", { exact: true })).toBeVisible();
  await expect(bob.getByRole("button", { name: "Update role for bob" })).toBeDisabled();
  await expect(bob.getByRole("button", { name: "Remove bob from group" })).toBeEnabled();
  await expect(members.getByRole("button", { name: "Remove alice (you) from group" })).toBeEnabled();
  await expect(members.getByRole("button", { name: "Cancel invitation for carol" })).toBeEnabled();

  const unresolved = members.filter({ has: page.locator('[data-user-id="google:legacy"]') });
  await expect(unresolved.getByText("Username unavailable", { exact: true })).toBeVisible();
  await expect(unresolved.getByText("Access controls are unavailable until this username can be verified.")).toBeVisible();
  await expect(unresolved.getByRole("combobox")).toHaveCount(0);
  await expect(unresolved.getByRole("button", { name: /Remove/ })).toHaveCount(0);

  const username = page.getByRole("textbox", { name: "Invite username" });
  await username.fill("missing");
  await page.getByRole("button", { name: "Send invitation" }).click();
  await expect(page.getByRole("alert")).toContainText(
    "No GitOne user named missing was found",
  );
  await expect(username).toHaveAttribute("aria-invalid", "true");
  await expect(username).toBeFocused();
  await username.fill("carol");
  await expect(username).toHaveAttribute("aria-invalid", "false");
  expect(invitationRequests).toBe(0);
});
