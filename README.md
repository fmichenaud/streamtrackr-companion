# StreamTrackr Companion

A lightweight Windows tray app that detects Steam achievement unlocks in
real time and pushes them to your [StreamTrackr](https://streamtrackr.com)
account — overlay, Twitch chat, Discord, stats, all updated within a
second of the unlock instead of waiting on Valve's Web API cache.

End-to-end latency from achievement unlock to your stream overlay:
**under one second**.

---

## What it does

- Watches the currently-running Steam game by reading Steam's own local
  cache files (`<Steam>\appcache\stats\UserGameStats_*.bin`).
- On a new unlock, posts the event to the StreamTrackr backend over
  HTTPS, which fans it out to your overlay (SSE), Twitch chat, Discord
  webhooks, session stats and goals.
- Runs quietly in the Windows tray with a status icon that reflects what
  it's doing (signed in, waiting for a game, actively tracking, error).
- Auto-updates itself in the background — new versions are downloaded,
  SHA-256-verified, and applied on the next restart.

## Is it safe to run while I stream?

**Yes — by architecture, not just by policy.**

The companion never:

- Loads `steam_api.dll` / `steam_api64.dll` or any other Steam DLL
- Calls `SteamAPI_Init`, `SetAchievement`, or any Steamworks function
- Opens a process handle on Steam or any game
- Reads or writes game process memory
- Injects code, hooks, or overlays into anything
- Bundles or ships any Valve binary

What it actually does, technically:

1. Reads `HKCU\Software\Valve\Steam\RunningAppID` (a Windows registry
   `DWORD`) every 5 seconds to know which game is active.
2. Reads two files Steam writes for its own UI:
   - `<Steam>\appcache\stats\UserGameStatsSchema_<appid>.bin` — the
     list of achievements + their bit positions
   - `<Steam>\appcache\stats\UserGameStats_<sid>_<appid>.bin` — your
     current unlock state
3. POSTs `{ appId, achievement: { apiName } }` to `api.streamtrackr.com`
   over HTTPS when it sees a new bit flip from 0 to 1.

That's the whole loop. Every file it touches is a regular file owned
by your Windows user account, in your own user-writable directory.
There is no scenario where any of this is visible to a kernel-level
anti-cheat — the companion is just a userspace process reading config
files, the same way Steam itself, a backup tool, or File Explorer
would.

For comparison: [Steam Achievement Notifier](https://store.steampowered.com/app/1722730/),
which is sold on the Steam Store, does call `SteamAPI_Init` against
the running game to read achievements. This companion takes the
stricter route and avoids the SDK entirely — strictly less invasive
than a product Valve has approved on their own storefront.

## Requirements

- Windows 10 or 11 (64-bit)
- Steam installed and logged in to the account you stream from
- A StreamTrackr Pro account (the companion is a Pro feature)

## Install

Grab the latest installer from the
[releases page](https://github.com/fmichenaud/streamtrackr-companion/releases/latest):

[`streamtrackr-companion-setup.exe`](https://github.com/fmichenaud/streamtrackr-companion/releases/latest/download/streamtrackr-companion-setup.exe)

Run it. The installer drops the app into `%LocalAppData%\Programs\StreamTrackr Companion`
(no admin prompt) and offers to launch at Windows startup. On first
launch, a browser tab opens to pair the companion with your StreamTrackr
account.

> **About the SmartScreen warning**: until this release is code-signed,
> Windows may show a "Windows protected your PC" prompt on first run.
> Click *More info → Run anyway* to continue. The warning fades on its
> own as more users install the same binary.

## Usage

Once paired and running, you don't have anything to do — the app sits
in the tray and works while you play.

Right-click the tray icon for status info, sign-out, manual update
check, and "Open dashboard" / "Quit".

## Build from source

The companion is written in Go (cgo is only needed for the system tray
on Windows). The build pipeline produces a single Windows binary plus
an NSIS installer.

### Prerequisites

- **Cross-compiling from macOS/Linux**: `brew install mingw-w64 makensis`
  (or the equivalent on your distro)
- **Native on Windows**: install [Go 1.26+](https://go.dev/dl/) and
  [NSIS](https://nsis.sourceforge.io/Download). CGO needs a working
  MinGW or MSVC toolchain — TDM-GCC is the easiest on Windows.

### Commands

```sh
# Build the .exe only
make build

# Build the .exe + NSIS installer
make installer VERSION=0.1.0

# macOS sanity build (no tray, no installer — just verifies the code compiles)
make mac

# Clean build artefacts
make clean
```

The `VERSION` is baked into the binary via `-ldflags` and surfaced in
the tray menu. Without it, the build is tagged `dev` and skips the
auto-update polling.

### How it works (no Steamworks SDK)

Steam keeps a per-user binary cache of achievement state on disk:

```
<Steam>\appcache\stats\UserGameStatsSchema_<appid>.bin
<Steam>\appcache\stats\UserGameStats_<steamid3>_<appid>.bin
```

Both are in Valve's BinaryKV format. The schema enumerates achievements
as `(statID, bit, apiName)` triplets; the user stats file holds the
current `int32 Value` of each stat. An achievement is unlocked iff its
bit is set in the matching stat's value.

The companion polls the stats file's mtime 4× per second. Steam
rewrites the whole file every time a game calls `StoreStats` — which
happens immediately when an achievement notification pops in-game. When
the mtime advances, we re-parse, diff against the last snapshot, and
emit each newly-unlocked achievement as an HTTP push.

End result: same sub-second latency the SDK-based design used to give
us, with **zero SDK dependency**, **zero bundled DLL**, **zero process
touched** — purely reading files that the OS already grants you read
access to as your own user.

### Repo layout

```
.
├── main.go                  Entry point (subcommand dispatch + watcher loop)
├── tray_*.go                Tray UI: status rows, menu, refresh ticker
├── tray_icon_status.go      Status badge overlay generated at startup
├── update_dialog_windows.go Interactive update flow with native dialogs
├── login.go                 OAuth-style loopback callback flow
├── pusher.go                HTTP push → /api/companion/steam/unlock
├── updater.go               Auto-update: manifest fetch + SHA-256 + apply
├── token_store.go           Token persisted at %AppData%\StreamTrackr
├── autostart_windows.go     HKCU\Run registry toggle
├── welcome_windows.go       First-launch native MessageBox
├── dialog_windows.go        Reusable MessageBox helpers
├── kv_binary.go             Valve BinaryKV (binary VDF) parser
├── steam_user.go            Active steamID3 resolution via loginusers.vdf
├── stats_reader.go          Schema + user-stats cache reader + bitmask math
├── stats_watcher.go         mtime polling primitive
├── assets/icon.ico          Tray + window icon (multi-res 16/32/48)
├── installer/installer.nsi  NSIS Modern UI installer (per-user, no UAC)
└── .github/workflows/       CI: builds + publishes a release on tag push
```

## Debugging

Run `streamtrackr-companion --dump-stats <appid>` from a terminal on any
machine with Steam installed to dump the currently-parsed schema +
achievement state for a given app. Output looks like:

```
steam-path : C:\Program Files (x86)\Steam
steamID3   : 138499704 (steamID64 76561198098765432)
appid      : 346900
schema     : C:\Program Files (x86)\Steam\appcache\stats\UserGameStatsSchema_346900.bin
stats      : C:\Program Files (x86)\Steam\appcache\stats\UserGameStats_138499704_346900.bin

🏆  stat=1      bit=0   ACH_FIRST_KILL
    stat=1      bit=1   ACH_HEADSHOT
🏆  stat=1      bit=2   ACH_VETERAN
...

Total: 12/47 unlocked
```

If anything mismatches what Steam's own UI shows, file an issue — that's
the regression surface we care about most.

## Security notes

- All requests to the backend use a long-lived bearer token persisted
  on disk at `%AppData%\StreamTrackr\token.json` (mode 0600). The token
  is SHA-256 hashed at rest on the server side.
- The pairing flow uses an OAuth-style auth code with a 5-minute TTL,
  bound to a single-use 32-hex `state` parameter, and only accepts
  loopback redirect URIs (`http://127.0.0.1:<port>/cb`).
- The companion never loads any native code at runtime: no Steamworks
  DLL, no SDK initialisation, no hook into any Steam process.
- The auto-updater is restricted to two GitHub-controlled hostnames
  (`github.com`, `objects.githubusercontent.com`) over HTTPS, and every
  downloaded binary is SHA-256-verified against a manifest before being
  applied.

## License

[MIT](./LICENSE) — see the LICENSE file for the full text.

## Related

- [StreamTrackr](https://streamtrackr.com) — the parent product (OBS
  overlay + Twitch / Discord integrations + stats / goals)
- [Steam Achievement Notifier](https://store.steampowered.com/app/1722730/)
  — uses the same Steamworks SDK pattern, available on Steam
