#!/usr/bin/env bash
# Dump the TV's current UI tree and print visible texts, marking the focused/selected node with '>'.
# Usage: tools/ui.sh [keyevents...]   e.g. tools/ui.sh DPAD_DOWN DPAD_DOWN DPAD_CENTER
A="${ADB:-$HOME/Android/Sdk/platform-tools/adb} -s ${TV_HOST:?set TV_HOST to the TV IP}:5555"
for k in "$@"; do $A shell input keyevent "KEYCODE_$k"; sleep 0.6; done
sleep 1.5
$A shell 'uiautomator dump /sdcard/ui.xml >/dev/null 2>&1; cat /sdcard/ui.xml' | python3 -c '
import sys, xml.etree.ElementTree as ET
root = ET.fromstring(sys.stdin.read())
def walk(n, hot=False):
    hot = hot or n.get("focused") == "true" or n.get("selected") == "true"
    t = n.get("text") or n.get("content-desc")
    if t: print((">" if hot else " "), t)
    for c in n: walk(c, hot)
walk(root)'
