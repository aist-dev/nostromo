// @ts-check
const { test, expect } = require("@playwright/test");

const project = () => process.env.NOSTROMO_E2E_PROJECT || "";

async function terminalText(page) {
  return page.locator("#terminal .xterm-rows").innerText();
}

test("chat lifecycle in the browser", async ({ page, isMobile }) => {
  const errors = [];
  page.on("console", (m) => { if (m.type() === "error") errors.push(m.text()); });
  page.on("pageerror", (e) => errors.push(String(e)));

  await page.goto("/");
  await expect(page.locator("#host")).not.toHaveText("…");

  // Create a chat through the dialog.
  if (isMobile) await page.locator("#menu-btn").click();
  await page.locator("#new-chat").click();
  const dialog = page.locator("#new-dialog");
  await expect(dialog).toBeVisible();
  await page.locator("#path-input").fill(project());
  await page.locator("#path-input").press("Enter");
  await expect(page.locator("#dir-path")).toContainText(project());
  await page.locator("#new-create").click();
  await expect(dialog).toBeHidden();

  // The terminal shows the agent; typing echoes.
  await expect(page.locator("#status")).toHaveText("running");
  await expect.poll(() => terminalText(page)).toContain("fakeagent ready>");
  const tag = isMobile ? "phone" : "desk";
  if (isMobile) {
    await expect(page.locator("#mobile-input")).toBeVisible();
    await page.locator("#line").fill("hello-" + tag);
    await page.locator("#line-form button[type=submit]").click();
  } else {
    await page.locator("#terminal").click();
    await page.keyboard.type("hello-" + tag);
    await page.keyboard.press("Enter");
  }
  await expect.poll(() => terminalText(page)).toContain("echo:hello-" + tag);

  // The drawer lists the chat under its directory.
  if (isMobile) await page.locator("#menu-btn").click();
  await expect(page.locator(".group-name").first()).toHaveText("demo app");
  await expect(page.locator(".chat.active .dot.running")).toBeVisible();
  if (isMobile) await page.locator("#scrim").click({ position: { x: 380, y: 300 } });

  // Stop, then resume.
  await page.locator("#act-stop").click();
  await expect(page.locator("#status")).toHaveText("stopped");
  await expect(page.locator("#empty-resume")).toBeVisible();
  await page.locator("#empty-resume").click();
  await expect(page.locator("#status")).toHaveText("running");
  await expect.poll(() => terminalText(page)).toContain("fakeagent ready>");

  // Rename.
  page.once("dialog", (d) => d.accept("Renamed " + tag));
  await page.locator("#title").click();
  await expect(page.locator("#title")).toHaveText("Renamed " + tag);

  // Delete the running chat (asks for confirmation).
  page.once("dialog", (d) => d.accept());
  await page.locator("#act-delete").click();
  await expect(page.locator("#title")).toHaveText("nostromo");
  await expect(page.locator(".chat")).toHaveCount(0);

  expect(errors).toEqual([]);
});
