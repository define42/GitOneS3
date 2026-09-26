import { expect, type Page } from "@playwright/test";

export function accountControl(page: Page) {
  return page.getByRole("banner").getByRole("button", { name: /^Account: / });
}

export async function openAccountMenu(page: Page) {
  const account = accountControl(page);
  if ((await account.getAttribute("aria-expanded")) !== "true")
    await account.click();
  await expect(account).toHaveAttribute("aria-expanded", "true");
}
