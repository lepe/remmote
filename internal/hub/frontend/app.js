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
  hostDisplays: [],   // what the probe said is up on the host
  allowExec: false,   // the host permits launching applications
  allowExecCommands: [], // the permitted commands, as it named them
};

/* ── views ──────────────────────────────────────────────────────── */

function show(view) {
  for (const el of document.querySelectorAll(".view")) el.classList.add("hidden");
  $("view-" + view).classList.remove("hidden");
  if (view === "devices") showDevices();
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
  state.hostDisplays = host.displays || [];
  state.allowExec = !!host.allowExec;
  state.allowExecCommands = host.allowExecCommands || [];
  // Launching applications is the host's decision, said in -allow-exec:
  // the option only exists where the host said yes.
  const appInput = document.querySelector('input[name="source"][value="app"]');
  const appLabel = $("e-app-radio-label");
  appInput.disabled = !state.allowExec;
  appLabel.title = state.allowExec
    ? "run one of the commands the host allows"
    : "this daemon does not allow launching applications (-allow-exec)";
  if ((document.querySelector('input[name="source"]:checked')?.value || "desktop") === "window") {
    fillWindowDisplay();
    winlistFor = null; // the display list may have changed under the picker
    refreshWindowList();
  }
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
// knownServers is what the Device select is offered: the hosts this
// machine is paired with, plus the one a connection may already name.
function knownServers() {
  const seen = [];
  for (const d of state.devices || []) {
    if (d.server && seen.indexOf(d.server) < 0) seen.push(d.server);
  }
  for (const p of state.profiles) {
    if (p.server && seen.indexOf(p.server) < 0) seen.push(p.server);
  }
  return seen;
}

// fillDevices fills the Device select: every host this machine is paired
// with, and "Other" last for one it is not paired with yet. The value is
// the server address, since that is what a connection is about.
function fillDevices(selected) {
  const sel = $("e-host");
  const keep = selected !== undefined ? selected : sel.value;
  sel.innerHTML = "";
  const first = document.createElement("option");
  first.value = "";
  first.textContent = (state.devices || []).length ? "choose a device" : "no devices yet";
  sel.appendChild(first);
  for (const d of state.devices || []) {
    const opt = document.createElement("option");
    opt.value = d.server;
    opt.textContent = d.name || d.server;
    opt.title = d.server;
    sel.appendChild(opt);
  }
  const other = document.createElement("option");
  other.value = "__other__";
  other.textContent = "Other\u2026";
  sel.appendChild(other);
  for (const s of knownServers()) {
    if (s && ![...sel.options].some((o) => o.value === s)) {
      const opt = document.createElement("option");
      opt.value = s;
      opt.textContent = s;
      sel.insertBefore(opt, other);
    }
  }
  sel.value = [...sel.options].some((o) => o.value === keep) ? keep : "";
}

// deviceFor picks the record for a server, so a selection can be named.
function deviceRecord(server) {
  return (state.devices || []).find((d) => d.server === server);
}

// chooseDevice is what picking from the Device select does: fill in the
// address and the name, or open the New device screen for a host this
// machine is not paired with yet.
async function chooseDevice() {
  const v = $("e-host").value;
  if (v === "__other__") {
    // "Other" is not a server: it is a request for one. Keep what is
    // already filled in, since coming back should not lose the draft.
    // The form opens only once the list is loaded, so the two cannot
    // both claim the tab.
    state.editing = readDraft();
    state.wantedDevice = true;
    show("devices");
    await refreshDevices();
    if (state.wantedDevice) newDevice();
    return;
  }
  state.wantedDevice = false;
  $("e-server").value = v;
  const d = deviceRecord(v);
  if (d && d.name) $("e-name").value = d.name;
  probe();
}

async function openEditor(name) {
  try {
    state.editing = name ? await api().Edit(name) : await api().NewDraft();
    state.editingName = name || "";
    $("editor-title").textContent = name ? "Edit connection" : "New connection";
    const d = state.editing;
    $("e-name").value = d.name || "";
    $("e-server").value = d.server || "";
    await refreshDevices();
    fillDevices(d.server || "");
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
  // Resizing the host screen only makes sense on a nested Xephyr display.
  $("e-resize-label").style.display = kind === "create" && server === "xephyr" ? "" : "none";
  if (source === "window") {
    // A window lives on a display that already exists: the create choice
    // has nothing to list, so the source decides, not a leftover pick.
    if (kind === "create") $("e-displaykind").value = "existing";
    fillWindowDisplay();
    refreshWindowList();
  }
  syncAppField();
}

/* The Application command field takes its shape from the host: a picker
   over the allowed list when the host named commands (-allow-exec /
   -allow-exec-file), the free box when it allowed everything with "*",
   and neither matters where launching is refused. */
function syncAppField() {
  const list = state.allowExecCommands || [];
  const concrete = state.allowExec && list.length > 0 && !list.includes("*");
  const sel = $("e-applist"), box = $("e-app");
  const pick = (id, show) => { $(id).classList.toggle("hidden", !show); };
  pick("e-applist", concrete);
  pick("e-applist-label", concrete);
  pick("e-app", !concrete);
  pick("e-app-label", !concrete);
  if (!concrete) return;
  const current = box.value.trim();
  const names = [...new Set([...list, current])].filter(Boolean);
  sel.textContent = "";
  for (const n of names) {
    const o = document.createElement("option");
    o.value = n;
    o.textContent = n;
    sel.appendChild(o);
  }
  if (current) sel.value = current;
  box.value = sel.value;
}

/* The window source picks in two steps: the display to look at, then a
   window running on it — as the daemon sees them, no hex ids to type. */

function fillWindowDisplay() {
  const sel = $("e-windowdisplay");
  const current = ($("e-displayname").value || "").trim();
  const names = [...new Set([...(state.hostDisplays || []), current])].filter(Boolean);
  if (!names.length) names.push(":0");
  sel.textContent = "";
  for (const n of names) {
    const o = document.createElement("option");
    o.value = n;
    o.textContent = n;
    sel.appendChild(o);
  }
  if (current) sel.value = current;
  $("e-displayname").value = sel.value; // the draft keeps a display name
}

let winlistFor = null; // the "server/display" the list was fetched for

async function refreshWindowList() {
  const server = $("e-server").value.trim();
  const display = ($("e-windowdisplay").value || $("e-displayname").value).trim() || ":0";
  const sel = $("e-windowlist");
  if (!server) return; // the probe fills e-server; without it there is nobody to ask
  const key = server + "/" + display;
  if (key === winlistFor && sel.options.length > 1) return;
  sel.disabled = true;
  sel.textContent = "";
  const wait = document.createElement("option");
  wait.value = "";
  wait.textContent = "looking on " + display + "…";
  sel.appendChild(wait);
  try {
    const wins = await api().Windows(server, "", "", display);
    const now = ($("e-windowdisplay").value || $("e-displayname").value).trim() || ":0";
    if (now !== display) return; // the display changed while we looked
    winlistFor = key;
    sel.textContent = "";
    const head = document.createElement("option");
    head.value = "";
    head.textContent = wins.length
      ? wins.length + (wins.length === 1 ? " window on " : " windows on ") + display
      : "no shareable windows on " + display;
    sel.appendChild(head);
    for (const w of wins) {
      const o = document.createElement("option");
      o.value = w.id;
      o.textContent = (w.title || w.id) + (w.class && w.class !== w.title ? " · " + w.class : "");
      sel.appendChild(o);
    }
    // A saved connection comes with its window id: keep it and show it
    // as the pick when the list knows it.
    const picked = $("e-window").value;
    if (picked) {
      for (const o of sel.options) {
        if (o.value === picked) {
          sel.value = picked;
          break;
        }
      }
    }
  } catch (err) {
    winlistFor = null;
    sel.textContent = "";
    const oops = document.createElement("option");
    oops.value = "";
    oops.textContent = "cannot list windows";
    sel.appendChild(oops);
    fail(err);
  } finally {
    sel.disabled = false;
  }
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
  else if (info.state === "starting") summary.push("Asking the daemon to get things ready —\nthe viewer opens as soon as the session is live.");
  else if (info.state === "live") summary.push("The session is running on the host — connecting opens the viewer in it.\n" +
    "Closing the viewer window detaches; “Open viewer” attaches again.");
  else if (info.state === "stopped") summary.push("Terminated: the session is gone. The daemon on the host is still running, ready for the next one.");
  else if (!info.state) summary.push("Choose a saved connection to start a remote session.");
  $("session-summary").textContent = summary.join("\n");
  $("btn-viewer").disabled = info.state !== "live";
  // Terminating is an act on a session: with none running (or only a
  // dead record of one), the button has nothing to act on — it hides.
  const active = info.state === "starting" || info.state === "live";
  $("btn-terminate").style.display = active ? "" : "none";
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

// showDevices decides what the Devices tab shows: the list of paired
// devices, or the form that makes one — which is what shows first when
// there is nothing to list yet.
function showDevices() {
  showForm((state.devices || []).length === 0);
}

// showForm chooses between the list and the form that replaces it — and
// the button that made the form would only be a button for itself.
//
// Visibility is the "hidden" class and nothing else. These panels carry
// that class in the markup, where it is display:none !important, so an
// inline style can never win against it: setting style.display looked
// like it hid things while the class kept them hidden forever.
function showForm(on) {
  $("devices-list-panel").classList.toggle("hidden", on);
  $("device-form").classList.toggle("hidden", !on);
  $("btn-new-device").classList.toggle("hidden", on);
}

async function refreshDevices() {
  try {
    state.devices = await api().Devices();
    renderDevices(state.devices);
    showDevices();
  } catch (err) {
    fail(err);
    state.devices = [];
    showDevices();
  }
}

function renderDevices(devices) {
  const box = $("devices");
  box.innerHTML = "";
  if (!devices.length) {
    box.innerHTML = '<div class="empty"><strong>No devices yet</strong>' +
      "<p>Pair one and it can connect to this host.</p></div>";
    return;
  }
  for (const d of devices) {
    const row = document.createElement("div");
    row.className = "row-item";
    row.innerHTML = '<span class="name"></span><span class="sub"></span><span class="tag"></span>';
    row.querySelector(".name").textContent = d.name;
    row.querySelector(".sub").textContent =
      d.server + (d.role ? " · " + d.role : "") + (d.encrypted ? " · encrypted" : " · not encrypted");
    row.querySelector(".tag").title = d.encrypted
      ? "TLS encrypted, and this host's certificate is checked on every connection"
      : "Not encrypted: anyone who can see this network can see the session";
    row.querySelector(".tag").textContent = d.pairedAt ? when(d.pairedAt) : "paired";
    row.onclick = () => editDevice(d.name);
    row.oncontextmenu = (ev) => {
      ev.preventDefault();
      confirmAsk("Revoke this device?", d.name +
        " will no longer be able to connect to " + d.server + ".", "Revoke", async () => {
          try { await api().RemoveDevice(d.name); await refreshDevices(); }
          catch (err) { fail(err); }
        });
    };
    box.appendChild(row);
  }
}

function when(iso) {
  try {
    const t = new Date(iso);
    return t.toLocaleDateString() + " " + t.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  } catch (e) { return "paired"; }
}

// editDevice opens the form with a device's values in it. A click is how
// a row is opened and changed; revoking is the other thing one might do
// to a row, and that is the right-click menu — removing a device is not
// something to do by accident on the way to editing it.
function editDevice(name) {
  const d = (state.devices || []).find((x) => x.name === name);
  if (!d) return;
  state.editingDevice = name;
  $("d-name").value = d.name;
  $("d-server").value = d.server;
  $("d-role").value = d.role || "control";
  $("d-code").value = "";
  $("d-tls").value = d.encrypted ? "on" : "off";
  $("d-tls").dispatchEvent(new Event("change"));
  $("d-code-row").classList.add("hidden");
  $("device-form-title").textContent = "Edit device";
  $("btn-pair").textContent = "Save device";
  showForm(true);
}

// newDevice opens the same form empty, for a device this machine does not
// have yet — which is the only case a pairing code is needed for.
function newDevice() {
  state.editingDevice = "";
  for (const id of ["d-name", "d-server", "d-code"]) $(id).value = "";
  $("d-role").value = "control";
  $("d-tls").value = "on";
  $("d-tls").dispatchEvent(new Event("change"));
  $("d-code-row").classList.remove("hidden");
  $("device-form-title").textContent = "New device";
  $("btn-pair").textContent = "Pair device";
  showForm(true);
}

// saveDevice is what the form's one button does: pair a device this
// machine does not have, or save the changes to one it does.
async function saveDevice() {
  try {
    if (state.editingDevice) {
      const d = (state.devices || []).find((x) => x.name === state.editingDevice);
      const rec = await api().UpdateDevice(state.editingDevice, {
        name: $("d-name").value, server: $("d-server").value,
        role: $("d-role").value, encrypted: $("d-tls").value === "on",
        fingerprint: d ? d.fingerprint : "",
      });
      toast("saved \"" + rec.name + "\" (" + rec.server + ")");
    } else {
      const rec = await api().PairDevice($("d-name").value, $("d-server").value,
        $("d-role").value, $("d-code").value, $("d-tls").value === "on");
      toast("paired \"" + rec.name + "\" with " + rec.server + " as " + rec.role +
        (rec.encrypted ? " (TLS encrypted and verified)" : " \u2014 not encrypted"));
    }
    await refreshDevices();
    if (state.wantedDevice) returnToEditor(rec.server);
  } catch (err) { fail(err); }
}

// returnToEditor goes back to the connection that asked for a device.
// server is the one to choose there — empty when the request was given
// up on, and then the form is left as it was rather than half-changed.
function returnToEditor(server) {
  state.wantedDevice = false;
  show("edit");
  fillDevices(server || "");
  const d = deviceRecord(server);
  if (d) {
    $("e-server").value = d.server;
    if (d.name) $("e-name").value = d.name;
    probe();
  } else {
    $("e-server").value = "";
    probe();
  }
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
    confirmAsk("Terminate the session?", "This ends the session on the host — anything it started goes with it. " +
      "remmote-server itself keeps running, ready for the next one.", "Terminate",
      async () => {
        try {
          await api().Terminate();
          renderSession(await api().Session());
          await refreshProfiles();
          show("list");
        } catch (err) { fail(err); }
      });
  };

  $("btn-new-device").onclick = newDevice;
  $("btn-device-cancel").onclick = () => {
    state.editingDevice = "";
    showForm(false);
    if (state.wantedDevice) returnToEditor("");
  };
  $("btn-pair").onclick = saveDevice;

  for (const tab of document.querySelectorAll(".tab")) {
    tab.onclick = () => {
      show(tab.dataset.view);
      if (tab.dataset.view === "devices") refreshDevices();
      if (tab.dataset.view === "list") refreshProfiles();
      if (tab.dataset.view === "session") refreshSession();
    };
  }
  for (const el of document.querySelectorAll('input[name="source"], #e-displaykind, #e-createserver')) {
    el.onchange = shapeEditor;
  }
  $("e-host").onchange = chooseDevice;
  $("e-windowdisplay").onchange = () => {
    $("e-displayname").value = $("e-windowdisplay").value;
    $("e-window").value = ""; // the saved pick belonged to another display
    winlistFor = null;
    refreshWindowList();
  };
  $("e-windowlist").onchange = () => {
    const v = $("e-windowlist").value;
    if (v) $("e-window").value = v;
  };
  $("e-applist").onchange = () => {
    const v = $("e-applist").value;
    if (v) $("e-app").value = v;
  };

  // The unsafe choice says what it costs, right where it is chosen.
  $("d-tls").onchange = () => {
    $("d-tls-warn").classList.toggle("hidden", $("d-tls").value !== "off");
  };
}

// Ctrl+1/2/3 switch tabs: the three views are the whole app, and a
// keyboard should reach all of them.
function switchTab(ev) {
  if (!ev.ctrlKey || ev.altKey || ev.metaKey) return false;
  // ev.key under a modifier is not always the digit; ev.code always is.
  const n = ev.key >= "1" && ev.key <= "9" ? ev.key :
    ((ev.code || "").match(/^Digit([1-9])$/) || [])[1];
  const views = { "1": "list", "2": "session", "3": "devices" };
  const view = views[n];
  if (!view) return false;
  ev.preventDefault();
  show(view);
  if (view === "devices") refreshDevices();
  if (view === "list") refreshProfiles();
  return true;
}

window.addEventListener("keydown", (ev) => {
  if (switchTab(ev)) return;
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
  await refreshDevices();
  show("list");
  refreshSession();
  setInterval(refreshSession, 2000);
  const rt = window.runtime;
  if (rt?.EventsOn) {
    rt.EventsOn("session", () => refreshSession());
  }
});
