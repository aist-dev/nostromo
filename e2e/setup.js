// Builds the agent and the fake agent, then runs `agent serve` against a private
// NOSTROMO_HOME and tmux socket. Returns a teardown that stops both.
const { execFileSync, spawn } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const root = path.resolve(__dirname, "..");
const port = 18787;

async function waitFor(url, ms) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    try {
      const res = await fetch(url);
      if (res.ok) return;
    } catch (_) { /* not up yet */ }
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error("server did not start: " + url);
}

module.exports = async function globalSetup() {
  const work = fs.mkdtempSync(path.join(os.tmpdir(), "nostromo-e2e-"));
  const home = path.join(work, "home");
  const project = path.join(work, "projects", "demo app");
  fs.mkdirSync(home, { recursive: true });
  fs.mkdirSync(project, { recursive: true });
  const agent = path.join(work, "agent");
  const fake = path.join(work, "fakeagent");
  execFileSync("go", ["build", "-o", agent, "./cmd/agent"], { cwd: root, stdio: "inherit" });
  execFileSync("go", ["build", "-o", fake, "./testdata/fakeagent"], { cwd: root, stdio: "inherit" });
  const socket = "nostromo-e2e-" + process.pid;
  fs.writeFileSync(path.join(home, "config.toml"), [
    `port = ${port}`,
    `tmux_socket = "${socket}"`,
    `check_updates = false`,
    `default_agent = "fake"`,
    `[agents.fake]`,
    `start = "'${fake}' --session-id {id}"`,
    `resume = "'${fake}' --resume {resume_id}"`,
    `shell_wrap = false`,
    ``,
  ].join("\n"));
  const env = { ...process.env, NOSTROMO_HOME: home, HOME: work };
  delete env.TMUX;
  const server = spawn(agent, ["serve"], { env, stdio: "inherit" });
  process.env.NOSTROMO_E2E_PROJECT = project;
  await waitFor(`http://127.0.0.1:${port}/healthz`, 15000);

  return async () => {
    server.kill("SIGTERM");
    try { execFileSync("tmux", ["-L", socket, "kill-server"], { stdio: "ignore" }); } catch (_) { /* no server */ }
    fs.rmSync(work, { recursive: true, force: true });
  };
};
