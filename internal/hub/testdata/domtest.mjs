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

const els = {};
const byId = (id) => (els[id] ||= mkEl(id));
for (const id of ["devices", "devices-list-panel", "device-form", "btn-new-device", "btn-pair",
  "device-form-title", "d-name", "d-server", "d-role", "d-code", "d-code-row", "d-tls", "d-tls-warn",
  "view-devices", "modal"]) byId(id);

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
    querySelector: () => mkEl("q"),
    querySelectorAll: () => [],
    createElement: (tag) => mkEl(tag),
    addEventListener: () => {},
    activeElement: null,
  },
  window: { go: { hub: { App: app } }, addEventListener: () => {}, runtime: null },
};
sandbox.globalThis = sandbox;
vm.createContext(sandbox);
vm.runInContext(src + "\n;globalThis.__x = {state, editDevice, newDevice, saveDevice, refreshDevices, showForm};", sandbox);
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

console.log(fails ? `\n${fails} FAILED` : "\nall passed");
process.exit(fails ? 1 : 0);
