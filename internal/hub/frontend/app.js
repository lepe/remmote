/* remmote — the connection manager's page. The Go side is
   window.go.hub.App (Wails bindings, promises); session events arrive
   on the "session" event. Nothing here knows about the network: that is
   the daemon's and Go's business. */

const api = () => window.go.hub.App;

const $ = (id) => document.getElementById(id);
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
  }
}

function toast(message) {
  const el = $("toast");
  el.textContent = message;
  el.classList.remove("hidden");
  clearTimeout(el._t);
  el._t = setTimeout(() => el.classList.add("hidden"), 5000);
}

function confirmAsk(title, body, okLabel, onOk) {
  $("modal-title").textContent = title;
  $("modal-body").textContent = body;
  $("modal-ok").textContent = okLabel;
  $("modal").classList.remove("hidden");
  $("modal-ok").onclick = () => { $("modal").classList.add("hidden"); onOk(); };
  $("modal-cancel").onclick = () => $("modal").classList.add("hidden");
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
  if (!state.profiles.length) {
    box.innerHTML = '<div class="empty">Nothing saved yet.<br>' +
      "<b>New</b> makes a connection; it is saved for next time.</div>";
    return;
  }
  state.profiles.forEach((p, i) => {
    const row = document.createElement("div");
    row.className = "row-item" + (i === state.selected ? " selected" : "");
    row.innerHTML =
      '<span class="name"></span><span class="sub"></span><span class="tag"></span>';
    row.querySelector(".name").textContent = p.name;
    row.querySelector(".sub").textContent = p.server + (p.identity ? " · " + p.identity : "");
    row.querySelector(".tag").textContent = p.source;
    row.onclick = () => { state.selected = i; renderProfiles(); };
    row.ondblclick = () => connect(p.name);
    box.appendChild(row);
  });
}

/* ── the editor ─────────────────────────────────────────────────── */

async function openEditor(name) {
  try {
    state.editing = name ? await api().Edit(name) : await api().NewDraft();
    state.editingName = name || "";
    const d = state.editing;
    $("e-name").value = d.Name || "";
    $("e-server").value = d.Server || "";
    $("e-identity").value = d.Identity || "";
    $("e-app").value = d.AppCmd || "";
    $("e-window").value = d.WindowID || "";
    $("e-maximize").checked = !!d.Maximize;
    $("e-displaykind").value = d.DisplayKind || "existing";
    $("e-displayname").value = d.DisplayName || "";
    $("e-createserver").value = d.CreateServer || "xvfb";
    $("e-createsize").value = d.CreateSize || "";
    $("e-createwm").value = d.CreateWM || "";
    $("e-createhost").value = d.CreateHost || "";
    $("e-codec").value = d.Codec || "hybrid";
    $("e-quality").value = d.Quality || "";
    $("e-fps").value = d.FPS || "";
    $("e-downscale").value = d.Downscale || "1";
    $("e-clipboard").checked = d.Clipboard !== false;
    $("e-resize").checked = !!d.ResizeDesktop;
    $("e-upscale").value = d.Upscale || "1";
    $("e-fast").checked = !!d.FastScale;
    for (const el of document.querySelectorAll('input[name="source"]')) {
      el.checked = el.value === (d.Source || "desktop");
    }
    shapeEditor();
    show("edit");
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
  d.Name = $("e-name").value;
  d.Server = $("e-server").value;
  d.Identity = $("e-identity").value;
  d.Source = document.querySelector('input[name="source"]:checked')?.value || "desktop";
  d.AppCmd = $("e-app").value;
  d.WindowID = $("e-window").value;
  d.Maximize = $("e-maximize").checked;
  d.DisplayKind = $("e-displaykind").value;
  d.DisplayName = $("e-displayname").value;
  d.CreateServer = $("e-createserver").value;
  d.CreateSize = $("e-createsize").value;
  d.CreateWM = $("e-createwm").value;
  d.CreateHost = $("e-createhost").value;
  d.Codec = $("e-codec").value;
  d.Quality = $("e-quality").value;
  d.FPS = $("e-fps").value;
  d.Downscale = $("e-downscale").value;
  d.Clipboard = $("e-clipboard").checked;
  d.ResizeDesktop = $("e-resize").checked;
  d.Upscale = $("e-upscale").value;
  d.FastScale = $("e-fast").checked;
  return d;
}

async function save(connectAfter) {
  try {
    const d = readDraft();
    await api().Save(d);
    state.editingName = d.Name;
    await refreshProfiles();
    const i = state.profiles.findIndex((p) => p.name === d.Name);
    if (i >= 0) state.selected = i;
    renderProfiles();
    if (connectAfter) connect(d.Name);
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
  dot.className = "dot " + (info.State || "");
  $("session-state").textContent = info.State || "…";
  const bits = [];
  if (info.Display) bits.push(info.Display + (info.Width ? " " + info.Width + "×" + info.Height : ""));
  if (info.Spec?.Source) bits.push(info.Spec.Source + " · " + (info.Spec.Stream?.Codec || ""));
  if (info.Spec?.App?.Command) bits.push("app: " + info.Spec.App.Command);
  $("session-detail").textContent = bits.join("  ·  ");
  const summary = [];
  if (info.Error) summary.push(info.Error);
  else if (info.State === "starting") summary.push("asking the daemon to get things ready…");
  else if (info.State === "live") summary.push("The session is running on the host. Opening the viewer attaches to it;\n" +
    "closing the viewer window detaches and nothing more.");
  else if (info.State === "stopped") summary.push("Terminated: the session is gone and the daemon has stopped.");
  $("session-summary").textContent = summary.join("\n");
  $("btn-viewer").disabled = info.State !== "live";
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
  try {
    const list = await api().Clients(...deviceArgs());
    const box = $("clients");
    box.innerHTML = "";
    if (!list.length) {
      box.innerHTML = '<div class="empty">No devices are paired.</div>';
      return;
    }
    for (const c of list) {
      const row = document.createElement("div");
      row.className = "row-item";
      row.innerHTML = '<span class="name"></span><span class="sub"></span><span class="tag"></span>';
      row.querySelector(".name").textContent = c.Name;
      row.querySelector(".sub").textContent = c.Role;
      row.querySelector(".tag").textContent = c.Revoked ? "revoked" : "paired";
      row.onclick = () => {
        confirmAsk("Revoke this device?", c.Name + " will no longer be able to connect.",
          "Revoke", async () => {
            try { await api().Revoke(...deviceArgs(), c.Name); await listClients(); }
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
    };
  }
  for (const el of document.querySelectorAll('input[name="source"], #e-displaykind, #e-createserver')) {
    el.onchange = shapeEditor;
  }
}

window.addEventListener("keydown", (ev) => {
  if (ev.key === "Escape") {
    if (!$("modal").classList.contains("hidden")) { $("modal").classList.add("hidden"); return; }
    if (!$("view-edit").classList.contains("hidden") ||
        !$("view-session").classList.contains("hidden")) show("list");
    return;
  }
  if (!$("view-list").classList.contains("hidden")) {
    if (ev.key === "ArrowDown" || ev.key === "ArrowUp") {
      ev.preventDefault();
      const step = ev.key === "ArrowDown" ? 1 : -1;
      state.selected = Math.min(Math.max(state.selected + step, 0),
        Math.max(state.profiles.length - 1, 0));
      renderProfiles();
    } else if (ev.key === "Enter") {
      const p = state.profiles[state.selected];
      if (p) connect(p.name);
    } else if (ev.key.toLowerCase() === "n") {
      openEditor(null);
    } else if (ev.key.toLowerCase() === "e") {
      const p = state.profiles[state.selected];
      if (p) openEditor(p.name);
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
