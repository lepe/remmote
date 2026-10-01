/* remmote — the connection manager's page. The Go side is
   window.go.hub.App (Wails bindings, promises); session events arrive
   on the "session" event. Nothing here knows about the network: that is
   the daemon's and Go's business. */

const api = () => window.go.hub.App;

const $ = (id) => document.getElementById(id);
const monitorIcon = '<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="3" y="4" width="18" height="13" rx="2"/><path d="M8 21h8m-4-4v4"/></svg>';
const state = {
  profiles: [],
  selected: 0,
  editing: null,      // the draft on the editor
  editingName: "",    // the name it had, so a rename is understood
  sessionName: "",    // the connection this session was made from
};

/* ── views ──────────────────────────────────────────────────────── */

function show(view) {
  for (const el of document.querySelectorAll(".view")) el.classList.add("hidden");
  $("view-" + view).classList.remove("hidden");
  for (const tab of document.querySelectorAll(".tab")) {
    tab.classList.toggle("active", tab.dataset.view === view);
    if (tab.dataset.view === view) tab.setAttribute("aria-current", "page");
    else tab.removeAttribute("aria-current");
  }
}

function toast(message) {
  const el = $("toast");
  el.textContent = message;
  el.classList.remove("hidden");
  clearTimeout(el._t);
  el._t = setTimeout(() => el.classList.add("hidden"), 5000);
}

let modalTrigger;
function closeModal() {
  $("modal").classList.add("hidden");
  modalTrigger?.focus();
}

function confirmAsk(title, body, okLabel, onOk) {
  modalTrigger = document.activeElement;
  $("modal-title").textContent = title;
  $("modal-body").textContent = body;
  $("modal-ok").textContent = okLabel;
  $("modal").classList.remove("hidden");
  $("modal-ok").onclick = () => { closeModal(); onOk(); };
  $("modal-cancel").onclick = closeModal;
  $("modal-cancel").focus();
}

function fail(err) {
  toast(String(err?.message || err).replace(/^Error: /, ""));
}

/* ── the speed dial ─────────────────────────────────────────────── */

async function refreshProfiles() {
  try {
    state.profiles = await api().Profiles();
    if (state.selected >= state.profiles.length) {
      state.selected = Math.max(state.profiles.length - 1, 0);
    }
    renderProfiles();
  } catch (err) { fail(err); }
}

function renderProfiles() {
  const box = $("profiles");
  box.innerHTML = "";
  $("profile-count").textContent = state.profiles.length;
  for (const id of ["btn-connect", "btn-edit", "btn-delete"]) {
    $(id).disabled = !state.profiles.length;
  }
  box.removeAttribute("aria-activedescendant");
  if (!state.profiles.length) {
    box.removeAttribute("role");
    box.innerHTML = '<div class="empty"><span class="connection-icon">' + monitorIcon +
      '</span><strong>Your workspace starts here</strong><p>Save a connection to quickly return to a remote desktop or application.</p>' +
      '<button class="primary" id="btn-first-connection">Create a connection</button></div>';
    $("btn-first-connection").onclick = () => openEditor(null);
    return;
  }
  box.setAttribute("role", "listbox");
  box.setAttribute("aria-activedescendant", "profile-" + state.selected);
  state.profiles.forEach((p, i) => {
    const row = document.createElement("div");
    row.className = "row-item" + (i === state.selected ? " selected" : "");
    row.id = "profile-" + i;
    row.setAttribute("role", "option");
    row.setAttribute("aria-selected", String(i === state.selected));
    row.innerHTML = '<span class="connection-icon">' + monitorIcon + '</span>' +
      '<span class="connection-copy"><span class="name"></span><span class="sub"></span></span><span class="tag"></span>';
    row.querySelector(".name").textContent = p.name;
    row.querySelector(".sub").textContent = p.server + (p.identity ? " · " + p.identity : "");
    row.querySelector(".tag").textContent = p.source;
    row.onclick = () => { state.selected = i; renderProfiles(); box.focus({preventScroll: true}); };
    row.ondblclick = () => connect(p.name);
    box.appendChild(row);
  });
}

/* ── the editor ─────────────────────────────────────────────────── */

// The form waits for the host: a list of displays it does not have, or
// window managers it never installed, is worse than no list at all.
async function probe() {
  const server = $("e-server").value.trim();
  const rest = $("e-rest");
  $("e-probe").className = "probe";
  if (!server) {
    rest.disabled = true;
    $("e-probe").textContent = "choose a server to begin";
    return;
  }
  $("e-probe").textContent = "connecting…";
  try {
    const res = await api().Probe(server, "", "");
    rest.disabled = false;
    $("e-probe").classList.add("ok");
    $("e-probe").textContent = "connected" + (res.device ? " as " + res.device : "");
    $("e-device").textContent = res.device ? "as " + res.device : "";
    applyHost(res.host);
  } catch (err) {
    rest.disabled = true;
    $("e-probe").classList.add("error");
    $("e-probe").textContent = "cannot reach it";
    fail(err);
  }
}

// applyHost fills the choices only the host can answer: which displays
// exist, which codecs it encodes with, which window managers it has.
function applyHost(host) {
  fill("host-displays", (host.displays || []).map((d) => d));
  fill("host-wms", host.windowManagers || []);
  const codec = $("e-codec");
  const wanted = codec.value || "hybrid";
  codec.innerHTML = "";
  for (const name of (host.codecs || ["hybrid"])) {
    const opt = document.createElement("option");
    opt.textContent = name;
    codec.appendChild(opt);
  }
  codec.value = (host.codecs || []).indexOf(wanted) >= 0 ? wanted : (host.codecs || ["hybrid"])[0];
  $("e-displaykind").querySelector('option[value="create"]').disabled = !host.canCreate;
  if (!host.canCreate && $("e-displaykind").value === "create") {
    $("e-displaykind").value = "existing";
    shapeEditor();
  }
}

function fill(id, values) {
  const box = $(id);
  box.innerHTML = "";
  for (const v of values) {
    const opt = document.createElement("option");
    opt.value = v;
    box.appendChild(opt);
  }
}

// knownServers is where the server box gets its memory: every host a
// saved connection already points at, newest use first.
function knownServers() {
  const seen = [];
  for (const p of state.profiles) {
    if (p.server && seen.indexOf(p.server) < 0) seen.push(p.server);
  }
  if (!seen.length) seen.push("127.0.0.1:7677");
  return seen;
}

async function openEditor(name) {
  try {
    state.editing = name ? await api().Edit(name) : await api().NewDraft();
    state.editingName = name || "";
    $("editor-title").textContent = name ? "Edit connection" : "New connection";
    const d = state.editing;
    fill("known-servers", knownServers());
    $("e-name").value = d.name || "";
    $("e-server").value = d.server || "";
    $("e-app").value = d.appCmd || "";
    $("e-window").value = d.windowID || "";
    $("e-maximize").checked = !!d.maximize;
    $("e-displaykind").value = d.displayKind || "existing";
    $("e-displayname").value = d.displayName || "";
    $("e-createserver").value = d.createServer || "xvfb";
    $("e-createsize").value = d.createSize || "";
    $("e-createwm").value = d.createWM || "";
    $("e-createhost").value = d.createHost || "";
    $("e-quality").value = d.quality || "";
    $("e-fps").value = d.fPS || "";
    $("e-scale").value = d.downscale || "1";
    $("e-clipboard").checked = d.clipboard !== false;
    $("e-resize").checked = !!d.resizeDesktop;
    $("e-fast").checked = !!d.fastScale;
    for (const el of document.querySelectorAll('input[name="source"]')) {
      el.checked = el.value === (d.source || "desktop");
    }
    shapeEditor();
    show("edit");
    $("e-rest").disabled = true;
    $("e-probe").textContent = "connecting…";
    probe();
    $("e-name").focus();
  } catch (err) { fail(err); }
}

// The fields that depend on the source and the display kind come and go
// with their choice.
function shapeEditor() {
  const source = document.querySelector('input[name="source"]:checked')?.value || "desktop";
  const kind = $("e-displaykind").value;
  const server = $("e-createserver").value;
  document.querySelector(".only-app").style.display = source === "app" ? "contents" : "none";
  document.querySelector(".only-window").style.display = source === "window" ? "contents" : "none";
  document.querySelector(".not-desktop").style.display = source === "desktop" ? "none" : "contents";
  document.querySelector(".only-create").style.display = kind === "create" ? "contents" : "none";
  $("e-displayname").style.display = kind === "create" ? "none" : "";
  $("e-createhost").style.display = server === "xephyr" ? "" : "none";
}

function readDraft() {
  const d = state.editing || {};
  d.name = $("e-name").value;
  d.server = $("e-server").value;
  d.identity = "";
  d.source = document.querySelector('input[name="source"]:checked')?.value || "desktop";
  d.appCmd = $("e-app").value;
  d.windowID = $("e-window").value;
  d.maximize = $("e-maximize").checked;
  d.displayKind = $("e-displaykind").value;
  d.displayName = $("e-displayname").value;
  d.createServer = $("e-createserver").value;
  d.createSize = $("e-createsize").value;
  d.createWM = $("e-createwm").value;
  d.createHost = $("e-createhost").value;
  d.codec = $("e-codec").value;
  d.quality = $("e-quality").value;
  d.fPS = $("e-fps").value;
  // One scale drives both halves: the host sends every Nth pixel and the
  // viewer magnifies them back, so the canvas and the pointer mapping
  // stay in host coordinates.
  d.downscale = $("e-scale").value;
  d.upscale = $("e-scale").value;
  d.clipboard = $("e-clipboard").checked;
  d.resizeDesktop = $("e-resize").checked;
  d.fastScale = $("e-fast").checked;
  return d;
}

async function save(connectAfter) {
  try {
    const d = readDraft();
    await api().Save(d);
    state.editingName = d.name;
    await refreshProfiles();
    const i = state.profiles.findIndex((p) => p.name === d.name);
    if (i >= 0) state.selected = i;
    renderProfiles();
    if (connectAfter) connect(d.name);
    else show("list");
  } catch (err) { fail(err); }
}

/* ── the session ────────────────────────────────────────────────── */

async function connect(name) {
  try {
    state.sessionName = name;
    show("session");
    const info = await api().Connect(name);
    renderSession(info);
  } catch (err) {
    fail(err);
    try { renderSession(await api().Session()); } catch (_) {}
  }
}

function renderSession(info) {
  const dot = $("session-dot");
  dot.className = "dot " + (info.state || "");
  $("session-state").textContent = info.state || "No active session";
  $("session-name").textContent = state.sessionName || "View and manage your current session.";
  const bits = [];
  if (info.display) bits.push(info.display + (info.width ? " " + info.width + "×" + info.height : ""));
  if (info.spec?.source) bits.push(info.spec.source + " · " + (info.spec.stream?.codec || ""));
  if (info.spec?.app?.command) bits.push("app: " + info.spec.app.command);
  $("session-detail").textContent = bits.join("  ·  ");
  const summary = [];
  if (info.error) summary.push(info.error);
  else if (info.state === "starting") summary.push("asking the daemon to get things ready…");
  else if (info.state === "live") summary.push("The session is running on the host. Opening the viewer attaches to it;\n" +
    "closing the viewer window detaches and nothing more.");
  else if (info.state === "stopped") summary.push("Terminated: the session is gone and the daemon has stopped.");
  else if (!info.state) summary.push("Choose a saved connection to start a remote session.");
  $("session-summary").textContent = summary.join("\n");
  $("btn-viewer").disabled = info.state !== "live";
  $("btn-terminate").disabled = !info.state || info.state === "stopped";
}

function renderLog(lines) {
  const el = $("session-log");
  el.textContent = (lines || []).join("\n");
  el.scrollTop = el.scrollHeight;
}

async function refreshSession() {
  try {
    renderSession(await api().Session());
    renderLog(await api().Logs());
    $("status").textContent = await api().Status();
    const viewing = await api().Viewing();
    $("btn-viewer").disabled = viewing || $("session-dot").className.indexOf("live") < 0;
    $("btn-detach").disabled = !viewing;
  } catch (err) { /* the panel stays as it was */ }
}

/* ── devices ────────────────────────────────────────────────────── */

function deviceArgs() {
  return [$("d-server").value, $("d-identity").value, $("d-tls").value];
}

async function listClients() {
  if (!$("d-server").value.trim()) return;
  try {
    const list = await api().Clients(...deviceArgs());
    const box = $("clients");
    box.innerHTML = "";
    if (!list.length) {
      box.innerHTML = '<div class="empty"><strong>No paired devices</strong><p>Use a pairing code below to give this machine access.</p></div>';
      return;
    }
    for (const c of list) {
      const row = document.createElement("button");
      row.type = "button";
      row.disabled = !!c.revoked;
      row.setAttribute("aria-label", c.name + (c.revoked ? ", revoked" : ", " + c.role + ". Revoke access"));
      row.className = "row-item";
      row.innerHTML = '<span class="name"></span><span class="sub"></span><span class="tag"></span>';
      row.querySelector(".name").textContent = c.name;
      row.querySelector(".sub").textContent = c.role;
      row.querySelector(".tag").textContent = c.revoked ? "revoked" : "paired";
      row.onclick = () => {
        confirmAsk("Revoke this device?", c.name + " will no longer be able to connect.",
          "Revoke", async () => {
            try { await api().Revoke(...deviceArgs(), c.name); await listClients(); }
            catch (err) { fail(err); }
          });
      };
      box.appendChild(row);
    }
  } catch (err) { fail(err); }
}

/* ── wiring ─────────────────────────────────────────────────────── */

function connectButtons() {
  $("btn-connect").onclick = () => {
    const p = state.profiles[state.selected];
    if (p) connect(p.name);
  };
  $("btn-new").onclick = () => openEditor(null);
  $("btn-edit").onclick = () => {
    const p = state.profiles[state.selected];
    if (p) openEditor(p.name);
  };
  $("btn-delete").onclick = () => {
    const p = state.profiles[state.selected];
    if (!p) return;
    confirmAsk("Delete this connection?", p.name + " will be forgotten on this machine.",
      "Delete", async () => {
        try { await api().Delete(p.name); await refreshProfiles(); }
        catch (err) { fail(err); }
      });
  };

  $("btn-save").onclick = () => save(false);
  $("btn-save-connect").onclick = () => save(true);
  $("btn-cancel").onclick = () => show("list");

  $("btn-viewer").onclick = async () => {
    try { await api().OpenViewer(state.sessionName); refreshSession(); }
    catch (err) { fail(err); }
  };
  $("btn-detach").onclick = async () => {
    try { await api().CloseViewer(); refreshSession(); }
    catch (err) { fail(err); }
  };
  $("btn-terminate").onclick = () => {
    confirmAsk("Terminate the session?", "This stops the session and shuts down " +
      "remmote-server on the host. Anything it started goes with it.", "Terminate",
      async () => {
        try {
          await api().Terminate();
          renderSession(await api().Session());
          await refreshProfiles();
          show("list");
        } catch (err) { fail(err); }
      });
  };

  $("btn-devices").onclick = listClients;
  $("btn-paircode").onclick = async () => {
    try {
      const code = await api().PairCode(...deviceArgs(), "view");
      toast("pairing code (view): " + code);
    } catch (err) { fail(err); }
  };
  $("btn-pair").onclick = async () => {
    try {
      const role = await api().Pair($("d-server").value, $("d-tls").value,
        $("d-code").value, $("d-name").value);
      toast("paired \"" + $("d-name").value + "\" as " + role);
    } catch (err) { fail(err); }
  };

  for (const tab of document.querySelectorAll(".tab")) {
    tab.onclick = () => {
      show(tab.dataset.view);
      if (tab.dataset.view === "devices") listClients();
      if (tab.dataset.view === "list") refreshProfiles();
      if (tab.dataset.view === "session") refreshSession();
    };
  }
  for (const el of document.querySelectorAll('input[name="source"], #e-displaykind, #e-createserver')) {
    el.onchange = shapeEditor;
  }
  $("e-server").addEventListener("change", probe);
  $("e-server").addEventListener("blur", probe);
}

window.addEventListener("keydown", (ev) => {
  if (!$("modal").classList.contains("hidden")) {
    if (ev.key === "Escape") { ev.preventDefault(); closeModal(); }
    if (ev.key === "Tab") {
      ev.preventDefault();
      (document.activeElement === $("modal-cancel") ? $("modal-ok") : $("modal-cancel")).focus();
    }
    return;
  }
  if (ev.key === "Escape") {
    if (!$("view-edit").classList.contains("hidden") ||
        !$("view-session").classList.contains("hidden")) show("list");
    return;
  }
  if (ev.ctrlKey || ev.metaKey || ev.altKey || /^(INPUT|SELECT|TEXTAREA)$/.test(ev.target.tagName)) return;
  if (!$("view-list").classList.contains("hidden")) {
    if (ev.key === "ArrowDown" || ev.key === "ArrowUp") {
      ev.preventDefault();
      const step = ev.key === "ArrowDown" ? 1 : -1;
      state.selected = Math.min(Math.max(state.selected + step, 0),
        Math.max(state.profiles.length - 1, 0));
      renderProfiles();
      $("profile-" + state.selected)?.scrollIntoView({block: "nearest"});
    } else if (ev.key === "Enter") {
      if (ev.target.closest("button")) return;
      const p = state.profiles[state.selected];
      if (p) connect(p.name);
    } else if (ev.key.toLowerCase() === "n") {
      openEditor(null);
    } else if (ev.key.toLowerCase() === "e") {
      const p = state.profiles[state.selected];
      if (p) openEditor(p.name);
    } else if (ev.key === "Delete") {
      $("btn-delete").click();
    }
  }
});

window.addEventListener("load", async () => {
  connectButtons();
  await refreshProfiles();
  show("list");
  refreshSession();
  setInterval(refreshSession, 2000);
  const rt = window.runtime;
  if (rt?.EventsOn) {
    rt.EventsOn("session", () => refreshSession());
  }
});
