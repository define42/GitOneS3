import { expect, test, type Page } from "@playwright/test";

const username = "spaces-page-user";
type Consumer = "dashboard" | "token picker" | "repository owners";
const consumers: Consumer[] = [
  "dashboard",
  "token picker",
  "repository owners",
];

function space(name: string, role = "developer", invited = false) {
  return { name, type: "group", role, invited };
}

async function mockSession(page: Page) {
  await page.route("**/api/v1/session", (route) =>
    route.fulfill({
      json: {
        authenticated: true,
        username,
        csrfToken: "ui-csrf",
        shardCount: 2,
        provider: "oidc",
      },
    }),
  );
  await page.route(`**/api/v1/users/${username}/tokens`, (route) =>
    route.fulfill({ json: { tokens: [] } }),
  );
  const listedRepositories = new Set<string>();
  await page.route("**/api/v1/repos/*", async (route) => {
    const namespace = new URL(route.request().url()).pathname
      .split("/")
      .at(-1)!;
    listedRepositories.add(namespace);
    await route.fulfill({
      json: {
        role: "owner",
        canWrite: true,
        repositories: [
          {
            id: namespace,
            namespace,
            name: "project",
            description: "",
            defaultBranch: "main",
            visibility: "private",
            empty: false,
            role: "owner",
            canWrite: true,
            createdAt: "2026-01-01T00:00:00Z",
            createdBy: username,
          },
        ],
      },
    });
  });
  return listedRepositories;
}

async function openConsumer(page: Page, consumer: Consumer) {
  await page.goto(
    consumer === "dashboard"
      ? "/"
      : consumer === "token picker"
        ? "/auth/tokens"
        : "/auth/new-repository",
  );
  if (consumer === "token picker")
    await page
      .getByRole("button", { name: "Generate new token", exact: true })
      .click();
}

function groupChoice(page: Page, consumer: Consumer, name: string) {
  if (consumer === "dashboard")
    return page.locator(`.group-row[href="/${name}"]`);
  if (consumer === "token picker")
    return page.getByRole("checkbox", { name: `${name}/project`, exact: true });
  return page
    .getByLabel("Owner", { exact: true })
    .locator(`option[value="${name}"]`);
}

for (const consumer of consumers) {
  test(`${consumer} follows all space pages including an empty first page`, async ({
    page,
  }) => {
    const listed = await mockSession(page);
    const cursor = "opaque +/&?=% cursor";
    const requests: string[] = [];
    let finishLastPage!: () => void;
    const lastPage = new Promise<void>((resolve) => {
      finishLastPage = resolve;
    });
    await page.route("**/api/v1/spaces?*", async (route) => {
      const url = new URL(route.request().url());
      expect(url.searchParams.get("limit")).toBe("100");
      const shard = url.searchParams.get("shard");
      const after = url.searchParams.get("cursor");
      requests.push(`${shard}:${after ?? "first"}`);
      if (shard === "1") {
        await route.fulfill({
          json: { spaces: [space("reader-team", "reader")] },
        });
      } else if (!after) {
        await route.fulfill({ json: { spaces: [], nextCursor: cursor } });
      } else if (after === cursor) {
        expect(url.search).toContain("%2B%2F%26%3F%3D%25");
        await route.fulfill({
          json: { spaces: [space("later-team")], nextCursor: "last-page" },
        });
      } else if (after === "last-page") {
        await lastPage;
        await route.fulfill({
          json: { spaces: [space("invited-team", "reader", true)] },
        });
      } else throw new Error(`Unexpected discovery cursor: ${after}`);
    });
    try {
      await openConsumer(page, consumer);
      await expect.poll(() => requests.includes("0:last-page")).toBe(true);
      // A successful shard/page must not be shown as if discovery is complete.
      await expect(groupChoice(page, consumer, "later-team")).toHaveCount(0);
      await expect(groupChoice(page, consumer, "reader-team")).toHaveCount(0);
      finishLastPage();
      await expect(groupChoice(page, consumer, "later-team")).toHaveCount(1);
      if (consumer === "repository owners") {
        await expect(groupChoice(page, consumer, "reader-team")).toHaveCount(0);
        await expect(groupChoice(page, consumer, "invited-team")).toHaveCount(
          0,
        );
      } else {
        await expect(groupChoice(page, consumer, "reader-team")).toHaveCount(1);
        if (consumer === "dashboard")
          await expect(
            page.locator('a[href="/invited-team/invitations/accept"]'),
          ).toBeVisible();
        else {
          await expect(groupChoice(page, consumer, "invited-team")).toHaveCount(
            0,
          );
          expect([...listed].sort()).toEqual(
            ["later-team", "reader-team", username].sort(),
          );
        }
      }
      await expect(page.getByRole("alert")).toHaveCount(0);
      expect(requests).toContain(`0:${cursor}`);
      expect(requests).toContain("0:last-page");
    } finally {
      finishLastPage();
    }
  });

  test(`${consumer} reports later-page failures without showing partial membership and can retry`, async ({
    page,
  }) => {
    await mockSession(page);
    let fail = true;
    await page.route("**/api/v1/spaces?*", async (route) => {
      const query = new URL(route.request().url()).searchParams;
      if (query.get("shard") === "1") {
        await route.fulfill({ json: { spaces: [space("other-shard-team")] } });
      } else if (!query.has("cursor")) {
        await route.fulfill({
          json: { spaces: [space("partial-team")], nextCursor: "next-page" },
        });
      } else if (fail) {
        await route.fulfill({
          status: 503,
          json: { detail: "Discovery temporarily unavailable." },
        });
      } else await route.fulfill({ json: { spaces: [space("final-team")] } });
    });
    await openConsumer(page, consumer);
    await expect(page.getByRole("alert")).toContainText(
      "Discovery temporarily unavailable",
    );
    await expect(groupChoice(page, consumer, "partial-team")).toHaveCount(0);
    await expect(groupChoice(page, consumer, "other-shard-team")).toHaveCount(
      0,
    );
    if (consumer === "token picker")
      await expect(
        page.getByRole("button", { name: "Generate token", exact: true }),
      ).toBeDisabled();
    fail = false;
    await page
      .getByRole("button", {
        name:
          consumer === "token picker"
            ? "Retry loading repositories"
            : "Try again",
        exact: true,
      })
      .click();
    await expect(groupChoice(page, consumer, "partial-team")).toHaveCount(1);
    await expect(groupChoice(page, consumer, "final-team")).toHaveCount(1);
    await expect(groupChoice(page, consumer, "other-shard-team")).toHaveCount(
      1,
    );
    await expect(page.getByRole("alert")).toHaveCount(0);
  });
}

for (const consumer of ["dashboard", "token picker"] as const) {
  test(`${consumer} rejects cursor cycles rather than looping or showing partial spaces`, async ({
    page,
  }) => {
    await mockSession(page);
    await page.route("**/api/v1/spaces?*", async (route) => {
      const query = new URL(route.request().url()).searchParams;
      if (query.get("shard") === "1") {
        await route.fulfill({ json: { spaces: [] } });
        return;
      }
      await route.fulfill({
        json: {
          spaces: [space("partial-team")],
          nextCursor:
            query.get("cursor") === "cursor-a" ? "cursor-b" : "cursor-a",
        },
      });
    });
    await openConsumer(page, consumer);
    await expect(page.getByRole("alert")).toContainText(
      "repeated pagination cursor",
    );
    await expect(groupChoice(page, consumer, "partial-team")).toHaveCount(0);
  });
}

test("closing the token form aborts in-flight space discovery without a stale error", async ({
  page,
}) => {
  await mockSession(page);
  let finishPendingPage!: () => void;
  const pendingPage = new Promise<void>((resolve) => {
    finishPendingPage = resolve;
  });
  let waiting = false;
  await page.route("**/api/v1/spaces?*", async (route) => {
    const query = new URL(route.request().url()).searchParams;
    if (query.get("shard") === "1") {
      await route.fulfill({ json: { spaces: [] } });
    } else if (!query.has("cursor")) {
      await route.fulfill({ json: { spaces: [], nextCursor: "pending-page" } });
    } else {
      waiting = true;
      await pendingPage;
      await route.fulfill({
        status: 503,
        json: { detail: "This canceled result must not appear." },
      });
    }
  });
  try {
    await openConsumer(page, "token picker");
    await expect.poll(() => waiting).toBe(true);
    const canceled = page.waitForEvent("requestfailed", {
      predicate: (request) =>
        new URL(request.url()).searchParams.get("cursor") === "pending-page",
    });
    await page.getByRole("button", { name: "Cancel", exact: true }).click();
    await canceled;
    finishPendingPage();
    await expect(
      page.getByRole("button", { name: "Generate new token", exact: true }),
    ).toBeVisible();
    await expect(page.getByRole("alert")).toHaveCount(0);
    await expect(
      page.getByRole("region", { name: "Generate access token" }),
    ).toHaveCount(0);
  } finally {
    finishPendingPage();
  }
});
