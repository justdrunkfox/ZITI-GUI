# ZITI-GUI

[Русская версия](README.ru.md)

A tray + window GUI for **ziti-edge-tunnel** (the OpenZiti tunneler) on Linux.
Start/stop the tunneler service, enable/disable each identity, enroll new
identities from a JWT, and browse every Ziti service with its intercept
addresses — inspired by the OpenZiti Desktop Edge app from the
[macOS tunneler guide](https://netfoundry.io/docs/openziti/how-to-guides/tunnelers/macos/),
rebuilt for Linux.

Pure Go, no interpreters embedded: the window is [Gio](https://gioui.org)
(native Wayland **and** X11), the tray is `fyne.io/systray` (StatusNotifierItem
over D-Bus), file dialogs use `zenity` with `xdg-desktop-portal` as fallback.

## What you get

**Window** (opens on start; re-opening focuses the existing one):

- header with a live status dot and Start / Stop / Restart / *Enroll JWT…* buttons;
- identity cards on the left with on/off switches (persisted across restarts),
  per-identity status, service count, MFA/auth warnings;
- the selected identity's services on the right: name, **all** intercept
  addresses with ports, dial/bind mode. Click any address to copy the server
  name to the clipboard (if a service has several addresses, click the one you
  want);
- footer with versions, IPC state and a service-journal button.

**Tray** (StatusNotifierItem): colored icon (green = running, orange = running
but IPC unreachable, yellow = starting, gray = stopped, red = failed), the same
controls, per-identity submenus, diagnostics, journal, quit.

**Under the hood**

- identities are toggled live via the tunneler IPC (`on_off_identity`) and the
  choice is persisted, so a service restart re-applies it;
- contexts that fail to initialize at service start (e.g. the network wasn't
  ready yet) are detected automatically and re-initialized ~20 s later;
- single instance: launching the binary again focuses the already open window;
- every action is logged to the journal.

## Requirements

- Linux, `systemd` (the tunneler is expected to run as a **system service** —
  that's how the official `ziti-edge-tunnel` .deb/.rpm packages install it);
- Go **1.25+** to build;
- build deps for Gio (cgo bindings): `gcc`, `pkg-config` and the libraries
  below (runtime libs are almost always already installed);

| Debian / Ubuntu | Fedora | Arch |
|---|---|---|
| `libxkbcommon-dev libxkbcommon-x11-dev libx11-dev libx11-xcb-dev libxcursor-dev libxfixes-dev libwayland-dev libegl-dev` | `libxkbcommon-devel libxkbcommon-x11-devel libX11-devel libxcb-devel libXcursor-devel libXfixes-devel wayland-devel mesa-libEGL-devel` | `libxkbcommon libxkbcommon-x11 libx11 libxcb libxcursor libxfixes wayland mesa` |

- at runtime: `polkit` (+ an authentication agent) and **either** `zenity`
  **or** `xdg-desktop-portal` for the JWT file dialog; a tray host
  (GNOME/KDE panel, waybar, DankMaterialShell, …) to see the icon;
- `wl-clipboard` is optional (extra clipboard fallback under Wayland).

## Build & run

```sh
go build -o ziti-gui .
./ziti-gui                # tray + window
./ziti-gui --once         # one status snapshot as JSON (handy for debugging)
./ziti-gui --identity-dir ~/.ziti/identities --service ziti-edge-tunnel.service
./ziti-gui --version
```

## How it works

The GUI assumes the tunneler runs as a **systemd system service** (default
`ziti-edge-tunnel.service`, package layout: binary in `/opt/openziti/bin`,
identities in `/opt/openziti/etc/identities`, IPC socket in `/tmp/.ziti/`).
Everything is auto-detected and can be overridden with flags or
`~/.config/ziti-gui/config.json`:

```json
{
  "identity_dir": "/opt/openziti/etc/identities",
  "service": "ziti-edge-tunnel.service",
  "ziti_bin": "",
  "poll_sec": 3,
  "disabled": {}
}
```

- **service control** — `pkexec systemctl start/stop/restart …`;
- **statuses** — the tunneler's own `dump` command over its IPC socket, parsed
  tolerantly (current text-report format of v1.18.x plus older JSON layouts);
- **enable/disable** — `on_off_identity` live + persisted in the config file
  (written only after a successful call), re-applied after service restarts and
  late-loading contexts;
- **enrollment** — `add` (enroll + load into the running tunneler, token passed
  as content); when the service is stopped — `enroll` into a temp file, then a
  one-shot `pkexec install` into the identities directory and an automatic
  service restart;
- **delete** — IPC `delete` + removing the identity file via `pkexec`
  (two-click confirmation).

## First-run setup

Out of the box the tunneler's IPC and its identity directory belong to the
service user (`ziti`), so the GUI can't see them. The tray/window will offer
**“Set up access (password once)”**, which runs a single `pkexec` script that:

1. adds your user to the `ziti` group (fully effective after re-login);
2. grants ACLs on `/tmp/.ziti`, its sockets and the identities directory for
   the current session;
3. installs a polkit rule
   (`/etc/polkit-1/rules.d/50-ziti-gui.rules`) allowing
   start/stop/restart of the tunneler service **without a password**.

After that the tray is fully self-service: no prompts on everyday actions.

## Autostart

Recommended: a systemd **user** unit (see `deploy/ziti-gui.service`, adjust the
binary path):

```sh
mkdir -p ~/.config/systemd/user
cp deploy/ziti-gui.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now ziti-gui.service
journalctl --user -u ziti-gui -f     # GUI logs
```

This way the GUI starts with the session, survives crashes
(`Restart=always`), doesn't inherit `no_new_privs` from sandboxed shells (a
setuid `pkexec` would refuse to work — the GUI will tell you if that happens),
and the tray's *Quit* item stops the unit so it isn't resurrected.
`spawn-at-startup` in niri's `config.kdl` or a DE autostart entry work too.

## DNS note

The tunneler intercepts DNS for Ziti names (default range `100.64.0.0/10`).
Two pitfalls worth knowing:

- it needs a real **upstream** for everything else; if your `resolv.conf` only
  points at the interceptor (some tools do that), name resolution loops and
  dies. Fix: add `--dns-upstream=<your LAN DNS>` to the service, e.g. a
  drop-in:

  ```ini
  # /etc/systemd/system/ziti-edge-tunnel.service.d/dns.conf
  [Service]
  ExecStart=
  ExecStart=/opt/openziti/bin/ziti-edge-tunnel run --verbose=info \
    --dns-ip-range=100.64.0.1/10 --dns-upstream=192.168.1.1 \
    --identity-dir=/opt/openziti/etc/identities
  ```

- tools that force their own nameserver into NetworkManager's
  `[global-dns-domain-*]` (e.g. a leftover from another VPN client) can point
  the whole system at the interceptor address; when the tunneler is stopped,
  DNS dies with it. Listing your LAN DNS as a second server there restores a
  fallback.

## Troubleshooting

- **No tray icon** — your bar must support StatusNotifierItem (waybar's
  `tray` module, GNOME/KDE panels, DankMaterialShell, …). A stale duplicate
  icon after an unclean kill is shell-side cache: restart your bar
  (`dms restart`, `pkill waybar && waybar &`, …).
- **`wl_display.connect failed`** — the GUI started before the graphical
  session. The app discovers the Wayland socket itself and retries; on X11
  it just uses `DISPLAY`.
- **A context didn't come up after a restart** — the GUI re-initializes
  wedged contexts automatically (~20 s); a manual off/on toggle on its card
  does the same.
- **“IPC unreachable”** — run *Set up access*; it's a permissions issue
  (group `ziti` + ACLs), not a tunnel fault.

## Debugging

```sh
./ziti-gui --once                    # status snapshot, no tray
go test ./...                        # parser tests
journalctl --user -u ziti-gui -f     # every GUI step is logged
go run ./porttest                    # raw xdg-desktop-portal FileChooser probe
```

## Project layout

```
main.go     flags, config, single-instance lock
tray.go     tray menu and actions
window.go   Gio window (identities, services, copy-to-clipboard)
state.go    status model, dump parsing, IPC actions
ziti.go     systemd / ziti-cli wrappers
portal.go   D-Bus: file chooser, notifications
setup.go    first-run access setup, diagnostics
icons.go    tray icons drawn in code
deploy/     systemd user unit
porttest/   portal debug probe (not needed at runtime)
```

## License

[Apache License 2.0](LICENSE)
