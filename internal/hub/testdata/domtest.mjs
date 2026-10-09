// Drives the hub's real app.js against a tiny DOM, so the click-to-edit
// behaviour is checked without needing clicks to reach a webview.
import fs from "node:fs";
import vm from "node:vm";

const src = fs.readFileSync(process.argv[2], "utf8");

function mkEl(id) {
  const classes = new Set();
  const el = {
    id, value: "", textContent: "", innerHTML: "", title: "",
    children: [], style: {},
    classList: {
      add: (c) => classes.add(c),
      remove: (c) => classes.delete(c),
      toggle: (c, on) => (on === undefined ? (classes.has(c) ? classes.delete(c) : classes.add(c)) : on ? classes.add(c) : classes.delete(c)),
      contains: (c) => classes.has(c),
    },
    appendChild: (c) => el.children.push(c),
    querySelector: () => mkEl("inner"),
    addEventListener: () => {},
    dispatchEvent: () => { if (el.onchange) el.onchange({}); return true; },
    scrollIntoView: () => {},
    closest: () => null,
  };
  return el;
}

// A select holds options; setting its value is ignored unless one offers it.
function mkSelect(id) {
  const el = mkEl(id);
  el.options = [];
  el._value = "";
  Object.defineProperty(el, "innerHTML", {
    get: () => "", set: () => { el.options.length = 0; },
  });
  Object.defineProperty(el, "value", {
    get: () => el._value,
    set: (v) => { el._value = el.options.some((o) => o.value === v) ? v : ""; },
  });
  el.appendChild = (o) => el.options.push(o);
  el.insertBefore = (o, ref) => {
    const i = el.options.indexOf(ref);
    if (i < 0) el.options.push(o); else el.options.splice(i, 0, o);
    return o;
  };
  el.create = (value, text) => ({ value, textContent: text, title: "" });
  return el;
}

const els = {};
const byId = (id) => (els[id] ||= (id === "e-host" ? mkSelect(id) : mkEl(id)));

// querySelector is how the editor decides what to show: one element per
// selector, so what shapeEditor did to it can be looked at — and the
// checked source radio is one the test sets by hand.
const bySelector = {};
const sourceRadio = mkEl("source-checked");
for (const id of ["devices", "devices-list-panel", "device-form", "btn-new-device", "btn-pair",
  "device-form-title", "d-name", "d-server", "d-role", "d-code", "d-code-row", "d-tls", "d-tls-warn",
  "view-devices", "modal", "e-host", "e-name", "e-server", "e-probe", "e-rest"]) byId(id);

const calls = [];
const app = {
  Devices: async () => [{ name: "Workstation", server: "127.0.0.1:7677", credential: "Workstation",
    role: "control", encrypted: true, fingerprint: "SHA256:aa", pairedAt: "2026-10-01T15:11:26+09:00" }],
  UpdateDevice: async (name, rec) => { calls.push(["UpdateDevice", name, rec]); return { ...rec, name: rec.name }; },
  PairDevice: async (n, s, r, c, e) => { calls.push(["PairDevice", n, s, r, c, e]); return { name: n, server: s, role: r, encrypted: e }; },
  RemoveDevice: async (n) => { calls.push(["RemoveDevice", n]); },
  Status: async () => "ok", Profiles: async () => [], Session: async () => null,
  Logs: async () => [], Viewing: async () => false,
};

const sandbox = {
  console, setTimeout, clearTimeout, setInterval: () => 0, clearInterval: () => {},
  Event: class { constructor(t) { this.type = t; } },
  Date, JSON, Math, Object, Array, String, Number, Error, Promise, isNaN, parseFloat, parseInt,
  document: {
    getElementById: byId,
    querySelector: (sel) => (sel === 'input[name="source"]:checked' ? sourceRadio : (bySelector[sel] ||= mkEl(sel))),
    querySelectorAll: () => [],
    createElement: (tag) => (tag === "option" ? { value: "", textContent: "", title: "" } : mkEl(tag)),
    addEventListener: () => {},
    activeElement: null,
  },
  window: { go: { hub: { App: app } }, addEventListener: () => {}, runtime: null },
};
sandbox.globalThis = sandbox;
vm.createContext(sandbox);
vm.runInContext(src + "\n;globalThis.__x = {state, editDevice, newDevice, saveDevice, refreshDevices, showForm, fillDevices, chooseDevice, knownServers, returnToEditor, shapeEditor};", sandbox);
const { editDevice, newDevice, saveDevice, refreshDevices, showForm, state } = sandbox.__x;
let fails = 0;
const ok = (cond, label) => { console.log((cond ? "  PASS " : "  FAIL ") + label); if (!cond) fails++; };

await refreshDevices();
ok(state.devices.length === 1, "refreshDevices loads the devices");
ok(byId("devices-list-panel").classList.contains("hidden") === false, "a non-empty list is shown");
ok(byId("device-form").classList.contains("hidden") === true, "the form is hidden when there is a list");

// Clicking a row opens it for editing.
editDevice("Workstation");
ok(state.editingDevice === "Workstation", "clicking a row selects that device");
ok(byId("d-name").value === "Workstation", "the name field is filled in");
ok(byId("d-server").value === "127.0.0.1:7677", "the server field is filled in");
ok(byId("d-role").value === "control", "the role field is filled in");
ok(byId("d-tls").value === "on", "the encryption field is filled in");
ok(byId("device-form").classList.contains("hidden") === false, "the form is shown");
ok(byId("btn-pair").textContent === "Save device", "the button says Save device");
ok(byId("d-code-row").classList.contains("hidden") === true, "no pairing code is asked for when editing");

// Saving writes the changes back.
byId("d-name").value = "Laptop";
byId("d-role").value = "view";
byId("d-tls").value = "off";
await saveDevice();
const upd = calls.find((c) => c[0] === "UpdateDevice");
ok(!!upd, "saving an edited device calls UpdateDevice");
ok(upd && upd[1] === "Workstation", "the update is for the device that was clicked");
ok(upd && upd[2].name === "Laptop" && upd[2].role === "view" && upd[2].encrypted === false,
  "the changed values are what is saved");
ok(!calls.find((c) => c[0] === "PairDevice"), "editing does not pair a second time");

// The empty form is what "+ New device" opens.
newDevice();
ok(state.editingDevice === "", "new device leaves nothing being edited");
ok(byId("d-name").value === "" && byId("d-server").value === "", "the fields are cleared");
ok(byId("d-role").value === "control", "the role defaults to control");
ok(byId("d-tls").value === "on", "encryption defaults to enabled");
ok(byId("d-code-row").classList.contains("hidden") === false, "a pairing code is asked for");
ok(byId("btn-pair").textContent === "Pair device", "the button says Pair device");
ok(byId("device-form-title").textContent === "New device", "the heading says New device");

// And pairing still goes through PairDevice, not the update path.
byId("d-name").value = "Phone"; byId("d-server").value = "h:7677"; byId("d-code").value = "ABC";
await saveDevice();
ok(!!calls.find((c) => c[0] === "PairDevice"), "a new device is paired");

// An empty list shows the form instead, as the tab is supposed to.
state.devices = [];
showForm((state.devices || []).length === 0);
ok(byId("devices-list-panel").classList.contains("hidden") === true, "no devices: the list is hidden");
ok(byId("device-form").classList.contains("hidden") === false, "no devices: the form is shown");
ok(byId("btn-new-device").classList.contains("hidden") === true, "no devices: the button is hidden");

// ── the connection editor's Device select ──────────────────────────
const { fillDevices, chooseDevice, knownServers, returnToEditor } = sandbox.__x;
await refreshDevices();

// The select offers what this machine is paired with, and Other last.
byId("e-host").value = "";
fillDevices();
ok(byId("e-host").options.length >= 3, "the Device select has options");
ok(byId("e-host").options[0].value === "", "it starts with a placeholder");
ok(byId("e-host").options[1].value === "127.0.0.1:7677", "the paired host is offered");
ok(byId("e-host").options[1].textContent === "Workstation", "a device is shown by its name");
ok(byId("e-host").options.some((o) => o.value === "__other__"), "Other is offered");
ok(byId("e-host").options[byId("e-host").options.length - 1].value === "__other__", "Other is last");

// Choosing one fills in the address and the name.
byId("e-name").value = "";
byId("e-host").value = "127.0.0.1:7677";
chooseDevice();
ok(byId("e-server").value === "127.0.0.1:7677", "choosing a device sets the server");
ok(byId("e-name").value === "Workstation", "choosing a device fills in the name");
ok(state.wantedDevice === false, "choosing an existing device does not ask for a new one");

// Other asks for a device and keeps the draft.
byId("e-name").value = "My connection";
byId("e-host").value = "__other__";
await chooseDevice();
ok(state.wantedDevice === true, "Other asks for a device to be made");
ok(byId("e-name").value === "My connection", "the draft is not lost on the way");

// Coming back chooses the device just paired.
await refreshDevices();
returnToEditor("127.0.0.1:7677");
ok(state.wantedDevice === false, "coming back clears the request");
ok(byId("e-host").value === "127.0.0.1:7677", "coming back selects the device");
ok(byId("e-name").value === "Workstation", "coming back names it after the device");

// Giving up on the request leaves the form as it was.
byId("e-name").value = "My connection";
byId("e-host").value = "__other__";
await chooseDevice();
ok(state.wantedDevice === true, "Other asks again");
returnToEditor("");
ok(byId("e-host").value === "", "giving up selects nothing");
ok(byId("e-name").value === "My connection", "giving up keeps the name that was typed");
ok(byId("e-server").value === "", "giving up leaves no server behind");

// A value that is not offered is not silently selected.
fillDevices("nowhere:1");
ok(byId("e-host").value === "", "an unknown address selects nothing");

// ── what each Share choice shows ───────────────────────────────────
const { shapeEditor } = sandbox.__x;
const shown = (sel) => (bySelector[sel] ? bySelector[sel].style.display : "");

// The window source picks its display in its own picker; the Display
// row below it would ask for the very same thing a second time.
byId("e-displaykind").value = "existing";
sourceRadio.value = "window";
shapeEditor();
ok(shown(".only-window") === "contents", "window source shows its display and window pickers");
ok(shown(".only-display") === "none", "window source hides the second Display row");

// The other sources still get that row.
sourceRadio.value = "desktop";
shapeEditor();
ok(shown(".only-display") === "contents", "desktop shows the Display row");
ok(shown(".only-window") === "none", "desktop hides the window pickers");
ok(shown(".only-create") === "none", "an existing display shows no create fields");

sourceRadio.value = "app";
byId("e-displaykind").value = "create";
shapeEditor();
ok(shown(".only-display") === "contents", "app shows the Display row");
ok(shown(".only-create") === "contents", "a display to create shows its fields");

// A window lives on a display that already exists, so a leftover
// "Create a display" is not honoured for it.
sourceRadio.value = "window";
shapeEditor();
ok(byId("e-displaykind").value === "existing", "window source drops a leftover create-display choice");
ok(shown(".only-create") === "none", "and shows no create-display fields");

console.log(fails ? `\n${fails} FAILED` : "\nall passed");
process.exit(fails ? 1 : 0);
