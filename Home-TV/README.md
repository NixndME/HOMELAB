# Home-TV: Android TV debloat

Xiaomi **MiTV-MOOQ0** (croods, Android TV 10), network ADB on port 5555. Based on
[Gathin23/tv-vibehack](https://github.com/Gathin23/tv-vibehack) (`tools/`, `launcher/`, `examples/` copied from it, MIT, see `LICENSE-tv-vibehack`).

## What was changed (2026-10-18)

| Change | How | Undo |
|---|---|---|
| 27 ad/telemetry/PatchWall/unused-app packages disabled | `examples/xiaomi-mitv-mooq0/debloat.txt` | `tools/restore.sh` |
| Stock launcher (sponsored rows) replaced by `com.gathin.tvhome` | `launcher/` | `tools/restore.sh` |
| Ad-free YouTube: SmartTube (`org.smarttube.stable`) in dock, official YouTube kept in grid | GitHub release APK | `adb uninstall org.smarttube.stable` |
| System-wide ad/tracker DNS: AdGuard DNS (Private DNS) | `settings put global private_dns_*` | `tools/restore.sh` |
| VLC (`org.videolan.vlc`) from Play Store, ad-free media player for USB/network files | Play Store | uninstall in Settings → Apps |
| Immich TV (`nl.giejay.android.tv.immich`, unofficial, GPL-3.0) from Play Store: photos + screensaver | Play Store | uninstall in Settings → Apps |
| SmartTube: preset 1080p 60fps VP9 (panel is 1080p60 SDR), SponsorBlock auto-skip (default) | in-app settings | SmartTube → Settings → Player |
| Sun NXT re-enabled (in use) | `pm enable` | — |
| Animations 0.5x | `settings put global *_animation_scale 0.5` | `tools/restore.sh` |

Free RAM after: ~930 MB (was ~340 MB; PatchWall alone took ~200 MB).

## Commands

```bash
python3 -m venv tools/venv && tools/venv/bin/pip install -r tools/requirements.txt   # once
export TV_HOST=<TV_IP>
tools/venv/bin/python -I tools/tv.py --probe
tools/disable.sh examples/xiaomi-mitv-mooq0/debloat.txt     # re-apply
tools/restore.sh examples/xiaomi-mitv-mooq0/debloat.txt     # undo everything
tools/venv/bin/python -I tools/tv.py 'pm enable <package>'  # re-enable one app
```

Rebuild launcher (dock order: `launcher/app/src/main/res/values/config.xml`):

```bash
export JAVA_HOME=~/.local/share/mise/installs/java/17 ANDROID_HOME=~/Android/Sdk
export JAVA_TOOL_OPTIONS=-XX:-UseContainerSupport   # JDK 17.0.2 crashes on cgroup v2 without this
launcher/build-and-install.sh
```

On Linux the real `adb` works fine and is faster for big APKs:
`ADB_VENDOR_KEYS=tools/adbkey/adbkey ~/Android/Sdk/platform-tools/adb connect <TV_IP>:5555`.

## Gotchas hit here
- First port scan with a 1 s timeout missed the TV; mDNS (`avahi-browse -rt _adb._tcp`) found it.
- No `~/.android/debug.keystore`: created with `keytool` (alias `androiddebugkey`, pass `android`).
- Firmware updates may re-enable packages or the stock launcher: re-run `disable.sh` and `build-and-install.sh`.
