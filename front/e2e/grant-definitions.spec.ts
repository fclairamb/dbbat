import { type Page } from "@playwright/test";
import { test, expect } from "./fixtures";

/**
 * Regression coverage for the grant-definition edit dialog.
 *
 * Bug: `DefinitionDialog` was mounted once (with `editing = null`) and seeded
 * all of its form fields from the `editing` prop via `useState` initializers,
 * which only run on first mount. Clicking "Edit" updated the prop but the
 * already-mounted dialog never re-read it, so the edit form opened empty —
 * silently overwriting the definition with blank/default values on save.
 *
 * The fix keys the dialog on `editing?.uid ?? "new"` so it remounts (and its
 * initializers re-run) whenever the target changes. These tests lock that in,
 * plus the sibling staleness paths (Edit A -> New, Edit A -> Edit B).
 */

const DEFS_URL = "grant-definitions";

/** Open the "New Definition" dialog and wait for it to be interactive. */
async function openCreateDialog(page: Page) {
  await page.getByTestId("create-grant-definition-button").click();
  await page.waitForSelector('[role="dialog"]');
  await expect(page.getByTestId("grant-definition-name")).toBeVisible();
}

/**
 * Fill the definition dialog with the given values. Assumes the dialog is open
 * and freshly mounted (blank).
 */
async function fillDefinition(
  page: Page,
  opts: {
    name: string;
    description: string;
    durationValue: string;
    durationUnitLabel: "Minutes" | "Hours" | "Days";
    controls: string[]; // e.g. ["read_only", "block_ddl"]
    maxQueries?: string;
    maxBytesValue?: string;
    bytesUnit?: "KB" | "MB" | "GB";
  }
) {
  await page.getByTestId("grant-definition-name").fill(opts.name);
  await page.getByTestId("grant-definition-description").fill(opts.description);
  await page
    .getByTestId("grant-definition-duration-value")
    .fill(opts.durationValue);

  // Radix Select for the duration unit.
  await page.getByTestId("grant-definition-duration-unit").click();
  await page.getByRole("option", { name: opts.durationUnitLabel }).click();

  // Set every control deterministically (check the desired ones, uncheck the
  // rest) so the helper is idempotent regardless of any state the dialog may
  // have retained from a prior open.
  for (const c of ["read_only", "block_copy", "block_ddl"]) {
    const box = page.locator(`#def-${c}`);
    const want = opts.controls.includes(c);
    if ((await box.isChecked()) !== want) {
      await box.click();
    }
  }

  if (opts.maxQueries) {
    await page.getByTestId("grant-definition-max-queries").fill(opts.maxQueries);
  }
  if (opts.maxBytesValue) {
    await page.getByTestId("grant-definition-max-bytes").fill(opts.maxBytesValue);
    if (opts.bytesUnit) {
      await page.getByTestId("grant-definition-bytes-unit").click();
      await page.getByRole("option", { name: opts.bytesUnit, exact: true }).click();
    }
  }
}

/** Submit the dialog and wait for it to close. */
async function submitDialog(page: Page) {
  await page.getByTestId("grant-definition-submit").click();
  await expect(page.locator('[role="dialog"]')).toBeHidden();
}

/** Locate the table row for a definition by its (unique) name. */
function rowByName(page: Page, name: string) {
  return page.locator("tr", { hasText: name });
}

/** Open the edit dialog for the definition whose row contains `name`. */
async function openEditDialog(page: Page, name: string) {
  const row = rowByName(page, name);
  await expect(row).toBeVisible();
  await row.locator('[data-testid^="edit-grant-definition-"]').click();
  await page.waitForSelector('[role="dialog"]');
  await expect(page.getByTestId("grant-definition-name")).toBeVisible();
}

test.describe("Grant Definition edit dialog prefill", () => {
  test("edit dialog is pre-filled with the definition's saved values", async ({
    authenticatedPage: page,
  }) => {
    await page.goto(DEFS_URL);
    await page.waitForLoadState("networkidle");

    const name = `E2E Prefill ${Date.now()}`;
    const description = "distinctive prefill description";

    await openCreateDialog(page);
    await fillDefinition(page, {
      name,
      description,
      durationValue: "3",
      durationUnitLabel: "Days",
      controls: ["read_only", "block_ddl"],
      maxQueries: "4200",
      maxBytesValue: "7",
      bytesUnit: "GB",
    });
    await submitDialog(page);

    // Re-open via the pencil — this is the assertion that failed before the fix.
    await openEditDialog(page, name);

    await expect(page.getByTestId("grant-definition-name")).toHaveValue(name);
    await expect(page.getByTestId("grant-definition-description")).toHaveValue(
      description
    );
    await expect(
      page.getByTestId("grant-definition-duration-value")
    ).toHaveValue("3");
    await expect(
      page.getByTestId("grant-definition-duration-unit")
    ).toContainText("Days");

    await expect(page.locator("#def-read_only")).toBeChecked();
    await expect(page.locator("#def-block_ddl")).toBeChecked();
    await expect(page.locator("#def-block_copy")).not.toBeChecked();

    await expect(page.getByTestId("grant-definition-max-queries")).toHaveValue(
      "4200"
    );
    await expect(page.getByTestId("grant-definition-max-bytes")).toHaveValue(
      "7"
    );
    await expect(
      page.getByTestId("grant-definition-bytes-unit")
    ).toContainText("GB");
  });

  test("editing one field persists it and leaves the rest intact", async ({
    authenticatedPage: page,
  }) => {
    await page.goto(DEFS_URL);
    await page.waitForLoadState("networkidle");

    const name = `E2E EditOne ${Date.now()}`;
    const description = "edit-one description";

    await openCreateDialog(page);
    await fillDefinition(page, {
      name,
      description,
      durationValue: "2",
      durationUnitLabel: "Hours",
      controls: ["read_only"],
      maxQueries: "100",
    });
    await submitDialog(page);

    // Change only the max-queries value.
    await openEditDialog(page, name);
    await page.getByTestId("grant-definition-max-queries").fill("500");
    await submitDialog(page);

    // Re-open: the edit stuck and every other field survived.
    await openEditDialog(page, name);
    await expect(page.getByTestId("grant-definition-max-queries")).toHaveValue(
      "500"
    );
    await expect(page.getByTestId("grant-definition-name")).toHaveValue(name);
    await expect(page.getByTestId("grant-definition-description")).toHaveValue(
      description
    );
    await expect(
      page.getByTestId("grant-definition-duration-value")
    ).toHaveValue("2");
    await expect(
      page.getByTestId("grant-definition-duration-unit")
    ).toContainText("Hours");
    await expect(page.locator("#def-read_only")).toBeChecked();
  });

  test("staleness: Edit A then New opens a blank form", async ({
    authenticatedPage: page,
  }) => {
    await page.goto(DEFS_URL);
    await page.waitForLoadState("networkidle");

    const name = `E2E EditThenNew ${Date.now()}`;

    await openCreateDialog(page);
    await fillDefinition(page, {
      name,
      description: "edit-then-new",
      durationValue: "5",
      durationUnitLabel: "Days",
      controls: ["block_copy"],
    });
    await submitDialog(page);

    // Edit A, then close.
    await openEditDialog(page, name);
    await expect(page.getByTestId("grant-definition-name")).toHaveValue(name);
    await page.getByRole("button", { name: "Cancel" }).click();
    await expect(page.locator('[role="dialog"]')).toBeHidden();

    // New must be blank — not carrying A's values.
    await openCreateDialog(page);
    await expect(page.getByTestId("grant-definition-name")).toHaveValue("");
    await expect(page.getByTestId("grant-definition-description")).toHaveValue(
      ""
    );
    await expect(page.locator("#def-block_copy")).not.toBeChecked();
  });

  test("staleness: New then New opens a blank form", async ({
    authenticatedPage: page,
  }) => {
    await page.goto(DEFS_URL);
    await page.waitForLoadState("networkidle");

    const name = `E2E NewThenNew ${Date.now()}`;

    // Create a definition (first New).
    await openCreateDialog(page);
    await fillDefinition(page, {
      name,
      description: "new-then-new",
      durationValue: "4",
      durationUnitLabel: "Hours",
      controls: ["read_only", "block_ddl"],
      maxQueries: "321",
    });
    await submitDialog(page);

    // Two consecutive New opens share the uid->"new" key, so only unmounting the
    // dialog on close guarantees the second New starts blank rather than
    // retaining the values just submitted.
    await openCreateDialog(page);
    await expect(page.getByTestId("grant-definition-name")).toHaveValue("");
    await expect(page.getByTestId("grant-definition-description")).toHaveValue(
      ""
    );
    await expect(page.getByTestId("grant-definition-max-queries")).toHaveValue(
      ""
    );
    await expect(page.locator("#def-read_only")).not.toBeChecked();
    await expect(page.locator("#def-block_copy")).not.toBeChecked();
    await expect(page.locator("#def-block_ddl")).not.toBeChecked();
  });

  test("staleness: Edit A then Edit B loads B's values", async ({
    authenticatedPage: page,
  }) => {
    await page.goto(DEFS_URL);
    await page.waitForLoadState("networkidle");

    const stamp = Date.now();
    const nameA = `E2E DefA ${stamp}`;
    const nameB = `E2E DefB ${stamp}`;

    await openCreateDialog(page);
    await fillDefinition(page, {
      name: nameA,
      description: "definition A",
      durationValue: "1",
      durationUnitLabel: "Hours",
      controls: ["read_only"],
    });
    await submitDialog(page);

    await openCreateDialog(page);
    await fillDefinition(page, {
      name: nameB,
      description: "definition B",
      durationValue: "9",
      durationUnitLabel: "Days",
      controls: ["block_ddl"],
    });
    await submitDialog(page);

    // Edit A, close, then Edit B — B's values must load, not A's.
    await openEditDialog(page, nameA);
    await expect(page.getByTestId("grant-definition-name")).toHaveValue(nameA);
    await page.getByRole("button", { name: "Cancel" }).click();
    await expect(page.locator('[role="dialog"]')).toBeHidden();

    await openEditDialog(page, nameB);
    await expect(page.getByTestId("grant-definition-name")).toHaveValue(nameB);
    await expect(page.getByTestId("grant-definition-description")).toHaveValue(
      "definition B"
    );
    await expect(
      page.getByTestId("grant-definition-duration-value")
    ).toHaveValue("9");
    await expect(
      page.getByTestId("grant-definition-duration-unit")
    ).toContainText("Days");
    await expect(page.locator("#def-block_ddl")).toBeChecked();
    await expect(page.locator("#def-read_only")).not.toBeChecked();
  });
});

test.describe("Grant Definition inline auto-approve toggle", () => {
  test("toggling the inline switch enables auto-approve and a subsequent request is approved instantly", async ({
    authenticatedPage: page,
  }) => {
    await page.goto(DEFS_URL);
    await page.waitForLoadState("networkidle");

    const name = `E2E AutoApprove ${Date.now()}`;

    // Create a manual (non-auto-approve) definition via the dialog.
    await openCreateDialog(page);
    await fillDefinition(page, {
      name,
      description: "starts manual, flipped via inline toggle",
      durationValue: "1",
      durationUnitLabel: "Hours",
      controls: ["read_only"],
    });
    await submitDialog(page);

    const row = rowByName(page, name);
    await expect(row).toBeVisible();
    const toggle = row.locator('[data-testid^="grant-definition-auto-approve-"]');
    await expect(toggle).toBeVisible();
    await expect(toggle).toHaveAttribute("data-state", "unchecked");

    // Flip it on: a confirmation dialog must appear before it takes effect.
    await toggle.click();
    await page.waitForSelector('[role="alertdialog"]');
    await expect(
      page.getByTestId("confirm-grant-definition-auto-approve")
    ).toBeVisible();
    await page.getByTestId("confirm-grant-definition-auto-approve").click();
    await expect(page.locator('[role="alertdialog"]')).toBeHidden();

    await expect(toggle).toHaveAttribute("data-state", "checked");

    // A request against this now-auto-approved definition should be approved
    // instantly rather than landing in the pending queue.
    await page.goto("grant-requests");
    await page.waitForLoadState("networkidle");
    await page.getByTestId("request-grant-button").click();
    await page.waitForSelector('[role="dialog"]');

    await page.getByTestId("grant-request-definition").click();
    await page.getByRole("option", { name }).click();

    await page.getByTestId("grant-request-database").click();
    await page.getByRole("option").first().click();

    await page
      .getByTestId("grant-request-justification")
      .fill("E2E auto-approve verification");
    await page.getByTestId("grant-request-submit").click();
    await expect(page.locator('[role="dialog"]')).toBeHidden();

    // Switch to "All" so the (already-resolved) request is visible, then
    // confirm it shows as approved, not pending.
    await page.getByRole("button", { name: "All" }).click();
    const requestRow = page.locator("tr", { hasText: name });
    await expect(requestRow.first()).toBeVisible();
    await expect(requestRow.first()).toContainText("approved");

    // Flip it back off: no confirmation dialog should be required this time.
    await page.goto(DEFS_URL);
    await page.waitForLoadState("networkidle");
    const toggleAfter = rowByName(page, name).locator(
      '[data-testid^="grant-definition-auto-approve-"]'
    );
    await toggleAfter.click();
    await expect(page.locator('[role="alertdialog"]')).toHaveCount(0);
    await expect(toggleAfter).toHaveAttribute("data-state", "unchecked");
  });
});
