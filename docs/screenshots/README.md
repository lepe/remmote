# Screenshot capture notes

These screenshots show the actual `remmote-hub` and remote viewer built from
this repository. They are not mockups. Names and connection profiles are demo
data; no personal desktop or existing connection store was used.

| File | Content |
|---|---|
| `connections.png` | Native hub with three dummy profiles; **Demo workstation** is selected. |
| `session.png` | Live desktop session after detaching the viewer. |
| `demo-connection.png` | Viewer content from the connected desktop; calculator input was sent through the viewer. |

## Setup

The capture used two disposable Xvfb displays with Openbox:

- Viewer display: `:190`, 1000 × 720, with the hub maximized.
- Host display: `:191`, 800 × 520, with `xcalc` and a plain background.
- Server: `remmote-server -display :191 -listen 127.0.0.1:17677`.
- Configuration: a temporary `XDG_CONFIG_HOME`, separate from real profiles.
- Demo profile: desktop source, existing display `:191`, hybrid codec,
  server `127.0.0.1:17677`.
- Other profiles: `build.example.test:7677` and `lab.example.test:7677`.
  These reserved example names are placeholders, not working servers.

The demo listener was unencrypted and bound only to loopback. For connections
between machines, follow the README's TLS or SSH examples.

## Recapture

Build with `make build build-hub`. With Xvfb, Openbox, `xcalc`, and ImageMagick
installed, start two unused X displays at the sizes above. Set a temporary
`XDG_CONFIG_HOME` before launching the server and hub, and place dummy profiles
in its `remmote/profiles.d/` directory. The demo profile has this shape:

```json
{
  "name": "Demo workstation",
  "server": "127.0.0.1:17677",
  "spec": {
    "source": "desktop",
    "display": {"kind": "existing", "name": ":191"},
    "stream": {"codec": "hybrid"}
  },
  "viewer": {}
}
```

Run the hub on the viewer display, select the demo profile, and capture the
connections screen with `DISPLAY=:190 import -window root connections.png`.
Connect, interact with the calculator, and capture the viewer by its X window
ID (`xwininfo` can find it). Close the viewer and capture the Session tab to
show that the session remains live. Stop the demo processes and remove the
temporary configuration when finished.
