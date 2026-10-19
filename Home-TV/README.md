# Home-TV: Android TV debloat

Xiaomi **MiTV-MOOQ0** (croods, Android TV 10), network ADB on port 5555. Based on
[Gathin23/tv-vibehack](https://github.com/Gathin23/tv-vibehack) (`tools/`, `launcher/`, `examples/` copied from it, MIT, see `LICENSE-tv-vibehack`).

## What was changed (2026-10-18)

| Change | How | Undo |
|---|---|---|
| 27 (+5 on 2026-10-19) ad/telemetry/PatchWall/unused-app packages disabled | `examples/xiaomi-mitv-mooq0/debloat.txt` | `tools/restore.sh` |
| Stock launcher (sponsored rows) replaced by `com.gathin.tvhome` | `launcher/` | `tools/restore.sh` |
| Ad-free YouTube: SmartTube (`org.smarttube.stable`) in dock, official YouTube kept in grid | GitHub release APK | `adb uninstall org.smarttube.stable` |
| (2026-10-19) Official YouTube removed (`pm uninstall --user 0 com.google.android.youtube.tv`, update + data gone, system copy hidden). SmartTube signed in to the Google account (subscriptions/history/recommendations come with the account) and is now the handler for all YouTube links. Phone casting: use YouTube app → Settings → Watch on TV → *Link with TV code* (code from SmartTube → Settings → Remote control) | `tools/tv.py` | `tools/tv.py 'cmd package install-existing com.google.android.youtube.tv'` |
| System-wide ad/tracker DNS: AdGuardHome on the Pi via router DHCP (Private DNS **off**, see `../AdGuardHome`) | `settings put global private_dns_mode off` | `settings put global private_dns_mode opportunistic` |
| VLC (`org.videolan.vlc`) from Play Store, ad-free media player for USB/network files | Play Store | uninstall in Settings → Apps |
| Immich TV (`nl.giejay.android.tv.immich`, unofficial, GPL-3.0) from Play Store, **disabled** until the Immich server runs | `debloat.txt` | `tools/tv.py 'pm enable nl.giejay.android.tv.immich'` |
| SmartTube: preset 1080p 60fps VP9 (panel is 1080p60 SDR), SponsorBlock auto-skip (default) | in-app settings | SmartTube → Settings → Player |
| Sun NXT re-enabled (in use) | `pm enable` | — |
| (2026-10-19) Old duplicate Xiaomi builds `com.suntv.sunnxt.mitv`, `com.xiaomitv.sonyliv` and dead Voot disabled | `debloat.txt` | `tools/restore.sh` |
| (2026-10-19) Firmware OTA `com.xiaomi.mitv.updateservice` disabled so updates stop re-enabling bloat. **No more security/firmware updates**; re-enable to update, then run `check.sh --fix` | `debloat.txt` | `tools/tv.py 'pm enable com.xiaomi.mitv.updateservice'` |
| (2026-10-19) JioCinema (`com.jio.media.stb.ondemand`) disabled: merged into JioHotstar | `debloat.txt` | `tools/restore.sh` |
| (2026-10-19) Dock: Netflix, Prime Video, SmartTube, JioHotstar, SonyLIV, ZEE5 (6 columns, tiles shrunk to 124x70dp in `launcher/.../values/dimens.xml`) | `launcher/` | edit `config.xml` + `dimens.xml`, rebuild |
| (2026-10-19) LocalSend 1.18.2 (`org.localsend.localsend_app`, open source, LAN-only): send files phone/PC → TV. Open it on the TV, then pick the TV in LocalSend on the phone. Storage permission pre-granted | GitHub release `android-arm32v7.apk` (TV is armeabi-v7a), `b64push.py` + `pm install` | uninstall in Settings → Apps |
| (2026-10-19) SmartTube: SponsorBlock auto-skips (with notification) sponsor, intro, endcards, subscribe reminders, self-promo; Shorts hidden everywhere and removed from the left menu | in-app settings | SmartTube → Settings → SponsorBlock / General |
| Animations 0.5x | `settings put global *_animation_scale 0.5` | `tools/restore.sh` |

Free RAM after: ~930 MB (was ~340 MB; PatchWall alone took ~200 MB).

## Commands

```bash
python3 -m venv tools/venv && tools/venv/bin/pip install -r tools/requirements.txt   # once
export TV_HOST=<TV_IP>
tools/venv/bin/python -I tools/tv.py --probe
tools/disable.sh examples/xiaomi-mitv-mooq0/debloat.txt     # re-apply
tools/check.sh examples/xiaomi-mitv-mooq0/debloat.txt       # report drift (re-enabled packages, launcher not home); exit 1 on drift
tools/check.sh examples/xiaomi-mitv-mooq0/debloat.txt --fix # re-apply debloat + launcher if drifted
tools/restore.sh examples/xiaomi-mitv-mooq0/debloat.txt     # undo everything
tools/venv/bin/python -I tools/tv.py 'pm enable <package>'  # re-enable one app
```

Rebuild launcher (dock order and columns: `launcher/app/src/main/res/values/config.xml`, tile size: `dimens.xml`):

```bash
export JAVA_HOME=~/.local/share/mise/installs/java/17 ANDROID_HOME=~/Android/Sdk
export JAVA_TOOL_OPTIONS=-XX:-UseContainerSupport   # JDK 17.0.2 crashes on cgroup v2 without this
launcher/build-and-install.sh
```

On Linux the real `adb` works fine and is faster for big APKs:
`ADB_VENDOR_KEYS=tools/adbkey/adbkey ~/Android/Sdk/platform-tools/adb connect <TV_IP>:5555`.

## Gotchas hit here
- SmartTube menus: `input keyevent DPAD_*` over network ADB arrives as a long press and jumps several rows; `input tap x y` on the node's bounds from `uiautomator dump` is reliable.
- First port scan with a 1 s timeout missed the TV; mDNS (`avahi-browse -rt _adb._tcp`) found it.
- No `~/.android/debug.keystore`: created with `keytool` (alias `androiddebugkey`, pass `android`).
- Firmware updates may re-enable packages or the stock launcher: run `tools/check.sh <list> --fix` (it only re-sets home; if the launcher app itself is gone, run `build-and-install.sh`).
