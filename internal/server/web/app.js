// nostromo web UI. The server computes groups, sorting and statuses; this file renders them.
"use strict";

(function () {
  const $ = (id) => document.getElementById(id);
  const el = (tag, cls, text) => {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  };

  const store = {
    get(key, def) {
      try { const v = localStorage.getItem(key); return v === null ? def : JSON.parse(v); } catch (_) { return def; }
    },
    set(key, value) {
      try { localStorage.setItem(key, JSON.stringify(value)); } catch (_) { /* storage unavailable */ }
    },
  };

  const state = {
    snap: null,
    etag: "",
    version: "",
    selected: location.hash.slice(1) || "",
    filter: "",
    collapsed: store.get("nostromo.collapsed", {}), // cwd -> true/false (explicit user choice)
    pendingDelete: null, // {id, timer}
  };

  // ---------- API ----------
  async function api(method, path, body) {
    const opts = { method, headers: {} };
    if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    const res = await fetch(path, opts);
    if (res.status === 204) return null;
    const data = await res.json().catch(() => null);
    if (!res.ok) {
      const msg = (data && data.error && data.error.message) || res.statusText;
      const err = new Error(msg);
      err.code = data && data.error && data.error.code;
      err.status = res.status;
      throw err;
    }
    return data;
  }

  async function refresh() {
    try {
      const headers = state.etag ? { "If-None-Match": state.etag } : {};
      const res = await fetch("api/state", { headers, cache: "no-store" });
      if (res.status === 304) return;
      if (!res.ok) throw new Error(res.statusText);
      state.etag = res.headers.get("ETag") || "";
      const snap = await res.json();
      if (state.version && snap.version !== state.version) {
        location.reload();
        return;
      }
      state.version = snap.version;
      state.snap = snap;
      render();
    } catch (e) {
      console.warn("state refresh failed", e);
    }
  }

  function findChat(id) {
    if (!state.snap) return null;
    for (const g of state.snap.groups) for (const c of g.chats) if (c.id === id) return c;
    return null;
  }

  // ---------- formatting ----------
  // Paths are clipped from the left with direction: rtl; LRM marks keep slashes in place.
  const ltr = (s) => "\u200E" + s + "\u200E";
  function ago(iso) {
    const s = (Date.now() - new Date(iso).getTime()) / 1000;
    if (s < 60) return "now";
    if (s < 3600) return Math.floor(s / 60) + "m";
    if (s < 86400) return Math.floor(s / 3600) + "h";
    return Math.floor(s / 86400) + "d";
  }
  function displayTitle(c) {
    if (c.title) return c.title;
    const d = new Date(c.created_at);
    return "Chat from " + d.toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
  }

  // ---------- drawer ----------
  function isCollapsed(g) {
    if (g.cwd in state.collapsed) return state.collapsed[g.cwd];
    return g.chats.every((c) => c.status === "stopped");
  }

  function renderDrawer() {
    const snap = state.snap;
    $("host").textContent = snap.host;
    $("version").textContent = /^\d+\.\d+\.\d+/.test(snap.version) ? "v" + snap.version : snap.version;
    document.title = snap.host + " · nostromo";

    const peers = $("peers");
    if (snap.peers && snap.peers.length) {
      peers.hidden = false;
      peers.replaceChildren(el("option", "", snap.host));
      for (const p of snap.peers) {
        const o = el("option", "", p.name);
        o.value = p.url;
        peers.append(o);
      }
    } else {
      peers.hidden = true;
    }

    $("update").hidden = !snap.update_available;
    $("update-text").textContent = snap.update_available ? "Version " + snap.update_available + " is available" : "";

    const groups = $("groups");
    const q = state.filter.toLowerCase();
    const frag = document.createDocumentFragment();
    let shown = 0;
    for (const g of snap.groups) {
      const chats = g.chats.filter((c) =>
        (!state.pendingDelete || state.pendingDelete.id !== c.id) &&
        (!q || displayTitle(c).toLowerCase().includes(q) || g.cwd.toLowerCase().includes(q)));
      if (!chats.length) continue;
      shown++;
      const box = el("div", "group" + (!q && isCollapsed(g) ? " collapsed" : ""));
      const head = el("div", "group-head");
      head.append(el("span", "chevron", "▾"));
      const names = el("div", "group-names");
      names.append(el("div", "group-name", g.name || g.cwd), el("div", "group-path", ltr(g.cwd)));
      head.append(names);
      const add = el("button", "icon-btn add", "+");
      add.type = "button";
      add.title = "New chat in " + g.cwd;
      add.setAttribute("aria-label", add.title);
      add.addEventListener("click", (ev) => { ev.stopPropagation(); createChat(g.cwd); });
      head.append(add);
      head.addEventListener("click", () => {
        state.collapsed[g.cwd] = !box.classList.contains("collapsed");
        store.set("nostromo.collapsed", state.collapsed);
        box.classList.toggle("collapsed");
      });
      const list = el("ul", "chat-list");
      for (const c of chats) {
        const li = el("li", "chat " + c.status + (c.id === state.selected ? " active" : ""));
        li.dataset.id = c.id;
        li.append(el("span", "dot " + c.status), el("span", "chat-title", displayTitle(c)), el("span", "chat-time", ago(c.last_started_at)));
        li.title = displayTitle(c);
        li.addEventListener("click", () => select(c.id));
        list.append(li);
      }
      box.append(head, list);
      frag.append(box);
    }
    if (!shown) frag.append(el("div", "empty-groups", q ? "No chats match the filter." : "No chats yet."));
    groups.replaceChildren(frag);
  }

  // ---------- main view ----------
  const isPhone = () => window.matchMedia("(max-width: 899px)").matches;

  function renderMain() {
    const c = findChat(state.selected);
    const hasChat = !!c && !(state.pendingDelete && state.pendingDelete.id === c.id);
    $("actions").hidden = !hasChat;
    $("status").hidden = !hasChat;
    $("title").disabled = !hasChat;
    $("mobile-input").hidden = !(hasChat && c.status === "running" && isPhone());

    if (!hasChat) {
      $("title").textContent = "nostromo";
      $("terminal").hidden = true;
      $("banner").hidden = true;
      $("empty").hidden = false;
      $("empty-title").textContent = state.selected && state.snap ? "Chat not found" : "No chat selected";
      $("empty-sub").textContent = "Pick a chat on the left or start a new one.";
      $("empty-meta").textContent = "";
      $("empty-resume").hidden = $("empty-delete").hidden = true;
      $("empty-new").hidden = false;
      term.disconnect();
      return;
    }

    $("title").textContent = displayTitle(c);
    const pill = $("status");
    pill.className = "status-pill " + c.status;
    pill.textContent = c.status;
    $("act-stop").hidden = c.status !== "running";

    if (c.status === "stopped") {
      term.disconnect();
      $("terminal").hidden = true;
      $("banner").hidden = true;
      $("empty").hidden = false;
      $("empty-title").textContent = displayTitle(c);
      $("empty-sub").textContent = c.cwd;
      $("empty-meta").textContent = "Last started " + new Date(c.last_started_at).toLocaleString() + " · " + c.agent;
      $("empty-resume").hidden = $("empty-delete").hidden = false;
      $("empty-new").hidden = true;
      return;
    }

    $("empty").hidden = true;
    $("terminal").hidden = false;
    $("banner").hidden = c.status !== "failed";
    if (c.status === "failed") {
      $("banner-text").textContent = c.exit_code === null ? "The agent exited" : "Exited with code " + c.exit_code;
    }
    term.show(c);
  }

  function render() {
    if (!state.snap) return;
    renderDrawer();
    renderMain();
  }

  function select(id) {
    if (id !== state.selected) {
      state.selected = id;
      history.pushState(null, "", id ? "#" + id : location.pathname);
    }
    closeDrawer();
    render();
  }

  // ---------- terminal ----------
  const term = (function () {
    let xterm = null, fit = null, ws = null, chatId = "", mode = "", retry = 0, retryTimer = null, wanted = false;
    const delays = [1000, 2000, 5000];

    function ensure() {
      if (xterm) return;
      xterm = new Terminal({
        cursorBlink: true,
        fontFamily: getComputedStyle(document.documentElement).getPropertyValue("--mono"),
        fontSize: isPhone() ? 12 : 13,
        scrollback: 5000,
        allowProposedApi: false,
        theme: { background: "#101114" },
      });
      fit = new FitAddon.FitAddon();
      xterm.loadAddon(fit);
      xterm.open($("terminal"));
      xterm.onData((d) => send(d));
      xterm.onBinary((d) => send(d));
      new ResizeObserver(() => resize()).observe($("terminal"));
    }

    function send(data) {
      if (mode !== "live" || !ws || ws.readyState !== WebSocket.OPEN) return;
      ws.send(new TextEncoder().encode(data));
    }

    function resize() {
      if (!xterm || $("terminal").hidden) return;
      try { fit.fit(); } catch (_) { return; }
      if (ws && ws.readyState === WebSocket.OPEN && mode === "live") {
        ws.send(JSON.stringify({ type: "resize", cols: xterm.cols, rows: xterm.rows }));
      }
    }

    function connect() {
      clearTimeout(retryTimer);
      if (!wanted) return;
      try { fit.fit(); } catch (_) { /* hidden */ }
      const proto = location.protocol === "https:" ? "wss:" : "ws:";
      const url = proto + "//" + location.host + location.pathname.replace(/[^/]*$/, "") +
        "api/chats/" + chatId + "/tty?cols=" + xterm.cols + "&rows=" + xterm.rows;
      const sock = new WebSocket(url);
      sock.binaryType = "arraybuffer";
      ws = sock;
      sock.onopen = () => { retry = 0; };
      sock.onmessage = (ev) => {
        if (typeof ev.data === "string") return; // {"type":"exit"}: the close handler refreshes
        xterm.write(new Uint8Array(ev.data));
      };
      sock.onclose = (ev) => {
        if (ws !== sock) return;
        ws = null;
        if (!wanted) return;
        if (ev.code === 4404 || ev.code === 4410) { refresh(); return; }
        if (mode !== "live") return;
        const delay = delays[Math.min(retry, delays.length - 1)];
        retry++;
        retryTimer = setTimeout(connect, delay);
      };
    }

    function show(c) {
      ensure();
      const nextMode = c.status === "failed" ? "snapshot" : "live";
      if (c.id === chatId && nextMode === mode && (ws || retryTimer)) return;
      disconnect();
      chatId = c.id;
      mode = nextMode;
      wanted = true;
      retry = 0;
      xterm.reset();
      xterm.options.disableStdin = mode !== "live";
      xterm.options.cursorBlink = mode === "live";
      connect();
      if (!isPhone()) xterm.focus();
    }

    function disconnect() {
      wanted = false;
      clearTimeout(retryTimer);
      retryTimer = null;
      if (ws) { const s = ws; ws = null; s.close(); }
      chatId = "";
      mode = "";
    }

    return { show, disconnect, send, resize, focus: () => xterm && xterm.focus() };
  })();

  // ---------- actions ----------
  async function guard(fn) {
    try { await fn(); } catch (e) { toast(e.message || String(e)); }
    refresh();
  }

  function createChat(cwd, agent) {
    return guard(async () => {
      const c = await api("POST", "api/chats", { cwd, agent: agent || "" });
      state.etag = "";
      await refresh();
      select(c.id);
    });
  }

  function resumeChat(id) {
    return guard(async () => {
      try { await api("POST", "api/chats/" + id + "/resume"); } catch (e) { if (e.code !== "already_running") throw e; }
      state.etag = "";
    });
  }

  function stopChat(id) {
    return guard(async () => { await api("POST", "api/chats/" + id + "/stop"); state.etag = ""; });
  }

  function renameChat(id) {
    const c = findChat(id);
    if (!c) return;
    const title = prompt("Chat title", c.title || "");
    if (title === null) return;
    return guard(async () => { await api("PATCH", "api/chats/" + id, { title }); state.etag = ""; });
  }

  function deleteChat(id) {
    const c = findChat(id);
    if (!c) return;
    if (c.status === "running") {
      if (!confirm("Stop and delete \"" + displayTitle(c) + "\"?")) return;
      return guard(async () => { await api("DELETE", "api/chats/" + id); state.etag = ""; if (state.selected === id) select(""); });
    }
    // Stopped or failed: delete after a 5 second undo window.
    flushPendingDelete();
    const timer = setTimeout(() => {
      state.pendingDelete = null;
      hideToast();
      guard(async () => { await api("DELETE", "api/chats/" + id); state.etag = ""; });
    }, 5000);
    state.pendingDelete = { id, timer };
    if (state.selected === id) select("");
    toast("Chat deleted", () => {
      clearTimeout(timer);
      state.pendingDelete = null;
      render();
    });
    render();
  }

  function flushPendingDelete() {
    const p = state.pendingDelete;
    if (!p) return;
    clearTimeout(p.timer);
    state.pendingDelete = null;
    api("DELETE", "api/chats/" + p.id).catch(() => {});
  }

  let toastTimer = null;
  function toast(text, undo) {
    $("toast-text").textContent = text;
    $("toast-undo").hidden = !undo;
    $("toast-undo").onclick = () => { hideToast(); if (undo) undo(); };
    $("toast").hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(hideToast, 5000);
  }
  function hideToast() { $("toast").hidden = true; }

  // ---------- new chat dialog ----------
  const dlg = {
    path: "",
    async open(initial) {
      const snap = state.snap || { agents: [], recent_dirs: [] };
      const sel = $("agent-select");
      sel.replaceChildren();
      for (const a of snap.agents || []) {
        const o = el("option", "", a);
        o.value = a;
        if (a === snap.default_agent) o.selected = true;
        sel.append(o);
      }
      $("agent-row").hidden = (snap.agents || []).length < 2;
      const recent = $("recent");
      recent.replaceChildren();
      for (const d of snap.recent_dirs || []) {
        const b = el("button", "", d.split("/").filter(Boolean).pop() || d);
        b.type = "button";
        b.title = d;
        b.addEventListener("click", () => dlg.browse(d));
        recent.append(b);
      }
      $("new-error").hidden = true;
      $("new-dialog").showModal();
      await dlg.browse(initial || (snap.recent_dirs && snap.recent_dirs[0]) || "");
    },
    async browse(path) {
      const q = new URLSearchParams();
      if (path) q.set("path", path);
      if ($("dir-all").checked) q.set("all", "1");
      const list = $("dir-list");
      try {
        const r = await api("GET", "api/dirs?" + q.toString());
        dlg.path = r.path;
        $("path-input").value = r.path;
        $("dir-path").textContent = ltr(r.path);
        $("dir-up").dataset.path = r.parent;
        list.replaceChildren();
        for (const d of r.dirs) {
          const li = el("li", "", d + "/");
          li.addEventListener("click", () => dlg.browse(r.path.replace(/\/$/, "") + "/" + d));
          list.append(li);
        }
        if (!r.dirs.length) list.append(el("li", "note", "No subdirectories"));
      } catch (e) {
        list.replaceChildren(el("li", "note", e.message));
      }
    },
    async create() {
      const cwd = $("path-input").value.trim();
      if (!cwd) return;
      try {
        const c = await api("POST", "api/chats", { cwd, agent: $("agent-select").value || "" });
        $("new-dialog").close();
        state.etag = "";
        await refresh();
        select(c.id);
      } catch (e) {
        $("new-error").textContent = e.message;
        $("new-error").hidden = false;
      }
    },
  };

  // ---------- drawer on phones ----------
  function openDrawer() { $("app").classList.add("drawer-open"); $("scrim").hidden = false; }
  function closeDrawer() { $("app").classList.remove("drawer-open"); $("scrim").hidden = true; }

  // ---------- phone keys ----------
  const KEYS = { esc: "\x1b", tab: "\t", stab: "\x1b[Z", ctrlc: "\x03", up: "\x1b[A", down: "\x1b[B", right: "\x1b[C", left: "\x1b[D", enter: "\r" };

  function scroll(dir) {
    if (!state.selected) return;
    api("POST", "api/chats/" + state.selected + "/scroll", { dir }).catch((e) => toast(e.message));
  }

  // ---------- wiring ----------
  function bind() {
    $("new-chat").addEventListener("click", () => { closeDrawer(); dlg.open(); });
    $("empty-new").addEventListener("click", () => dlg.open());
    $("menu-btn").addEventListener("click", openDrawer);
    $("scrim").addEventListener("click", closeDrawer);
    $("filter").addEventListener("input", (e) => { state.filter = e.target.value; renderDrawer(); });
    $("peers").addEventListener("change", (e) => { if (e.target.value) location.href = e.target.value; });
    $("update-btn").addEventListener("click", () => guard(async () => {
      await api("POST", "api/update");
      toast("Updating… the page reloads when the new version is up.");
    }));

    $("title").addEventListener("click", () => renameChat(state.selected));
    $("act-rename").addEventListener("click", () => renameChat(state.selected));
    $("act-stop").addEventListener("click", () => stopChat(state.selected));
    $("act-delete").addEventListener("click", () => deleteChat(state.selected));
    for (const id of ["empty-resume", "banner-resume"]) $(id).addEventListener("click", () => resumeChat(state.selected));
    for (const id of ["empty-delete", "banner-delete"]) $(id).addEventListener("click", () => deleteChat(state.selected));

    $("dir-up").addEventListener("click", (e) => dlg.browse(e.currentTarget.dataset.path));
    $("dir-all").addEventListener("change", () => dlg.browse(dlg.path));
    $("path-input").addEventListener("keydown", (e) => {
      if (e.key === "Enter") { e.preventDefault(); dlg.browse($("path-input").value.trim()); }
    });
    $("new-create").addEventListener("click", () => dlg.create());

    $("keybar").addEventListener("click", (e) => {
      const b = e.target.closest("button");
      if (!b) return;
      if (b.dataset.key) term.send(KEYS[b.dataset.key]);
      if (b.dataset.scroll) scroll(b.dataset.scroll);
    });
    $("line-form").addEventListener("submit", (e) => {
      e.preventDefault();
      const input = $("line");
      const text = input.value;
      input.value = "";
      if (text) term.send(text);
      // Enter separately, so the TUI does not treat it as part of a paste.
      setTimeout(() => term.send("\r"), 100);
    });

    window.addEventListener("hashchange", () => { state.selected = location.hash.slice(1); render(); });
    window.addEventListener("popstate", () => { state.selected = location.hash.slice(1); render(); });
    window.addEventListener("resize", () => { render(); term.resize(); });

    const vv = window.visualViewport;
    const setVh = () => {
      const h = vv ? vv.height : window.innerHeight;
      document.documentElement.style.setProperty("--vh", h + "px");
      if (vv) window.scrollTo(0, 0);
    };
    if (vv) { vv.addEventListener("resize", setVh); vv.addEventListener("scroll", setVh); }
    setVh();

    document.addEventListener("visibilitychange", () => { if (!document.hidden) refresh(); });
  }

  bind();
  refresh();
  setInterval(() => { if (!document.hidden) refresh(); }, 2000);
  setInterval(() => { if (state.snap) renderDrawer(); }, 60000);
})();
