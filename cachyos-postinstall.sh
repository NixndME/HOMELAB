#!/usr/bin/env bash
# ============================================================================
# CachyOS Setup — single script, run top-to-bottom.
#
# PART 1 (always runs, idempotent): snapshot, system update, SSH server,
#         RDP server (KDE native KRDP — works with macOS Windows App / RDP
#         clients), KVM/libvirt nested-virt lab stack, UFW fixes.
# PART 2 (interactive, safe to re-run anytime on its own): checkbox picker
#         for discretionary applications — check to install, uncheck an
#         installed item to remove it.
#
# Run as your normal user (NOT root, NOT with sudo). Script uses sudo internally.
# ============================================================================

set -uo pipefail   # no -e: one failed step shouldn't abort the whole run

# ---------------------------------------------------------------------------
# PART 1: FOUNDATION
# ---------------------------------------------------------------------------

echo "==> [1/9] Baseline snapshot"
sudo snapper -c root create -d "setup run $(date +%F_%H%M)"

echo "==> [2/9] Full system update"
sudo pacman -Syu --noconfirm

echo "==> [3/9] Bootstrap: paru, dialog, curl, openssl"
if ! command -v paru &>/dev/null; then
  sudo pacman -S --needed --noconfirm base-devel git
  git clone https://aur.archlinux.org/paru.git /tmp/paru-build
  (cd /tmp/paru-build && makepkg -si --noconfirm)
  rm -rf /tmp/paru-build
fi
sudo pacman -S --needed --noconfirm dialog curl openssl kconfig

echo "==> [4/9] SSH server (remote terminal access)"
sudo pacman -S --needed --noconfirm openssh
sudo systemctl enable --now sshd.service

echo "==> [5/9] RDP server — KDE native KRDP (macOS: use 'Windows App' RDP client)"
sudo pacman -S --needed --noconfirm krdp
mkdir -p "$HOME/.local/share/krdpserver"
CERT_PATH="$HOME/.local/share/krdpserver/krdp.crt"
KEY_PATH="$HOME/.local/share/krdpserver/krdp.key"
if [[ ! -f "$CERT_PATH" ]]; then
  openssl req -nodes -new -x509 -keyout "$KEY_PATH" -out "$CERT_PATH" -days 365 -batch
fi
kwriteconfig6 --file krdpserverrc --group General --key Certificate "$CERT_PATH"
kwriteconfig6 --file krdpserverrc --group General --key CertificateKey "$KEY_PATH"
kwriteconfig6 --file krdpserverrc --group General --key SystemUserEnabled true
systemctl --user enable --now app-org.kde.krdpserver.service
echo "  If this doesn't connect: System Settings -> Remote Desktop -> enable RDP there instead (GUI is the documented fallback)."

echo "==> [6/9] Firewall: open SSH (22) and RDP (3389)"
if command -v ufw &>/dev/null && sudo ufw status | grep -q "Status: active"; then
  sudo ufw allow 22/tcp
  sudo ufw allow 3389/tcp
  sudo ufw reload
else
  echo "  ufw inactive/not installed — skip (open these ports manually once ufw is enabled)"
fi

echo "==> [7/9] Virtualization / KVM lab stack"
sudo pacman -S --needed --noconfirm \
  qemu-full qemu-img libvirt virt-manager virt-viewer edk2-ovmf \
  dnsmasq bridge-utils vde2 openbsd-netcat ebtables iptables-nft swtpm

if ! command -v qemu-img &>/dev/null; then
  echo "  ERROR: qemu-img still not on PATH after install. Check: pacman -Qi qemu-img"
  exit 1
fi
echo "  qemu-img confirmed: $(command -v qemu-img)"

# libvirtd caches its qemu-img/capability detection at daemon startup. A plain
# `restart` doesn't always force a clean re-probe (this is a well-known bug class,
# not Arch-specific - same symptom reported on NixOS, Guix, MX Linux etc. whenever
# libvirtd started before qemu-img existed, or its stale on-disk capability cache
# survives a restart). Force a genuinely clean state: stop, clear the cache, start.
sudo systemctl stop libvirtd.service
sudo rm -rf /var/cache/libvirt/qemu/capabilities/*
sleep 1
sudo systemctl enable --now libvirtd.service

# Verify libvirtd actually sees qemu-img now, not just that the binary exists -
# these are different things, and this is the actual failure mode to catch.
sleep 2
if ! sudo virsh -c qemu:///system capabilities &>/dev/null; then
  echo "  ERROR: libvirtd did not come back up cleanly. Check: sudo systemctl status libvirtd"
  exit 1
fi
echo "  libvirtd confirmed responsive with qemu-img capability re-probed."

sudo usermod -aG libvirt,kvm "$USER"
sudo virsh net-autostart default || true
sudo virsh net-start default || true
echo "options kvm_amd nested=1" | sudo tee /etc/modprobe.d/kvm_amd.conf >/dev/null
sudo modprobe -r kvm_amd 2>/dev/null || true
sudo modprobe kvm_amd

if command -v ufw &>/dev/null && sudo ufw status | grep -q "Status: active"; then
  sudo ufw allow in on virbr0
  sudo ufw route allow in on virbr0
  sudo ufw route allow out on virbr0
  sudo ufw reload
fi

echo "==> [8/9] Dev toolchain: Python, Go, jq, yq"
sudo pacman -S --needed --noconfirm python go jq yq

echo "==> Flatpak + Flathub (needed for Bambu Studio, avoids AUR webkit2gtk source builds)"
sudo pacman -S --needed --noconfirm flatpak
flatpak remote-add --if-not-exists flathub https://dl.flathub.org/repo/flathub.flatpakrepo

mkdir -p "$HOME/.config/environment.d"
if [[ ! -f "$HOME/.config/environment.d/flatpak-xdg.conf" ]]; then
  cat > "$HOME/.config/environment.d/flatpak-xdg.conf" <<'ENVCONF'
XDG_DATA_DIRS=/var/lib/flatpak/exports/share:/usr/local/share/:/usr/share/
ENVCONF
  echo "  XDG_DATA_DIRS configured so Flatpak apps show up in the KDE app menu."
  echo "  NOTE: takes effect on next login/reboot only (systemd environment.d is read once per session)."
fi

echo "==> [9/9] fish shell: LIBVIRT_DEFAULT_URI"
if command -v fish &>/dev/null; then
  fish -c "set -Ux LIBVIRT_DEFAULT_URI qemu:///system"
fi

echo ""
echo "==> Foundation complete. Log out/in for group membership (libvirt/kvm) to take effect."
echo "==> Verify nested virt after relogin: cat /sys/module/kvm_amd/parameters/nested  (expect: 1)"
echo ""

# ---------------------------------------------------------------------------
# PART 2: INTERACTIVE APPLICATION CHECKLIST
# ---------------------------------------------------------------------------

# ---- Catalog: tag | description | method(pacman/aur/custom) | package ----
TAGS=(claude-desktop antigravity zed claude-code gemini-cli opencode opera obsidian freelens terraform kubectl helm ansible argocd k9s kubectx podman podman-desktop kind k3s teams outlook usbimager bambu-studio beeper zen-browser github-cli vocalinux firefox vlc)
DESCS=(
  "Claude Desktop [AUR]"
  "Antigravity IDE - Google AI IDE, VS Code fork, Gemini 3.1 Pro built in [AUR, proper package]"
  "Zed - native Rust editor with ACP agent support [official]"
  "Claude Code CLI [AUR, tracks official binary]"
  "Gemini CLI [official]"
  "OpenCode - model-agnostic AI agent (Claude/GPT/Gemini/local) [official]"
  "Opera browser [AUR]"
  "Obsidian notes [official]"
  "Freelens - Kubernetes IDE [AUR]"
  "Terraform [official]"
  "kubectl [official]"
  "Helm [official]"
  "Ansible [official]"
  "ArgoCD CLI [official]"
  "k9s - terminal K8s dashboard [official]"
  "kubectx / kubens [official]"
  "Podman (includes 'docker' command compat) [official]"
  "Podman Desktop - GUI [official]"
  "Kind - local K8s in a container [official]"
  "K3s [AUR]"
  "Microsoft Teams [AUR, actively maintained]"
  "Microsoft Outlook [AUR, PWA wrapper - low maintenance, verify still works]"
  "USB Imager - USB/SD image flasher, GUI [AUR]"
  "Bambu Studio - 3D printer slicer, P2S-ready [Flatpak, avoids AUR webkit2gtk build]"
  "Beeper - unified chat (WhatsApp/Telegram/Signal/iMessage/etc) [AUR]"
  "Zen Browser - Firefox-based, privacy-focused [AUR, prebuilt binary]"
  "GitHub CLI ('gh') [official]"
  "Vocalinux - 100% offline voice dictation, Vulkan GPU accel [AUR, maintained by upstream author]"
  "Firefox"
  "VLC"
)
METHODS=(aur aur pacman aur pacman pacman aur pacman aur pacman pacman pacman pacman pacman pacman pacman custom pacman pacman aur aur aur aur flatpak aur aur pacman aur pacman pacman)
PKGS=(claude-desktop antigravity-ide zed claude-code gemini-cli opencode opera obsidian freelens-bin terraform kubectl helm ansible argocd k9s kubectx podman podman-desktop kind k3s-bin teams-for-linux outlook-for-linux-bin usbimager com.bambulab.BambuStudio beeper-v4-bin zen-browser-bin github-cli vocalinux firefox vlc)

install_podman() {
  sudo pacman -S --needed --noconfirm podman podman-docker
  systemctl --user enable --now podman.socket 2>/dev/null || true
  echo "  Podman installed. 'docker' command now maps to podman."
}

remove_podman() {
  systemctl --user disable --now podman.socket 2>/dev/null || true
  sudo pacman -Rns --noconfirm podman-docker podman
}

is_installed() {
  local method="$1" pkg="$2"
  case "$method" in
    custom)
      case "$pkg" in
        podman)      pacman -Qi podman &>/dev/null && pacman -Qi podman-docker &>/dev/null ;;
      esac
      ;;
    flatpak)
      flatpak info "$pkg" &>/dev/null
      ;;
    *)
      pacman -Qi "$pkg" &>/dev/null
      ;;
  esac
}

install_item() {
  local method="$1" pkg="$2"
  case "$method" in
    pacman)  sudo pacman -S --needed --noconfirm "$pkg" ;;
    aur)     paru -S --noconfirm "$pkg" ;;
    flatpak) flatpak install -y --noninteractive flathub "$pkg" ;;
    custom)
      case "$pkg" in
        podman)      install_podman ;;
      esac
      ;;
  esac
}

remove_item() {
  local method="$1" pkg="$2"
  case "$method" in
    pacman|aur) sudo pacman -Rns --noconfirm "$pkg" ;;
    flatpak)    flatpak uninstall -y --noninteractive "$pkg" ;;
    custom)
      case "$pkg" in
        podman)      remove_podman ;;
      esac
      ;;
  esac
}

contains() {
  local needle="$1"; shift
  for x in "$@"; do [[ "$x" == "$needle" ]] && return 0; done
  return 1
}

echo "==> Launching software checklist..."
ARGS=()
for i in "${!TAGS[@]}"; do
  if is_installed "${METHODS[$i]}" "${PKGS[$i]}"; then st="on"; else st="off"; fi
  ARGS+=("${TAGS[$i]}" "${DESCS[$i]}" "$st")
done

TMPFILE=$(mktemp)
dialog --backtitle "CachyOS Setup" \
  --separate-output \
  --title "Software selection (space=toggle, enter=apply, esc=skip)" \
  --checklist "Checked = install/keep. Unchecked = remove if currently installed." \
  23 78 18 "${ARGS[@]}" \
  2> "$TMPFILE"
STATUS=$?
mapfile -t SELECTED < "$TMPFILE"
rm -f "$TMPFILE"
clear

if [[ $STATUS -ne 0 ]]; then
  echo "Checklist skipped/cancelled — foundation setup above still applied. No app changes made."
  exit 0
fi

echo "==> Applying application changes..."
for i in "${!TAGS[@]}"; do
  tag="${TAGS[$i]}"; method="${METHODS[$i]}"; pkg="${PKGS[$i]}"
  was_installed=false
  is_installed "$method" "$pkg" && was_installed=true

  if contains "$tag" "${SELECTED[@]}"; then
    if [[ "$was_installed" == false ]]; then
      echo "-- Installing: ${DESCS[$i]}"
      install_item "$method" "$pkg"
    fi
  else
    if [[ "$was_installed" == true ]]; then
      echo "-- Removing: ${DESCS[$i]}"
      remove_item "$method" "$pkg"
    fi
  fi
done

echo ""
echo "==> All done. Re-run this whole script anytime — foundation steps are idempotent,"
echo "    and the checklist always reflects current install state."
