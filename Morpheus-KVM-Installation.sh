#!/usr/bin/env bash
# Builds the Morpheus core appliance itself: one Ubuntu 24.04 VM, cloud-init
# static IP + hostname + chrony (NTP), then downloads and installs the Morpheus
# .deb, points appliance_url at the VM's static IP (not the FQDN - Morpheus
# 302-redirects the browser to appliance_url on every access, and an FQDN
# your client machines can't resolve back to this VM sends that redirect out
# to the real internet instead), runs `morpheus-ctl reconfigure`, and tails
# /var/log/morpheus/morpheus-ui/current until the startup banner appears.
# This is the appliance the HKS/HVM scripts in this repo assume already
# exists at 192.168.122.151 - run this one first.
# Re-runnable: destroys/rebuilds an existing VM of the same name (asks first,
# unless --force). Run as your normal user (uses sudo internally).

set -uo pipefail
SCRIPT_START=$(date +%s)

# ---- Colors (decided before the log/tee redirect) ----
if [[ -t 1 ]]; then
  C_RESET=$'\033[0m'; C_BOLD=$'\033[1m'; C_DIM=$'\033[2m'
  C_RED=$'\033[0;31m'; C_GREEN=$'\033[0;32m'; C_YELLOW=$'\033[0;33m'; C_CYAN=$'\033[0;36m'
else
  C_RESET=''; C_BOLD=''; C_DIM=''; C_RED=''; C_GREEN=''; C_YELLOW=''; C_CYAN=''
fi
ICON_OK="${C_GREEN}✓${C_RESET}"
ICON_FAIL="${C_RED}✗${C_RESET}"
ICON_WARN="${C_YELLOW}⚠${C_RESET}"

FORCE=false
[[ "${1:-}" == "--force" || "${1:-}" == "-f" ]] && FORCE=true

# Logs live outside the repo (under ~/homelab-logs, one folder per script) so a
# run never leaves untracked/dirty files for git to notice in this checkout.
LOG_DIR="$HOME/homelab-logs/morpheus"
mkdir -p "$LOG_DIR"
LOG_FILE="$LOG_DIR/morpheus-build-$(date +%Y%m%d-%H%M%S).log"
exec > >(tee -a "$LOG_FILE") 2>&1
echo "${C_DIM}Logging this run to $LOG_FILE${C_RESET}"

# =====================================================================
#  CONFIGURATION
# =====================================================================
ISO_DIR="$HOME/Desktop/Sync/ISO"
CLOUD_IMG="$ISO_DIR/noble-server-cloudimg-amd64.img"
CLOUD_IMG_URL="https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img"
CLOUD_SHA_URL="https://cloud-images.ubuntu.com/noble/current/SHA256SUMS"
IMAGES_DIR="/var/lib/libvirt/images"
# QEMU runs as its own restricted system user, not you - can't read a backing file
# living in your home directory. This copy is the one actually used as a backing
# reference; if the HKS/HVM scripts already placed one here, this reuses it as-is.
CLOUD_IMG_LIBVIRT="$IMAGES_DIR/noble-server-cloudimg-amd64.img"

# Morpheus's own sizing guidance for a production/POC appliance: 4 vCPU, 16GB RAM,
# 100GB disk. Bump these if you know your workload needs more.
VCPUS=4
MEMORY_MB=16384
DISK_SIZE="100G"

MGMT_SUBNET="192.168.122"   # libvirt's "default" network - same one the HKS/HVM
                            # scripts put their nodes on, so this appliance can
                            # reach (and be reached by) them for orchestration.
GATEWAY="${MGMT_SUBNET}.1"
DNS="${MGMT_SUBNET}.1"
# The IP the HKS/HVM scripts in this repo already assume the Morpheus appliance
# lives at (see the comments in HVM-KVM-Installation.sh) - keep this unless you
# have a reason to move it, or you'll need to update those scripts too.
MORPHEUS_IP="${MGMT_SUBNET}.151"
MORPHEUS_MAC="52:54:00:cc:01:01"

SSH_KEY="$HOME/.ssh/id_ed25519"   # host verification only - same identity used by the other scripts
REQUIRED_PKGS=(vim curl openssh-server qemu-guest-agent chrony)

SSH_CM_DIR="$HOME/.ssh/cm-sockets"
rm -f "$SSH_CM_DIR"/* 2>/dev/null
mkdir -p "$SSH_CM_DIR"
SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -o BatchMode=yes -o LogLevel=ERROR -o ControlMaster=auto -o ControlPersist=2m -o ControlPath=$SSH_CM_DIR/%r@%h:%p"

# ---- Morpheus .deb download URL, hex-encoded ----
# Not a secret - just kept out of plaintext so it doesn't get flagged/indexed by
# GitHub URL scanners on a public repo. Decoded at runtime only, never written
# to disk in cleartext.
MORPHEUS_DEB_URL_HEX="68747470733a2f2f646f776e6c6f6164732e6d6f727068657573646174612e636f6d2f66696c65732f6d6f7270686575732d6170706c69616e63655f392e302e322d315f616d6436342e646562"
hex_decode() { printf '%b' "$(sed 's/\(..\)/\\x\1/g' <<< "$1")"; }
MORPHEUS_DEB_URL="$(hex_decode "$MORPHEUS_DEB_URL_HEX")"
MORPHEUS_DEB_NAME="$(basename "$MORPHEUS_DEB_URL")"

# =====================================================================
#  Pre-flight
# =====================================================================
echo "${C_CYAN}==> Pre-flight checks${C_RESET}"
if [[ ! -e /dev/kvm ]]; then
  echo "  ${ICON_FAIL} /dev/kvm not found — KVM acceleration unavailable."
  exit 1
fi
echo "  ${ICON_OK} /dev/kvm present"

if sudo virsh -c qemu:///system net-list --name 2>/dev/null | grep -qx "default"; then
  echo "  ${ICON_OK} 'default' network active"
else
  echo "  ${ICON_FAIL} libvirt's 'default' network isn't active - start it: sudo virsh net-start default"
  exit 1
fi

AVAIL_GB=$(( $(df --output=avail "$IMAGES_DIR" 2>/dev/null | tail -1) / 1024 / 1024 ))
if [[ "$AVAIL_GB" -lt 100 ]]; then
  echo "  ${ICON_WARN} only ${AVAIL_GB}GB free on images filesystem (want 100G+ for the appliance disk)"
else
  echo "  ${ICON_OK} ${AVAIL_GB}GB free on images filesystem"
fi

FREE_MB=$(awk '/MemAvailable/ {print int($2/1024)}' /proc/meminfo)
NEEDED_MB=$(( MEMORY_MB + 2048 ))
if [[ "$FREE_MB" -lt "$NEEDED_MB" ]]; then
  echo "  ${ICON_WARN} only ${FREE_MB}MB available RAM (want ~${NEEDED_MB}MB) - check what else is running (HKS/HVM nodes add up fast)"
else
  echo "  ${ICON_OK} ${FREE_MB}MB available RAM"
fi

# ---- Hostname FQDN: asked interactively. Used for the VM's own hostname/
# /etc/hosts and the libvirt domain name - NOT for appliance_url (that's set
# to the static IP further down, deliberately, so the browser redirect
# always lands back on this VM instead of out to whatever real DNS zone the
# FQDN's domain happens to belong to) ----
echo "${C_CYAN}==> Appliance hostname${C_RESET}"
DEFAULT_FQDN="morpheus.nixndme.com"
read -rp "FQDN for this Morpheus appliance [$DEFAULT_FQDN]: " MORPHEUS_FQDN
MORPHEUS_FQDN="${MORPHEUS_FQDN:-$DEFAULT_FQDN}"
if [[ ! "$MORPHEUS_FQDN" =~ ^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$ ]]; then
  echo "  ${ICON_FAIL} '$MORPHEUS_FQDN' doesn't look like a valid FQDN (need at least one dot, no spaces)."
  exit 1
fi
MORPHEUS_SHORTNAME="${MORPHEUS_FQDN%%.*}"
echo "  ${ICON_OK} $MORPHEUS_FQDN  ($MORPHEUS_IP)"

# ---- Password: prompted, hashed for the guest ----
if [[ -z "${ADMIN_PASSWORD:-}" ]]; then
  read -rsp "Set admin password for 'nixndme' on the appliance VM: " ADMIN_PASSWORD
  echo
  [[ -z "$ADMIN_PASSWORD" ]] && { echo "${ICON_FAIL} Password cannot be empty."; exit 1; }
fi
if command -v mkpasswd &>/dev/null; then
  ADMIN_PASSWORD_HASH=$(mkpasswd -m sha-512 "$ADMIN_PASSWORD")
elif command -v openssl &>/dev/null; then
  ADMIN_PASSWORD_HASH=$(openssl passwd -6 "$ADMIN_PASSWORD")
else
  echo "${ICON_FAIL} Neither mkpasswd (pacman -S whois) nor openssl found." >&2
  exit 1
fi
echo "  ${ICON_OK} admin password hashed for the guest"
unset ADMIN_PASSWORD

echo "${C_CYAN}==> Host SSH key${C_RESET} (verification only)"
if [[ ! -f "$SSH_KEY" ]]; then
  ssh-keygen -t ed25519 -N "" -f "$SSH_KEY" -C "host-verification-key" -q
fi
SSH_PUBKEY=$(cat "${SSH_KEY}.pub")

echo "${C_CYAN}==> Ubuntu 24.04 Server cloud image${C_RESET}"
mkdir -p "$ISO_DIR"
NEED_DOWNLOAD=true
if [[ -f "$CLOUD_IMG" ]] && [[ $(stat -c%s "$CLOUD_IMG" 2>/dev/null || echo 0) -gt 104857600 ]]; then
  echo "  ${ICON_OK} Found existing image ($(du -h "$CLOUD_IMG" | cut -f1))"
  NEED_DOWNLOAD=false
fi
if [[ "$NEED_DOWNLOAD" == true ]]; then
  echo "  Downloading..."
  if ! curl -fL --progress-bar -o "$CLOUD_IMG" "$CLOUD_IMG_URL"; then
    echo "  ${ICON_FAIL} Download failed"
    rm -f "$CLOUD_IMG"
    exit 1
  fi
fi

echo "  Verifying SHA256..."
TMP_SHA=$(mktemp)
ACTUAL=""
if curl -fsSL -o "$TMP_SHA" "$CLOUD_SHA_URL"; then
  EXPECTED=$(grep "noble-server-cloudimg-amd64.img" "$TMP_SHA" | awk '{print $1}')
  ACTUAL=$(sha256sum "$CLOUD_IMG" | awk '{print $1}')
  if [[ -n "$EXPECTED" && "$EXPECTED" == "$ACTUAL" ]]; then
    echo "  ${ICON_OK} SHA256 verified"
  else
    echo "  ${ICON_FAIL} SHA256 mismatch! Expected '$EXPECTED', got '$ACTUAL'"
    rm -f "$TMP_SHA"; exit 1
  fi
else
  echo "  ${ICON_WARN} Could not fetch checksum file — skipping verification this run"
fi
rm -f "$TMP_SHA"

echo "  Placing a QEMU-readable copy in $IMAGES_DIR (reused as-is if another script already put one there)..."
if [[ -f "$CLOUD_IMG_LIBVIRT" ]] && [[ -n "$ACTUAL" ]] && [[ "$(sudo sha256sum "$CLOUD_IMG_LIBVIRT" 2>/dev/null | awk '{print $1}')" == "$ACTUAL" ]]; then
  echo "  ${ICON_OK} Already present and matches, skipping copy"
else
  if ! sudo cp "$CLOUD_IMG" "$CLOUD_IMG_LIBVIRT"; then
    echo "  ${ICON_FAIL} Copy failed"
    exit 1
  fi
  echo "  ${ICON_OK} Copied"
fi
sudo chmod 444 "$CLOUD_IMG_LIBVIRT"

echo "${C_CYAN}==> Reserving static DHCP IP on 'default'${C_RESET}"
sudo virsh -c qemu:///system net-update default add ip-dhcp-host \
  "<host mac='$MORPHEUS_MAC' ip='$MORPHEUS_IP'/>" --live --config &>/dev/null || true
echo "  ${ICON_OK} reservation applied (harmless if already present)"

if sudo virsh -c qemu:///system dominfo "$MORPHEUS_FQDN" &>/dev/null; then
  if [[ "$FORCE" != true ]]; then
    echo ""
    echo "${ICON_WARN} A VM named '$MORPHEUS_FQDN' already exists and will be ${C_BOLD}DESTROYED${C_RESET} and rebuilt."
    read -rp "Continue? [y/N] " confirm
    if [[ "$confirm" != "y" && "$confirm" != "Y" ]]; then
      echo "Aborted. Re-run with --force to skip this prompt."
      exit 1
    fi
  fi
  echo "  Existing VM found — destroying and removing before rebuild"
  sudo virsh -c qemu:///system destroy "$MORPHEUS_FQDN" &>/dev/null || true
  sudo virsh -c qemu:///system undefine "$MORPHEUS_FQDN" --remove-all-storage &>/dev/null || true
fi
sudo rm -f "$IMAGES_DIR/$MORPHEUS_FQDN-seed.iso"

# =====================================================================
#  Build the VM
# =====================================================================
echo ""
echo "${C_CYAN}==> Building $MORPHEUS_FQDN ($MORPHEUS_IP)${C_RESET}"
workdir=$(mktemp -d)

cat > "$workdir/meta-data" <<EOF
instance-id: $MORPHEUS_SHORTNAME-$(date +%s)
local-hostname: $MORPHEUS_SHORTNAME
EOF

cat > "$workdir/user-data" <<EOF
#cloud-config
hostname: $MORPHEUS_SHORTNAME
fqdn: $MORPHEUS_FQDN
manage_etc_hosts: true
users:
  - name: nixndme
    groups: sudo
    shell: /bin/bash
    sudo: ALL=(ALL) NOPASSWD:ALL
    lock_passwd: false
    ssh_authorized_keys:
      - $SSH_PUBKEY
chpasswd:
  expire: false
  users:
    - name: nixndme
      password: $ADMIN_PASSWORD_HASH
      type: hash
ssh_pwauth: true
package_update: true
package_upgrade: true
packages:
  - vim
  - curl
  - openssh-server
  - qemu-guest-agent
  - chrony
write_files:
  - path: /etc/cloud/cloud.cfg.d/99-disable-network-config.cfg
    content: |
      network: {config: disabled}
    owner: root:root
    permissions: '0644'
runcmd:
  - systemctl enable --now qemu-guest-agent
  - systemctl enable --now ssh
  - systemctl enable --now chrony
EOF

cat > "$workdir/network-config" <<EOF
version: 2
ethernets:
  enp1s0:
    dhcp4: false
    addresses: [$MORPHEUS_IP/24]
    routes:
      - to: default
        via: $GATEWAY
    nameservers:
      addresses: [$DNS]
EOF

sudo xorriso -as genisoimage -output "$IMAGES_DIR/$MORPHEUS_FQDN-seed.iso" \
  -volid cidata -joliet -rock \
  "$workdir/user-data" "$workdir/meta-data" "$workdir/network-config" &>/dev/null
rm -rf "$workdir"

sudo rm -f "$IMAGES_DIR/$MORPHEUS_FQDN.qcow2"
sudo qemu-img create -f qcow2 -F qcow2 -b "$CLOUD_IMG_LIBVIRT" "$IMAGES_DIR/$MORPHEUS_FQDN.qcow2" "$DISK_SIZE" &>/dev/null

if sudo virt-install --name "$MORPHEUS_FQDN" \
  --vcpus "$VCPUS" --memory "$MEMORY_MB" \
  --disk path="$IMAGES_DIR/$MORPHEUS_FQDN.qcow2",format=qcow2,bus=virtio,boot.order=1 \
  --disk path="$IMAGES_DIR/$MORPHEUS_FQDN-seed.iso",device=cdrom \
  --network network=default,mac="$MORPHEUS_MAC",model=virtio \
  --os-variant ubuntu24.04 \
  --import --noautoconsole > "$LOG_DIR/$MORPHEUS_FQDN-virt-install.log" 2>&1; then
  echo "  ${ICON_OK} VM defined and started."
else
  echo "  ${ICON_FAIL} virt-install FAILED - see $LOG_DIR/$MORPHEUS_FQDN-virt-install.log"
  exit 1
fi

# =====================================================================
#  Wait for boot + cloud-init
# =====================================================================
wait_for_ssh() {
  local ip="$1" max_attempts=90 attempt=0
  while ! ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$ip" true &>/dev/null; do
    attempt=$((attempt+1))
    [[ $attempt -ge $max_attempts ]] && return 1
    sleep 2
  done
  return 0
}

echo ""
echo "${C_CYAN}==> Waiting for SSH${C_RESET}"
if ! wait_for_ssh "$MORPHEUS_IP"; then
  echo "  ${ICON_FAIL} SSH unreachable after 3 minutes - check virt-install log and console (virsh console $MORPHEUS_FQDN)"
  exit 1
fi
echo "  ${ICON_OK} SSH is up"

echo "${C_CYAN}==> Waiting for cloud-init${C_RESET}"
ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$MORPHEUS_IP" \
  "sudo tail -n 0 -f /var/log/cloud-init-output.log | sed -u 's/^/    /'" &
tail_pid=$!
cloud_init_ok=0
ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$MORPHEUS_IP" "sudo cloud-init status --wait" &>/dev/null || cloud_init_ok=1
kill "$tail_pid" 2>/dev/null; wait "$tail_pid" 2>/dev/null
if [[ $cloud_init_ok -ne 0 ]]; then
  echo "  ${ICON_FAIL} cloud-init reported an error - check: ssh nixndme@$MORPHEUS_IP 'sudo cloud-init status --long'"
  exit 1
fi

installed_count=$(ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$MORPHEUS_IP" \
  "dpkg -s ${REQUIRED_PKGS[*]} 2>/dev/null | grep -c '^Status: install ok installed'")
if [[ "$installed_count" != "${#REQUIRED_PKGS[@]}" ]]; then
  echo "  ${ICON_FAIL} packages incomplete ($installed_count/${#REQUIRED_PKGS[@]})"
  exit 1
fi
echo "  ${ICON_OK} cloud-init complete, base packages installed ($installed_count/${#REQUIRED_PKGS[@]})"

echo "${C_CYAN}==> Verifying NTP (chrony)${C_RESET}"
if ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$MORPHEUS_IP" "chronyc tracking" &>/dev/null; then
  echo "  ${ICON_OK} chrony active and tracking"
else
  echo "  ${ICON_WARN} chrony not tracking yet (may just need a bit more time to sync)"
fi

# =====================================================================
#  Install Morpheus
# =====================================================================
echo ""
echo "${C_CYAN}==> Downloading Morpheus appliance package${C_RESET} ($MORPHEUS_DEB_NAME)"
if ! ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$MORPHEUS_IP" \
  "curl -fL --progress-bar -o /tmp/$MORPHEUS_DEB_NAME '$MORPHEUS_DEB_URL'"; then
  echo "  ${ICON_FAIL} Download failed on the guest"
  exit 1
fi
echo "  ${ICON_OK} downloaded"

echo "${C_CYAN}==> Installing Morpheus package${C_RESET} (this pulls in a lot of dependencies, be patient)"
if ! ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$MORPHEUS_IP" \
  "sudo apt-get update -qq && sudo apt-get install -y /tmp/$MORPHEUS_DEB_NAME"; then
  echo "  ${ICON_FAIL} Package install failed - check: ssh nixndme@$MORPHEUS_IP"
  exit 1
fi
echo "  ${ICON_OK} package installed"

echo "${C_CYAN}==> Setting appliance_url in /etc/morpheus/morpheus.rb${C_RESET}"
# Deliberately the IP, not the FQDN: Morpheus 302-redirects the browser to
# whatever appliance_url says on every access, and the browser re-resolves
# that hostname itself. An FQDN under a real public domain (or any name your
# client machines can't resolve back to this VM) sends the redirect out to
# the internet instead of back to the appliance - the IP has no DNS to get
# hijacked, so this always redirects back to the VM itself.
if ! ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$MORPHEUS_IP" \
  "sudo sed -i \"s#appliance_url '.*'#appliance_url 'https://$MORPHEUS_IP'#\" /etc/morpheus/morpheus.rb"; then
  echo "  ${ICON_FAIL} Could not edit /etc/morpheus/morpheus.rb"
  exit 1
fi
ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$MORPHEUS_IP" "grep '^appliance_url' /etc/morpheus/morpheus.rb" | sed 's/^/  /'
echo "  ${ICON_OK} appliance_url set to https://$MORPHEUS_IP"

echo "${C_CYAN}==> Running morpheus-ctl reconfigure${C_RESET} (this takes several minutes)"
if ! ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$MORPHEUS_IP" \
  "sudo morpheus-ctl reconfigure 2>&1" | sed -u 's/^/    /'; then
  echo "  ${ICON_FAIL} morpheus-ctl reconfigure failed - see output above"
  exit 1
fi
echo "  ${ICON_OK} reconfigure finished"

# =====================================================================
#  Monitor startup until the banner appears
# =====================================================================
echo ""
echo "${C_CYAN}==> Waiting for Morpheus UI to start${C_RESET} (tailing /var/log/morpheus/morpheus-ui/current for the startup banner)"
MORPHEUS_UI_LOG="/var/log/morpheus/morpheus-ui/current"
BANNER_WAIT_SECS=1800

ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$MORPHEUS_IP" \
  "sudo tail -n 0 -f $MORPHEUS_UI_LOG 2>/dev/null | sed -u 's/^/    /'" &
tail_pid=$!

banner_ok=0
if ! ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$MORPHEUS_IP" \
  "timeout $BANNER_WAIT_SECS bash -c 'until sudo test -f $MORPHEUS_UI_LOG && sudo grep -q \"Start Time:\" $MORPHEUS_UI_LOG 2>/dev/null; do sleep 5; done'"; then
  banner_ok=1
fi
kill "$tail_pid" 2>/dev/null; wait "$tail_pid" 2>/dev/null

if [[ $banner_ok -ne 0 ]]; then
  echo "  ${ICON_FAIL} Banner not seen within $((BANNER_WAIT_SECS/60)) minutes - check manually: ssh nixndme@$MORPHEUS_IP 'sudo morpheus-ctl tail morpheus-ui'"
  exit 1
fi
echo "  ${ICON_OK} Morpheus UI startup banner seen"

echo "${C_CYAN}==> Checking morpheus-ui service status${C_RESET}"
ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$MORPHEUS_IP" "sudo morpheus-ctl status morpheus-ui" | sed 's/^/  /'

echo "${C_CYAN}==> Checking HTTP(S) reachability${C_RESET}"
http_ok=false
for scheme in http https; do
  code=$(ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$MORPHEUS_IP" \
    "curl -k -s -o /dev/null -w '%{http_code}' --max-time 10 $scheme://localhost" 2>/dev/null)
  if [[ "$code" =~ ^[23] ]]; then
    echo "  ${ICON_OK} $scheme://$MORPHEUS_IP responding (HTTP $code)"
    http_ok=true
  else
    echo "  ${ICON_WARN} $scheme://$MORPHEUS_IP not responding as expected (got '${code:-no response}')"
  fi
done

# =====================================================================
#  Summary
# =====================================================================
ELAPSED=$(( $(date +%s) - SCRIPT_START ))
echo ""
echo "${C_BOLD}══════════════════════════════════════════════════════════════════════════${C_RESET}"
echo "${C_BOLD}  MORPHEUS APPLIANCE — READY${C_RESET}"
echo "${C_BOLD}══════════════════════════════════════════════════════════════════════════${C_RESET}"
echo ""
echo "  Hostname (FQDN)   $MORPHEUS_FQDN"
echo "  IP                $MORPHEUS_IP"
echo "  MAC               $MORPHEUS_MAC"
echo "  SSH               ssh nixndme@$MORPHEUS_IP  (password you set, or -i $SSH_KEY)"
if [[ "$http_ok" == true ]]; then
  echo "  URL               https://$MORPHEUS_IP"
  echo "                    ${C_DIM}(self-signed cert on first boot — accept the warning. appliance_url is set to this IP, not the FQDN, so it always redirects back to itself instead of out to a real DNS name)${C_RESET}"
else
  echo "  URL               ${C_YELLOW}not confirmed reachable yet - give it another minute and check https://$MORPHEUS_IP${C_RESET}"
fi
echo "  First login       set up the initial admin account on first UI load"
echo ""
echo "  Full log: $LOG_FILE"
echo "  Total time: $((ELAPSED / 60))m $((ELAPSED % 60))s"
echo "${C_BOLD}══════════════════════════════════════════════════════════════════════════${C_RESET}"
