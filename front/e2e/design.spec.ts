import { test, expect } from "./fixtures";

// The rules of /design-reference that a page can break without a type error.
test.describe("Design reference", () => {
  test("renders every section", async ({ authenticatedPage: page }) => {
    await page.goto("design-reference");
    await expect(page.getByTestId("design-reference-page")).toBeVisible();
    for (const id of [
      "conventions",
      "app-chrome",
      "page-header",
      "detail-header",
      "button-placement",
      "forms",
      "data-display",
      "feedback",
      "colors",
    ]) {
      await expect(
        page.getByTestId(`design-reference-section-${id}`),
      ).toBeVisible();
    }
    // An example on the page must not rename the page's own breadcrumb.
    await expect(
      page.locator('nav[aria-label="breadcrumb"] [aria-current="page"]'),
    ).toHaveText("Design Reference");
  });

  test("an edit page is titled with the record's name, not a uid", async ({
    authenticatedPage: page,
  }) => {
    await page.goto("users");
    await page.getByTestId("edit-user-admin").click();
    await expect(page.getByTestId("edit-user-page")).toBeVisible();

    const header = page.getByTestId("detail-header");
    await expect(header.getByRole("heading", { level: 1 })).toHaveText("admin");

    // The top-bar trail ends on the name too, never on a (short) uid.
    const leaf = page.locator(
      'nav[aria-label="breadcrumb"] [aria-current="page"]',
    );
    await expect(leaf).toHaveText("admin");

    // The way back is the breadcrumb link, and it lands on the list.
    await header.getByTestId("back-to-list").click();
    await expect(page).toHaveURL(/\/users$/);
  });

  test("list pages share the layout's centered column", async ({
    authenticatedPage: page,
  }) => {
    // Pages used to differ: some wrapped themselves in `container mx-auto
    // py-6`, which shifted the header down and narrowed the column. Every
    // page header now starts at the same offset.
    const offsets: number[] = [];
    for (const path of ["users", "user-groups", "server-groups", "grant-definitions"]) {
      await page.goto(path);
      const box = await page.getByTestId("page-header").boundingBox();
      expect(box).not.toBeNull();
      offsets.push(Math.round(box!.x), Math.round(box!.y));
    }
    expect(new Set(offsets.filter((_, i) => i % 2 === 0)).size).toBe(1);
    expect(new Set(offsets.filter((_, i) => i % 2 === 1)).size).toBe(1);
  });
});
