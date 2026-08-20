# mangomon

Binary Cache:

- Cache: <https://cache.ysun.co>
- Key: `cache.ysun.co-1:WxPYwT5g3kt9XhUhHPpNLZKI9HIOsVVAuqSHpok8Qt4=`

mangomon is a fork of [nirimon](https://github.com/stepbrobd/nirimon) (itself a
fork of [hyprmon](https://github.com/erans/hyprmon) by Eran Sandler, Apache 2.0)
that is intended to only work for [mango](https://github.com/mangowm/mango),
just like nirimon is only built for Niri and hyprmon only for Hyprland. The Niri
IPC layer is replaced by
[wlr-output-management](https://wayland.app/protocols/wlr-output-management-unstable-v1),
which mango implements and whose reference client `wlr-randr` mango's own docs
recommend. The profile JSON format is preserved, so existing nirimon or hyprmon
profiles keep working if you copy `~/.config/nirimon` (or `~/.config/hyprmon`)
to `~/.config/mangomon`.

Note that mangomon does not write to your mango config file. Monitor application
is one atomic `wlr-randr` invocation, which is runtime-temporary: a mango
restart reverts outputs to whatever your `monitorrule` lines say. Persistence
belongs to the profile json files in `~/.config/mangomon/profiles/`, applied at
startup via `exec-once` (see the [Mango](#mango) section), or to `monitorrule`
lines you write yourself.

## Installation

To run mangomon in an ephemeral environment:

```sh
nix run github:stepbrobd/mangomon
```

Or if you are not using Nix/NixOS, build from source:

```sh
git clone --depth=1 https://github.com/stepbrobd/mangomon
pushd mangomon
go build -ldflags="-s -w -X main.Version=$(cat version.txt)"
sudo mv mangomon /usr/local/bin/
popd
```

Or if you must:

```sh
go install -ldflags="-s -w -X main.Version=0-unstable-$(date -u +%Y-%m-%d)+go" ysun.co/mangomon@latest
```

When building from source, `wlr-randr` is required and `wl-mirror` is optional
(mirroring only); install both yourself. The Nix package wraps both in
automatically.

## Usage

```sh
mangomon                    # main TUI
mangomon profiles           # profile selection menu
mangomon -profile <profile> # apply a saved profile directly
mangomon -list-profiles     # list profile names (active marked with *)
mangomon -active-profile    # print the name of the currently matching profile
```

### Keyboard

Main UI:

| Key               | Action                                      |
| ----------------- | ------------------------------------------- |
| Arrow keys / hjkl | Move selected monitor by the grid step      |
| Shift + arrows    | Move by 10x grid step                       |
| Tab / Shift-Tab   | Cycle through monitors                      |
| G                 | Cycle grid size (1, 8, 16, 32, 64 px)       |
| L                 | Cycle snap mode (Off, Edges, Centers, Both) |
| R                 | Open scale picker                           |
| F                 | Open mode (resolution + refresh) picker     |
| M                 | Open mirror configuration (needs wl-mirror) |
| C / D             | Open advanced display settings dialog       |
| Enter / Space     | Toggle the selected monitor on/off          |
| A                 | Apply the current layout to mango now       |
| Z                 | Revert to previous configuration            |
| O                 | Open the profiles page                      |
| P                 | Save current layout as a named profile      |
| ?                 | Show help                                   |
| Q / Ctrl-C        | Quit                                        |

### Mouse

| Action       | Effect                       |
| ------------ | ---------------------------- |
| Left click   | Select monitor               |
| Left drag    | Move monitor (with snapping) |
| Right click  | Toggle monitor on/off        |
| Scroll wheel | Adjust monitor scale         |

### Profiles

Profiles are json files in `~/.config/mangomon/profiles/`. They store the full
monitor layout (resolution, refresh, position, scale, transform, vrr, and
EDID-derived identifiers for stable matching across port reassignments).

```sh
mangomon -profile home
mangomon -profile work
mangomon -profile docked
mangomon profiles        # interactive menu
```

VRR maps to the protocol's adaptive sync: on/off. The legacy hyprmon value
"fullscreen-only" applies as on.

### Mirroring

Mango has no native output mirroring the way Hyprland does. mangomon keeps
hyprmon's mirror picker (press `M`) and the exact same profile schema, but
applies the mirror by spawning
[wl-mirror](https://github.com/Ferdi265/wl-mirror): for a monitor set to mirror
another, mangomon launches `wl-mirror --fullscreen-output <target> <source>`,
which captures the source output and shows it fullscreen on the target.

wl-mirror must be on `PATH`. The Nix package wraps it in automatically; if you
build from source, install wl-mirror yourself. When wl-mirror is missing the
mirror picker still works and the choice is saved to the profile json, but it is
not applied (the picker says so), so a profile stays portable to hyprmon.

Because the mirror is an ordinary fullscreen Wayland window and not a
compositor-level clone, it behaves differently from Hyprland's native mirror.
Keep these gotchas in mind:

- It is just a fullscreen window on the target output. mango does not pin you to
  it: you can switch tags, focus other windows, or move the wl-mirror window
  away, and the target stops showing the mirror until you switch back.
- The mirror process is detached and keeps running after mangomon exits (so a
  `mangomon -profile ...` from a keybind or hotplug hook leaves a working
  mirror). mangomon tracks it in `$XDG_RUNTIME_DIR/mangomon/mirrors.json` and
  tears it down on the next apply that disables the mirror. It is also cleared
  on logout, or you can `pkill wl-mirror` by hand.
- Scaling uses wl-mirror's `fit` mode: the whole source is always shown,
  letterboxed when the aspect ratios differ. wl-mirror cannot aspect-distort to
  fill the way Hyprland does, so expect black bars on mismatched ratios instead
  of stretching.
- If the source output is unplugged or turned off, wl-mirror exits; re-apply to
  restart the mirror once the source is back.
- Only active, non-mirrored monitors can be a source, and circular mirrors are
  prevented, same as hyprmon.

### Mango

To apply a profile at startup, and to switch profiles from a keybind, add to
your mango config:

```ini
exec-once=mangomon -profile docked
bind=SUPER,F1,spawn,mangomon -profile home
bind=SUPER,F2,spawn,mangomon -profile work
```

If you want a layout to survive without mangomon running at startup, copy it
into `monitorrule` lines instead (`wlr-randr` shows the values mangomon
applied):

```ini
monitorrule=name:^eDP-1$,width:2880,height:1920,refresh:120,x:0,y:0,scale:1.5,rr:0,vrr:0
```
