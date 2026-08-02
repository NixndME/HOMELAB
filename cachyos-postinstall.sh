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

echo "==> [1/8] Baseline snapshot"
sudo snapper -c root create -d "setup run $(date +%F_%H%M)"

echo "==> [2/8] Full system update"
sudo pacman -Syu --noconfirm

echo "==> [3/8] Bootstrap: paru, dialog, curl, openssl"
if ! command -v paru &>/dev/null; then
  sudo pacman -S --needed --noconfirm base-devel git
  git clone https://aur.archlinux.org/paru.git /tmp/paru-build
  (cd /tmp/paru-build && makepkg -si --noconfirm)
  rm -rf /tmp/paru-build
fi
sudo pacman -S --needed --noconfirm dialog curl openssl kconfig

echo "==> [4/8] SSH server (remote terminal access)"
sudo pacman -S --needed --noconfirm openssh
sudo systemctl enable --now sshd.service

echo "==> [5/8] RDP server — KDE native KRDP (macOS: use 'Windows App' RDP client)"
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

echo "==> [6/8] Firewall: open SSH (22) and RDP (3389)"
if command -v ufw &>/dev/null && sudo ufw status | grep -q "Status: active"; then
  sudo ufw allow 22/tcp
  sudo ufw allow 3389/tcp
  sudo ufw reload
else
  echo "  ufw inactive/not installed — skip (open these ports manually once ufw is enabled)"
fi

echo "==> [7/8] Virtualization / KVM lab stack"
sudo pacman -S --needed --noconfirm \
  qemu-full libvirt virt-manager virt-viewer edk2-ovmf \
  dnsmasq bridge-utils vde2 openbsd-netcat ebtables iptables-nft swtpm
sudo systemctl enable --now libvirtd.service
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

echo "==> [8/8] fish shell: LIBVIRT_DEFAULT_URI"
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
TAGS=(claude-desktop antigravity opera obsidian freelens terraform kubectl helm ansible argocd k9s kubectx podman podman-desktop docker-compat kind k3s teams outlook firefox vlc)
DESCS=(
  "Claude Desktop [AUR]"
  "Antigravity IDE - Google AI IDE [tarball, replaces VS Code]"
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
  "Podman [official]"
  "Podman Desktop - GUI [official]"
  "Docker CLI compat: 'docker' -> podman [official, podman-docker]"
  "Kind - local K8s in a container [official]"
  "K3s [AUR]"
  "Microsoft Teams [AUR, actively maintained]"
  "Microsoft Outlook [AUR, PWA wrapper - low maintenance, verify still works]"
  "Firefox"
  "VLC"
)
METHODS=(aur custom aur pacman aur pacman pacman pacman pacman pacman pacman pacman pacman pacman custom pacman aur aur aur pacman pacman)
PKGS=(claude-desktop antigravity opera obsidian freelens-bin terraform kubectl helm ansible argocd k9s kubectx podman podman-desktop docker-compat kind k3s-bin teams-for-linux outlook-for-linux-bin firefox vlc)

install_antigravity() {
  echo "  Resolving latest Antigravity IDE tarball URL..."
  local url
  url=$(curl -fsSL --compressed \
        -A "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36" \
        https://antigravity.google/download 2>/dev/null \
        | grep -Eo 'https://[^"]+linux-x64[^"]*IDE[^"]*\.tar\.gz' | sort -u | head -n1)
  if [[ -z "$url" ]]; then
    echo "  Could not auto-resolve the URL (Google may have changed their page)."
    echo "  Download manually from: https://antigravity.google/download/linux"
    return 1
  fi
  echo "  Found: $url"
  local tmp; tmp=$(mktemp -d)
  if ! curl -fsSL "$url" -o "$tmp/antigravity-ide.tar.gz"; then
    echo "  Download failed."; rm -rf "$tmp"; return 1
  fi
  sudo rm -rf /opt/antigravity-ide
  sudo mkdir -p /opt/antigravity-ide
  sudo tar -xzf "$tmp/antigravity-ide.tar.gz" -C /opt/antigravity-ide --strip-components=1
  sudo chown root:root /opt/antigravity-ide/chrome-sandbox
  sudo chmod 4755 /opt/antigravity-ide/chrome-sandbox
  mkdir -p "$HOME/.local/bin"
  ln -sf /opt/antigravity-ide/antigravity-ide "$HOME/.local/bin/antigravity-ide"
  rm -rf "$tmp"
  echo "  Installed. Launch with: antigravity-ide  (ensure ~/.local/bin is in PATH)"
}

remove_antigravity() {
  sudo rm -rf /opt/antigravity-ide
  rm -f "$HOME/.local/bin/antigravity-ide"
}

install_docker_compat() {
  sudo pacman -S --needed --noconfirm podman-docker
  systemctl --user enable --now podman.socket 2>/dev/null || true
  echo "  'docker' command now maps to podman. Rootless docker-compatible socket enabled."
}

remove_docker_compat() {
  systemctl --user disable --now podman.socket 2>/dev/null || true
  sudo pacman -Rns --noconfirm podman-docker
}

is_installed() {
  local method="$1" pkg="$2"
  case "$method" in
    custom)
      case "$pkg" in
        antigravity)   [[ -x "$HOME/.local/bin/antigravity-ide" ]] ;;
        docker-compat) pacman -Qi podman-docker &>/dev/null ;;
      esac
      ;;
    *)
      pacman -Qi "$pkg" &>/dev/null
      ;;
  esac
}

install_item() {
  local method="$1" pkg="$2"
  case "$method" in
    pacman) sudo pacman -S --needed --noconfirm "$pkg" ;;
    aur)    paru -S --noconfirm "$pkg" ;;
    custom)
      case "$pkg" in
        antigravity)   install_antigravity ;;
        docker-compat) install_docker_compat ;;
      esac
      ;;
  esac
}

remove_item() {
  local method="$1" pkg="$2"
  case "$method" in
    pacman|aur) sudo pacman -Rns --noconfirm "$pkg" ;;
    custom)
      case "$pkg" in
        antigravity)   remove_antigravity ;;
        docker-compat) remove_docker_compat ;;
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
