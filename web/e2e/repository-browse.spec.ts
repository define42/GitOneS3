import { expect, test } from "@playwright/test";

test("repository views use one snapshot request and honor the server default branch", async ({ page }) => {
  const base = "/api/v1/repos/alice/project";
  const commit = "a".repeat(40);
  const previous = "b".repeat(40);
  const repository = {
    id: "project", namespace: "alice", name: "project", description: "Browse test",
    defaultBranch: "trunk", createdAt: "2026-01-01T00:00:00Z", createdBy: "alice",
    visibility: "private", empty: false, role: "owner", canWrite: true,
  };
  const completed: URL[] = [];
  page.on("requestfinished", (request) => {
    const url = new URL(request.url());
    if (url.pathname.startsWith(base)) completed.push(url);
  });
  await page.route("**/api/v1/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/v1/session") {
      await route.fulfill({ json: {
        authenticated: true, username: "alice", userId: "oidc:alice", shardCount: 1, provider: "oidc",
      } });
      return;
    }
    if (url.pathname === "/api/v1/spaces") {
      await route.fulfill({ json: { spaces: [] } });
      return;
    }
    if (url.pathname !== `${base}/browse`) {
      await route.fulfill({ status: 404, json: { detail: "Unexpected separate repository request" } });
      return;
    }
    const path = url.searchParams.get("path") ?? "";
    const blob = {
      ref: "trunk", path: path.endsWith(".md") ? path : `${path ? `${path}/` : ""}README.md`,
      commit, content: `# ${path || "Root"}\n`, size: 20, binary: false,
    };
    const shared = { repository, branches: [{ name: "trunk", commit }] };
    if (url.searchParams.get("view") === "commits") {
      await route.fulfill({ json: { ...shared, commits: [
        { id: commit, message: "Latest change", authorName: "Alice", createdAt: "2026-01-02T00:00:00Z", parents: [previous] },
        { id: previous, message: "Initial commit", authorName: "Alice", createdAt: "2026-01-01T00:00:00Z", parents: [] },
      ] } });
    } else if (path.endsWith(".md")) {
      await route.fulfill({ json: { ...shared, blob } });
    } else {
      await route.fulfill({ json: {
        ...shared,
        tree: { ref: "trunk", path, commit, entries: [
          { name: "README.md", path: blob.path, type: "file", size: blob.size },
        ] },
        readme: blob,
      } });
    }
  });

  for (const location of ["", "?path=docs", "?path=docs%2FREADME.md", "?view=commits"]) {
    completed.length = 0;
    await page.goto(`/alice/project${location}`);
    await expect(page.getByLabel("Branch", { exact: true })).toHaveValue("trunk");
    if (location.includes("commits")) {
      await expect(page.locator(".commit-row h3")).toHaveText(["Latest change", "Initial commit"]);
    } else if (location.includes("README.md")) {
      await expect(page.getByRole("region", { name: "File contents" })).toContainText("docs/README.md");
    } else {
      await expect(page.getByRole("region", { name: "README", exact: true })).toContainText(location ? "docs" : "Root");
    }
    await expect.poll(() => completed.length).toBe(1);
    expect(completed[0].pathname).toBe(`${base}/browse`);
    expect(completed[0].searchParams.has("ref")).toBe(false);
    await expect(page.getByRole("alert")).toHaveCount(0);
  }
});
