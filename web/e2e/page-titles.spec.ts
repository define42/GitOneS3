import { expect, test } from "@playwright/test";

test("browser titles identify each page and repository view", async ({ page }) => {
  await page.route("**/api/v1/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/v1/session") {
      await route.fulfill({
        json: {
          authenticated: true,
          username: "alice",
          userId: "google:alice",
          csrfToken: "title-test-csrf",
          shardCount: 1,
          provider: "oidc",
        },
      });
    } else if (path === "/api/v1/spaces") {
      await route.fulfill({ json: { spaces: [] } });
    } else if (path === "/api/v1/repos/alice") {
      await route.fulfill({
        json: { repositories: [], role: "owner", canWrite: true },
      });
    } else if (path === "/api/v1/groups/team") {
      await route.fulfill({
        json: {
          name: "team",
          type: "group",
          creatorUserId: "google:alice",
          role: "owner",
          members: { "google:alice": "owner" },
          memberUsernames: { "google:alice": "alice" },
          invitations: {},
          invitationUsernames: {},
          csrfToken: "title-test-csrf",
        },
      });
    } else if (path === "/api/v1/groups/team/invitation") {
      await route.fulfill({ json: { name: "team", role: "reader" } });
    } else if (path === "/api/v1/users/alice/tokens") {
      await route.fulfill({ json: { tokens: [] } });
    } else if (path === "/api/v1/users/alice/ssh-keys") {
      await route.fulfill({ json: { keys: [] } });
    } else {
      await route.fulfill({ status: 404, json: { detail: "Not found" } });
    }
  });

  const cases = [
    ["/", "Your spaces · GitOne"],
    ["/alice", "Your spaces · GitOne"],
    ["/auth/login", "Sign in · GitOne"],
    ["/auth/register", "Create account · GitOne"],
    ["/auth/new-group", "Create group · GitOne"],
    ["/auth/new-repository", "Create repository · GitOne"],
    ["/auth/tokens", "Access tokens · GitOne"],
    ["/auth/ssh-keys", "SSH keys · GitOne"],
    ["/team", "/team · GitOne"],
    ["/team/settings", "Settings · /team · GitOne"],
    ["/team/invitations/accept", "Join /team · GitOne"],
    ["/team/repo", "team/repo · GitOne"],
    ["/team/repo?view=commits", "Commits · team/repo · GitOne"],
    ["/team/repo?path=docs%2FREADME.md", "README.md · team/repo · GitOne"],
  ] as const;

  for (const [path, title] of cases) {
    await page.goto(path);
    await expect(page).toHaveTitle(title);
  }
});

test("signed-out home has a welcome title", async ({ page }) => {
  await page.route("**/api/v1/session", (route) =>
    route.fulfill({
      json: { authenticated: false, shardCount: 1, provider: "oidc" },
    }),
  );
  await page.goto("/");
  await expect(page).toHaveTitle("Welcome · GitOne");
});
