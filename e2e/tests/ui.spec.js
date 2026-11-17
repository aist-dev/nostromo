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
  await expect(page.locator("#version")).not.toBeEmpty();

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

test("copying a terminal selection", async ({ page, context, isMobile }) => {
  test.skip(isMobile, "mouse selection is a desktop gesture");
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  const readClipboard = () => page.evaluate(() => navigator.clipboard.readText());

  await page.goto("/");
  await page.locator("#new-chat").click();
  await page.locator("#path-input").fill(project());
  await page.locator("#path-input").press("Enter");
  await expect(page.locator("#dir-path")).toContainText(project());
  await page.locator("#new-create").click();
  await expect.poll(() => terminalText(page)).toContain("fakeagent ready>");

  // Drag across the first line: tmux selects it and sends it back as OSC 52.
  const box = await page.locator("#terminal .xterm-screen").boundingBox();
  if (!box) throw new Error("no terminal");
  const row = await page.locator("#terminal .xterm-rows > div").first().boundingBox();
  if (!row) throw new Error("no row");
  const y = row.y + row.height / 2;
  await page.mouse.move(box.x + 2, y);
  await page.mouse.down();
  await page.mouse.move(box.x + box.width / 2, y, { steps: 10 });
  await page.mouse.up();
  await expect(page.locator("#toast-text")).toContainText("Copied");
  await expect.poll(readClipboard).toContain("fakeagent");

  // Shift+drag selects in the browser, past tmux, and copies on release.
  await page.evaluate(() => navigator.clipboard.writeText(""));
  await page.keyboard.down("Shift");
  await page.mouse.move(box.x + 2, y);
  await page.mouse.down();
  await page.mouse.move(box.x + box.width / 2, y, { steps: 10 });
  await page.mouse.up();
  await page.keyboard.up("Shift");
  await expect.poll(readClipboard).toContain("fakeagent");

  // Right click opens only the tmux menu: the browser menu is suppressed.
  const prevented = await page.evaluate(() => {
    const ev = new MouseEvent("contextmenu", { bubbles: true, cancelable: true });
    document.querySelector("#terminal .xterm-screen").dispatchEvent(ev);
    return ev.defaultPrevented;
  });
  expect(prevented).toBe(true);

  page.once("dialog", (d) => d.accept());
  await page.locator("#act-delete").click();
  await expect(page.locator(".chat")).toHaveCount(0);
});

test("pasting into the terminal", async ({ page, context, isMobile }) => {
  test.skip(isMobile, "Ctrl+V is a keyboard gesture");
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  // Record what the terminal sends to tmux.
  await page.addInitScript(() => {
    const w = /** @type {any} */ (window);
    w.sent = [];
    const send = WebSocket.prototype.send;
    WebSocket.prototype.send = function (d) {
      if (d instanceof Uint8Array) w.sent.push(new TextDecoder().decode(d));
      return send.call(this, d);
    };
  });
  const sent = () => page.evaluate(() => /** @type {any} */ (window).sent.join(""));
  const reset = () => page.evaluate(() => { /** @type {any} */ (window).sent = []; });

  await page.goto("/");
  await page.locator("#new-chat").click();
  await page.locator("#path-input").fill(project());
  await page.locator("#path-input").press("Enter");
  await expect(page.locator("#dir-path")).toContainText(project());
  await page.locator("#new-create").click();
  await expect.poll(() => terminalText(page)).toContain("fakeagent ready>");
  await page.locator("#terminal").click();

  // Ctrl+V pastes the browser clipboard as a bracketed paste, not ^V.
  await page.evaluate(() => navigator.clipboard.writeText("pasted-text"));
  await reset();
  await page.keyboard.press("Control+V");
  await expect.poll(sent).toBe("\x1b[200~pasted-text\x1b[201~");

  // A paste without text (an image) becomes ^V, for the agent's own clipboard reading.
  await reset();
  await page.evaluate(() => {
    const ev = new ClipboardEvent("paste", { clipboardData: new DataTransfer(), bubbles: true, cancelable: true });
    document.querySelector("#terminal textarea")?.dispatchEvent(ev);
  });
  await expect.poll(sent).toBe("\x16");

  page.once("dialog", (d) => d.accept());
  await page.locator("#act-delete").click();
  await expect(page.locator(".chat")).toHaveCount(0);
});
