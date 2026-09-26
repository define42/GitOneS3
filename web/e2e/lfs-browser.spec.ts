import { expect, test } from "@playwright/test";
import type { Page } from "@playwright/test";
import type { RepositoryBlob } from "../src/api";

const oid = "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03";
const downloadPath = `/alice/hello.git/info/lfs/objects/${oid}`;

async function mockRepository(
  page: Page,
  overrides: Partial<RepositoryBlob> = {},
) {
  const blob: RepositoryBlob = {
    ref: "main",
    path: "hello.txt",
    commit: "a".repeat(40),
    content: "hello\n",
    size: 6,
    binary: false,
    lfs: { oid, size: 6 },
    ...overrides,
  };
  blob.lfs = { oid, size: blob.size };
  await page.route("**/api/v1/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/v1/session") {
      await route.fulfill({
        json: {
          authenticated: true,
          username: "alice",
          userId: "oidc:alice",
          shardCount: 1,
          provider: "oidc",
        },
      });
    } else if (path === "/api/v1/spaces") {
      await route.fulfill({ json: { spaces: [] } });
    } else if (path === "/api/v1/repos/alice/hello") {
      await route.fulfill({
        json: {
          id: "test-repository",
          namespace: "alice",
          name: "hello",
          description: "LFS browser test",
          defaultBranch: "main",
          createdAt: "2026-01-01T00:00:00Z",
          createdBy: "oidc:alice",
          visibility: "private",
          empty: false,
          role: "owner",
          canWrite: true,
        },
      });
    } else if (path === "/api/v1/repos/alice/hello/branches") {
      await route.fulfill({
        json: { branches: [{ name: "main", commit: blob.commit }] },
      });
    } else if (path === "/api/v1/repos/alice/hello/tree") {
      await route.fulfill({
        json: {
          ref: "main",
          path: "",
          commit: blob.commit,
          entries: [
            {
              name: blob.path,
              path: blob.path,
              type: "file",
              size: blob.size,
              lfs: blob.lfs,
            },
            { name: "plain.txt", path: "plain.txt", type: "file", size: 12 },
          ],
        },
      });
    } else if (path === "/api/v1/repos/alice/hello/blob") {
      await route.fulfill({ json: blob });
    } else {
      await route.fulfill({ status: 404, json: { detail: "Not found" } });
    }
  });
}

test("LFS files have an icon, actual text preview, and a GitOne download", async ({ page }) => {
  await mockRepository(page);
  await page.goto("/alice/hello");
  const file = page.getByRole("link", { name: "hello.txt Git LFS 6 bytes" });
  await expect(file.locator(".lfs-badge svg")).toBeVisible();
  await expect(page.getByRole("link", { name: "plain.txt 12 bytes" }).locator(".lfs-badge")).toHaveCount(0);
  await file.click();
  const preview = page.getByRole("region", { name: "File contents" });
  await expect(preview.getByText("Git LFS", { exact: true })).toBeVisible();
  await expect(preview.locator("pre")).toHaveText("hello\n");
  await expect(preview).not.toContainText("git-lfs.github.com/spec");
  const downloadLink = preview.getByRole("link", { name: "Download hello.txt" });
  await expect(downloadLink).toHaveAttribute("href", downloadPath);
  await expect(downloadLink).toHaveAttribute("download", "hello.txt");
});

for (const fixture of [
  { path: "image.bin", size: 512, binary: true, message: "Binary file. A text preview is not available.", sizeLabel: "512 bytes" },
  { path: "large.txt", size: 3 * 1024 ** 2, tooLarge: true, message: "This file is too large to preview. Download it to view its contents.", sizeLabel: "3.0 MB" },
]) {
  test(`LFS ${fixture.path} offers a download when a preview is unavailable`, async ({ page }) => {
    const { message, sizeLabel, ...blob } = fixture;
    await mockRepository(page, { ...blob, content: "" });
    await page.goto(`/alice/hello?path=${fixture.path}`);
    const preview = page.getByRole("region", { name: "File contents" });
    await expect(preview).toContainText(message);
    await expect(preview).toContainText(sizeLabel);
    await expect(preview.locator("pre")).toHaveCount(0);
    await expect(preview.getByRole("link", { name: `Download ${fixture.path}` })).toHaveAttribute("href", downloadPath);
  });
}

for (const tooLarge of [false, true]) {
  test(`LFS README ${tooLarge ? "provides a download for a large file" : "previews its content"}`, async ({ page }) => {
    await mockRepository(page, {
      path: "README.md",
      content: tooLarge ? "" : "# Stored with LFS\n",
      size: tooLarge ? 2 * 1024 ** 2 : 18,
      tooLarge,
    });
    await page.goto("/alice/hello");
    const readme = page.getByRole("region", { name: "README", exact: true });
    await expect(readme.getByText("Git LFS", { exact: true })).toBeVisible();
    await expect(readme.getByRole("link", { name: "Download README.md" })).toHaveAttribute("href", downloadPath);
    if (tooLarge) {
      await expect(readme).toContainText("This file is too large to preview.");
      await expect(readme.locator("pre")).toHaveCount(0);
    } else {
      await expect(readme.locator("pre")).toHaveText("# Stored with LFS\n");
    }
    await expect(readme.getByRole("link", { name: "View file" })).toHaveAttribute("href", "/alice/hello?ref=main&path=README.md");
  });
}

test("LFS labels and download controls fit a narrow screen", async ({ page }) => {
  const path = "a-very-long-lfs-filename-without-spaces.txt";
  await page.setViewportSize({ width: 320, height: 740 });
  await mockRepository(page, { path });
  await page.goto("/alice/hello");
  const file = page.getByRole("link", { name: `${path} Git LFS 6 bytes` });
  await expect(file.locator(".lfs-badge")).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(320);
  await file.click();
  await expect(page.getByRole("link", { name: `Download ${path}` })).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(320);
});
