#!/usr/bin/env bash
# Creates hvm01/02/03 unattended from Ubuntu 24.04 cloud image + cloud-init (NoCloud).
# Re-runnable: destroys/rebuilds existing VMs of the same name (asks first, unless --force).
# Run as normal user (uses sudo internally).
#
# NETWORK ARCHITECTURE: Management runs on libvirt's "default" network (same one
# Morpheus's own appliance uses) - required so Morpheus can actually reach these hosts
# for SSH/orchestration. Storage/Overlay/Compute are dedicated isolated networks (no
# Morpheus-reachability needed - only peer-to-peer between HVM nodes themselves).
# If you already added a UFW rule for virbr0 earlier (for the default network), it
# already covers Management here too - no new rule needed for that part.

set -uo pipefail
SCRIPT_START=$(date +%s)

# ---- Colors (decided before the log/tee redirect, since after redirecting, stdout is a
# pipe, not a TTY, and this check would wrongly disable colors if done afterward) ----
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

# ---- Logging: every run leaves a timestamped record ----
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LOG_DIR="$SCRIPT_DIR/logs"
mkdir -p "$LOG_DIR"
LOG_FILE="$LOG_DIR/hvm-build-$(date +%Y%m%d-%H%M%S).log"
exec > >(tee -a "$LOG_FILE") 2>&1
echo "${C_DIM}Logging this run to $LOG_FILE${C_RESET}"
echo "${C_DIM}(tip: add 'logs/' to .gitignore in this repo)${C_RESET}"

# =====================================================================
#  CONFIGURATION
# =====================================================================
ISO_DIR="$HOME/Desktop/Sync/ISO"
CLOUD_IMG="$ISO_DIR/noble-server-cloudimg-amd64.img"
CLOUD_IMG_URL="https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img"
CLOUD_SHA_URL="https://cloud-images.ubuntu.com/noble/current/SHA256SUMS"
IMAGES_DIR="/var/lib/libvirt/images"
# QEMU runs as its own restricted system user (not you), which can't traverse into
# your home directory to read a backing file living there ("Permission denied" at
# VM start, since backing files are read live, not just at creation time). Keep the
# download cached in the home dir (avoids re-downloading on every rebuild), but the
# actual backing-file reference used by qemu-img must live somewhere QEMU can reach -
# /var/lib/libvirt/images is already correctly permissioned for exactly this.
CLOUD_IMG_LIBVIRT="$IMAGES_DIR/noble-server-cloudimg-amd64.img"

VCPUS=4
MEMORY_MB=8192
DISK_SIZE="60G"
DATA_DISK_SIZE="500G"

# Dedicated libvirt networks (created if missing). Mgmt is NAT (internet + host access);
# storage/overlay/compute are isolated bridges - no DHCP, no forward, we set static IPs
# ourselves in each guest. Nodes still reach each other fine (same bridge = same L2
# segment); the HOST just has no route onto the isolated ones, which is fine since the
# script only ever talks to guests via their management IP.
NET_MGMT="default"   # Morpheus's appliance lives here (192.168.122.0/24) - management
                     # MUST be reachable from Morpheus for SSH/orchestration, so it goes
                     # on the SAME network, not a separate isolated one. Storage/Overlay/
                     # Compute don't need Morpheus-reachability - only peer-to-peer between
                     # HVM nodes - so those stay on their own dedicated isolated networks.
NET_STORAGE="hvm-storage"
NET_OVERLAY="hvm-overlay"
NET_COMPUTE="hvm-compute"
# Explicit short bridge names: Linux caps interface names at 15 chars, and
# "virbr-hvm-storage" (17) etc. exceed that - this is what caused
# "Numerical result out of range" (a disguised ENAMETOOLONG) previously.
declare -A NET_BRIDGE=(
  [$NET_STORAGE]="vbr-store"
  [$NET_OVERLAY]="vbr-ovly"
  [$NET_COMPUTE]="vbr-cmpt"
)

# Management uses libvirt's pre-existing "default" network/subnet - same one Morpheus's
# own appliance (192.168.122.151) is on.
MGMT_SUBNET="192.168.122"
GATEWAY="${MGMT_SUBNET}.1"
DNS="${MGMT_SUBNET}.1"

SSH_KEY="$HOME/.ssh/id_ed25519"                # host verification only - never on guests
CLUSTER_KEY="$HOME/.ssh/hvm_cluster_key"       # node-to-node only

REQUIRED_PKGS=(vim curl openssh-server qemu-guest-agent multipath-tools chrony)

STORAGE_SUBNET="192.168.140"   # deliberately distinct from MGMT_SUBNET (192.168.122) -
                                # both networks sharing a subnet number, even on separate
                                # bridges, causes kernel routing ambiguity (two interfaces
                                # claiming the same subnet) - this is what made "HVM Host
                                # Build OVS Interface" take 50+ minutes instead of ~2 seconds.

# Node definitions: name mgmt_ip mac_mgmt mac_storage mac_overlay storage_ip mac_compute
NODES=(
  "hvm01.nixndme.com ${MGMT_SUBNET}.201 52:54:00:aa:01:01 52:54:00:aa:02:01 52:54:00:aa:03:01 ${STORAGE_SUBNET}.211 52:54:00:aa:04:01"
  "hvm02.nixndme.com ${MGMT_SUBNET}.202 52:54:00:aa:01:02 52:54:00:aa:02:02 52:54:00:aa:03:02 ${STORAGE_SUBNET}.212 52:54:00:aa:04:02"
  "hvm03.nixndme.com ${MGMT_SUBNET}.203 52:54:00:aa:01:03 52:54:00:aa:02:03 52:54:00:aa:03:03 ${STORAGE_SUBNET}.213 52:54:00:aa:04:03"
)

SSH_CM_DIR="$HOME/.ssh/cm-sockets"
# Clear any stale sockets from a prior (possibly interrupted) run before creating new
# ones - these are meant to be ephemeral per-run, and a stale socket pointing at a
# now-gone connection can cause a new SSH call to hang instead of failing fast.
rm -f "$SSH_CM_DIR"/* 2>/dev/null
mkdir -p "$SSH_CM_DIR"
SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -o BatchMode=yes -o LogLevel=ERROR -o ControlMaster=auto -o ControlPersist=2m -o ControlPath=$SSH_CM_DIR/%r@%h:%p"

# =====================================================================
#  Helper functions
# =====================================================================
hash_password() {
  local plain="$1"
  if command -v mkpasswd &>/dev/null; then
    mkpasswd -m sha-512 "$plain"
  elif command -v openssl &>/dev/null; then
    openssl passwd -6 "$plain"
  else
    echo "${ICON_FAIL} Neither mkpasswd (pacman -S whois) nor openssl found." >&2
    exit 1
  fi
}

ensure_libvirt_network() {
  local name="$1" mode="$2"   # mode = nat | isolated
  local bridge="${NET_BRIDGE[$name]}"
  if sudo virsh -c qemu:///system net-info "$name" &>/dev/null; then
    # Check active state via net-list (exact name match against currently-active
    # networks) BEFORE calling net-start - net-start erroring "already active"
    # is not a failure, and net-info's field-aligned text is more brittle to parse.
    if sudo virsh -c qemu:///system net-list --name 2>/dev/null | grep -qx "$name"; then
      echo "  ${ICON_OK} network '$name' already exists and active"
      return 0
    fi
    if sudo virsh -c qemu:///system net-start "$name" &>/dev/null; then
      echo "  ${ICON_OK} network '$name' already exists (started now)"
      return 0
    fi
    echo "  ${ICON_FAIL} network '$name' exists but failed to start - check: sudo virsh net-start $name"
    return 1
  fi

  local xml
  if [[ "$mode" == "nat" ]]; then
    xml=$(cat <<XMLEOF
<network>
  <name>$name</name>
  <forward mode='nat'/>
  <bridge name='${bridge}' stp='off' delay='0'/>
  <ip address='$GATEWAY' netmask='255.255.255.0'>
    <dhcp>
      <range start='${MGMT_SUBNET}.100' end='${MGMT_SUBNET}.199'/>
    </dhcp>
  </ip>
</network>
XMLEOF
)
  else
    xml=$(cat <<XMLEOF
<network>
  <name>$name</name>
  <bridge name='${bridge}' stp='off' delay='0'/>
</network>
XMLEOF
)
  fi

  if ! echo "$xml" | sudo virsh -c qemu:///system net-define /dev/stdin &>/dev/null; then
    echo "  ${ICON_FAIL} failed to define network '$name' - check: sudo virsh net-define <file>"
    return 1
  fi
  if ! sudo virsh -c qemu:///system net-start "$name" &>/dev/null; then
    echo "  ${ICON_FAIL} failed to start network '$name' - check: sudo journalctl -u libvirtd --since '2 min ago'"
    return 1
  fi
  sudo virsh -c qemu:///system net-autostart "$name" &>/dev/null
  echo "  ${ICON_OK} created + started network '$name' ($mode, bridge $bridge)"
}

build_hosts_block() {
  for node in "${NODES[@]}"; do
    read -r n_name n_ip _ <<< "$node"
    echo "$n_ip $n_name ${n_name%%.*}"
  done
}

cleanup_existing_vm() {
  local name="$1"
  if sudo virsh -c qemu:///system dominfo "$name" &>/dev/null; then
    echo "  [$name] Existing VM found — destroying and removing before rebuild"
    sudo virsh -c qemu:///system destroy "$name" &>/dev/null || true
    sudo virsh -c qemu:///system undefine "$name" --remove-all-storage &>/dev/null || true
  fi
  sudo rm -f "$IMAGES_DIR/$name-seed.iso"
}

# =====================================================================
#  Pre-flight
# =====================================================================
echo "${C_CYAN}==> Tools${C_RESET}"
sudo pacman -S --needed --noconfirm libisoburn openssh whois

echo "${C_CYAN}==> Pre-flight checks${C_RESET}"
if [[ ! -e /dev/kvm ]]; then
  echo "  ${ICON_FAIL} /dev/kvm not found — KVM acceleration unavailable. Check BIOS virtualization settings."
  exit 1
fi
echo "  ${ICON_OK} /dev/kvm present"

NESTED=$(cat /sys/module/kvm_amd/parameters/nested 2>/dev/null || echo "0")
if [[ "$NESTED" != "1" && "$NESTED" != "Y" ]]; then
  echo "  ${ICON_WARN} nested virtualization may be off (kvm_amd nested=$NESTED)"
else
  echo "  ${ICON_OK} nested virtualization enabled"
fi

AVAIL_GB=$(( $(df --output=avail "$IMAGES_DIR" 2>/dev/null | tail -1) / 1024 / 1024 ))
if [[ "$AVAIL_GB" -lt 50 ]]; then
  echo "  ${ICON_WARN} only ${AVAIL_GB}GB free on images filesystem — sparse disks grow as used, monitor this"
else
  echo "  ${ICON_OK} ${AVAIL_GB}GB free on images filesystem"
fi

FREE_MB=$(awk '/MemAvailable/ {print int($2/1024)}' /proc/meminfo)
NEEDED_MB=$(( ${#NODES[@]} * MEMORY_MB + 4096 ))
if [[ "$FREE_MB" -lt "$NEEDED_MB" ]]; then
  echo "  ${ICON_WARN} only ${FREE_MB}MB available RAM (want ~${NEEDED_MB}MB for ${#NODES[@]} VMs)"
else
  echo "  ${ICON_OK} ${FREE_MB}MB available RAM"
fi

# ---- Password: hashed, never stored in cleartext beyond this shell ----
if [[ -z "${ADMIN_PASSWORD:-}" ]]; then
  read -rsp "Set admin password for 'nixndme' on all 3 HVM nodes: " ADMIN_PASSWORD
  echo
  [[ -z "$ADMIN_PASSWORD" ]] && { echo "${ICON_FAIL} Password cannot be empty."; exit 1; }
fi
ADMIN_PASSWORD_HASH=$(hash_password "$ADMIN_PASSWORD")
unset ADMIN_PASSWORD
echo "  ${ICON_OK} admin password hashed (SHA-512), cleartext cleared from shell"

echo "${C_CYAN}==> Host SSH key${C_RESET} (verification only — never placed on guests)"
if [[ ! -f "$SSH_KEY" ]]; then
  ssh-keygen -t ed25519 -N "" -f "$SSH_KEY" -C "host-verification-key" -q
fi
SSH_PUBKEY=$(cat "${SSH_KEY}.pub")

echo "${C_CYAN}==> Cluster-internal SSH key${C_RESET} (node-to-node only)"
if [[ ! -f "$CLUSTER_KEY" ]]; then
  ssh-keygen -t ed25519 -N "" -f "$CLUSTER_KEY" -C "hvm-cluster-internal-key" -q
fi
CLUSTER_PUBKEY=$(cat "${CLUSTER_KEY}.pub")
CLUSTER_PRIVKEY=$(cat "$CLUSTER_KEY")

echo "${C_CYAN}==> Ubuntu 24.04 Server cloud image${C_RESET}"
mkdir -p "$ISO_DIR"
NEED_DOWNLOAD=true
if [[ -f "$CLOUD_IMG" ]] && [[ $(stat -c%s "$CLOUD_IMG" 2>/dev/null || echo 0) -gt 104857600 ]]; then
  echo "  ${ICON_OK} Found existing image ($(du -h "$CLOUD_IMG" | cut -f1))"
  NEED_DOWNLOAD=false
fi
if [[ "$NEED_DOWNLOAD" == true ]]; then
  echo "  Downloading..."
  wget -O "$CLOUD_IMG" "$CLOUD_IMG_URL"
fi

echo "  Verifying SHA256 against Ubuntu's published checksum..."
TMP_SHA=$(mktemp)
if wget -q -O "$TMP_SHA" "$CLOUD_SHA_URL"; then
  EXPECTED=$(grep "noble-server-cloudimg-amd64.img" "$TMP_SHA" | awk '{print $1}')
  ACTUAL=$(sha256sum "$CLOUD_IMG" | awk '{print $1}')
  if [[ -n "$EXPECTED" && "$EXPECTED" == "$ACTUAL" ]]; then
    echo "  ${ICON_OK} SHA256 verified"
  else
    echo "  ${ICON_FAIL} SHA256 mismatch (or not found in checksum file)! Expected '$EXPECTED', got '$ACTUAL'"
    rm -f "$TMP_SHA"
    exit 1
  fi
else
  echo "  ${ICON_WARN} Could not fetch checksum file — skipping verification this run"
fi
rm -f "$TMP_SHA"

echo "  Placing a QEMU-readable copy in $IMAGES_DIR (home directory isn't traversable by the qemu system user)..."
if [[ -f "$CLOUD_IMG_LIBVIRT" ]] && [[ "$(sudo sha256sum "$CLOUD_IMG_LIBVIRT" 2>/dev/null | awk '{print $1}')" == "$ACTUAL" ]]; then
  echo "  ${ICON_OK} Already present and matches, skipping copy"
else
  sudo cp "$CLOUD_IMG" "$CLOUD_IMG_LIBVIRT"
  echo "  ${ICON_OK} Copied"
fi
sudo chmod 444 "$CLOUD_IMG_LIBVIRT"

echo "${C_CYAN}==> Ensuring libvirt networks${C_RESET}"
NET_SETUP_OK=true
if sudo virsh -c qemu:///system net-list --name 2>/dev/null | grep -qx "default"; then
  echo "  ${ICON_OK} 'default' network active (Management uses this - same network as Morpheus)"
else
  echo "  ${ICON_FAIL} libvirt's 'default' network isn't active - start it: sudo virsh net-start default"
  NET_SETUP_OK=false
fi
ensure_libvirt_network "$NET_STORAGE" "isolated" || NET_SETUP_OK=false
ensure_libvirt_network "$NET_OVERLAY" "isolated" || NET_SETUP_OK=false
ensure_libvirt_network "$NET_COMPUTE" "isolated" || NET_SETUP_OK=false
if [[ "$NET_SETUP_OK" != true ]]; then
  echo "${ICON_FAIL} One or more networks failed to come up. Aborting before attempting VM builds."
  exit 1
fi

echo "${C_CYAN}==> Reserving static DHCP IPs on 'default' for management interfaces${C_RESET}"
for node in "${NODES[@]}"; do
  read -r n_name n_ip n_mgmt _ <<< "$node"
  sudo virsh -c qemu:///system net-update default add ip-dhcp-host \
    "<host mac='$n_mgmt' ip='$n_ip'/>" --live --config &>/dev/null || true
done
echo "  ${ICON_OK} reservations applied (harmless if already present from an earlier run)"

HOSTS_BLOCK=$(build_hosts_block)

declare -A RESULT_STATUS RESULT_REASON RESULT_MGMT_IP RESULT_MGMT_IF RESULT_STORAGE_IF RESULT_OVERLAY_IF RESULT_COMPUTE_IF RESULT_DISK RESULT_DATA_DISK RESULT_PKGS RESULT_PEER_SSH

# User-writable scratch dir for build/verify status tracking - NOT under $IMAGES_DIR,
# which is root-owned and caused "Permission denied" here (redirects and touch happen
# as the invoking user, before sudo elevation takes effect, even inside a `sudo ...` line).
BUILD_STATUS_DIR=$(mktemp -d)

existing_found=false
for node in "${NODES[@]}"; do
  read -r n_name _ <<< "$node"
  sudo virsh -c qemu:///system dominfo "$n_name" &>/dev/null && existing_found=true
done
if [[ "$existing_found" == true && "$FORCE" != true ]]; then
  echo ""
  echo "${ICON_WARN} One or more of hvm01/02/03 already exist and will be ${C_BOLD}DESTROYED${C_RESET} and rebuilt."
  read -rp "Continue? [y/N] " confirm
  if [[ "$confirm" != "y" && "$confirm" != "Y" ]]; then
    echo "Aborted. Re-run with --force to skip this prompt."
    exit 1
  fi
fi

# =====================================================================
#  Create one VM
# =====================================================================
create_hvm_vm() {
  local name="$1" ip="$2" mac_mgmt="$3" mac_storage="$4" mac_overlay="$5" storage_ip="$6" mac_compute="$7"
  local workdir
  workdir=$(mktemp -d)

  echo "  [$name] Building ($ip)..."
  cleanup_existing_vm "$name"

  cat > "$workdir/meta-data" <<EOF
instance-id: $name-$(date +%s)
local-hostname: $name
EOF

  local pkg_yaml=""
  for p in "${REQUIRED_PKGS[@]}"; do
    pkg_yaml+="  - ${p}"$'\n'
  done

  cat > "$workdir/user-data" <<EOF
#cloud-config
hostname: $name
fqdn: $name
manage_etc_hosts: true
users:
  - name: nixndme
    groups: sudo
    shell: /bin/bash
    sudo: ALL=(ALL) NOPASSWD:ALL
    lock_passwd: false
    ssh_authorized_keys:
      - $SSH_PUBKEY
      - $CLUSTER_PUBKEY
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
$pkg_yaml
write_files:
  - path: /etc/multipath.conf
    content: |
      defaults {
          user_friendly_names yes
          find_multipaths yes
      }
    owner: root:root
    permissions: '0644'
  - path: /home/nixndme/.ssh/hvm_cluster_key
    content: |
$(echo "$CLUSTER_PRIVKEY" | sed 's/^/      /')
    permissions: '0600'
  - path: /home/nixndme/.ssh/hvm_cluster_key.pub
    content: |
      $CLUSTER_PUBKEY
    permissions: '0644'
  - path: /etc/cloud/cloud.cfg.d/99-disable-network-config.cfg
    content: |
      network: {config: disabled}
    owner: root:root
    permissions: '0644'
  - path: /etc/hvm-cluster-hosts
    content: |
$(echo "$HOSTS_BLOCK" | sed 's/^/      /')
    owner: root:root
    permissions: '0644'
runcmd:
  - mkdir -p /home/nixndme/.ssh
  - chown -R nixndme:nixndme /home/nixndme/.ssh
  - chmod 700 /home/nixndme/.ssh
  - chmod 600 /home/nixndme/.ssh/hvm_cluster_key
  - chmod 644 /home/nixndme/.ssh/hvm_cluster_key.pub
  - systemctl enable --now qemu-guest-agent
  - systemctl enable --now ssh
  - systemctl enable --now multipathd
  - systemctl enable --now chrony
  - cat /etc/hvm-cluster-hosts >> /etc/hosts
EOF

  cat > "$workdir/network-config" <<EOF
version: 2
ethernets:
  enp1s0:
    dhcp4: false
    addresses: [$ip/24]
    routes:
      - to: default
        via: $GATEWAY
    nameservers:
      addresses: [$DNS]
  enp2s0:
    dhcp4: false
    addresses: [$storage_ip/24]
  enp3s0:
    dhcp4: false
    optional: true
  enp4s0:
    dhcp4: false
    optional: true
EOF

  sudo xorriso -as genisoimage -output "$IMAGES_DIR/$name-seed.iso" \
    -volid cidata -joliet -rock \
    "$workdir/user-data" "$workdir/meta-data" "$workdir/network-config" &>/dev/null
  rm -rf "$workdir"

  sudo rm -f "$IMAGES_DIR/$name.qcow2" "$IMAGES_DIR/$name-data.qcow2"
  sudo qemu-img create -f qcow2 -F qcow2 -b "$CLOUD_IMG_LIBVIRT" "$IMAGES_DIR/$name.qcow2" "$DISK_SIZE" &>/dev/null
  sudo qemu-img create -f qcow2 "$IMAGES_DIR/$name-data.qcow2" "$DATA_DISK_SIZE" &>/dev/null

  if sudo virt-install --name "$name" \
    --vcpus "$VCPUS" --memory "$MEMORY_MB" \
    --disk path="$IMAGES_DIR/$name.qcow2",format=qcow2,bus=virtio,boot.order=1 \
    --disk path="$IMAGES_DIR/$name-data.qcow2",format=qcow2,bus=virtio,target.dev=vdb \
    --disk path="$IMAGES_DIR/$name-seed.iso",device=cdrom \
    --network network="$NET_MGMT",mac="$mac_mgmt",model=virtio \
    --network network="$NET_STORAGE",mac="$mac_storage",model=virtio \
    --network network="$NET_OVERLAY",mac="$mac_overlay",model=virtio \
    --network network="$NET_COMPUTE",mac="$mac_compute",model=virtio \
    --os-variant ubuntu24.04 \
    --import --noautoconsole > "$BUILD_STATUS_DIR/$name-virt-install.log" 2>&1; then
    echo "  [$name] ${ICON_OK} VM defined and started."
  else
    echo "  [$name] ${ICON_FAIL} virt-install FAILED - see $BUILD_STATUS_DIR/$name-virt-install.log"
    touch "$BUILD_STATUS_DIR/$name.CREATE_FAILED"
  fi
}

# =====================================================================
#  Verification
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

iface_for_mac() {
  local ip="$1" mac="$2"
  ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$ip" \
    "ip -o link show | grep -i '$mac' | awk -F': ' '{print \$2}' | cut -d'@' -f1"
}

verify_vm() {
  local name="$1" ip="$2" mac_mgmt="$3" mac_storage="$4" mac_overlay="$5" expected_storage_ip="$6" mac_compute="$7"
  local resultfile="$RESULTS_DIR/$name.result"
  local -a failures=()

  if [[ -f "$BUILD_STATUS_DIR/$name.CREATE_FAILED" ]]; then
    {
      echo "STATUS=FAILED"
      echo "REASON=virt-install failed during creation - see $BUILD_STATUS_DIR/$name-virt-install.log"
      echo "MGMT_IP=?"; echo "MGMT_IF=?"; echo "STORAGE_IF=?"; echo "OVERLAY_IF=?"; echo "COMPUTE_IF=?"
      echo "DISK=?"; echo "DATA_DISK=?"; echo "PKGS=?"
    } > "$resultfile"
    return
  fi

  if ! wait_for_ssh "$ip"; then
    {
      echo "STATUS=FAILED"
      echo "REASON=SSH unreachable after 3 minutes - VM may not have booted, or network/firewall issue"
      echo "MGMT_IP=?"; echo "MGMT_IF=?"; echo "STORAGE_IF=?"; echo "OVERLAY_IF=?"; echo "COMPUTE_IF=?"
      echo "DISK=?"; echo "DATA_DISK=?"; echo "PKGS=?"
    } > "$resultfile"
    return
  fi

  ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$ip" \
    "sudo tail -n 0 -f /var/log/cloud-init-output.log | sed -u 's/^/    [$name] /'" &
  local tail_pid=$!
  local cloud_init_ok=0
  ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$ip" "sudo cloud-init status --wait" &>/dev/null || cloud_init_ok=1
  kill "$tail_pid" 2>/dev/null; wait "$tail_pid" 2>/dev/null
  [[ $cloud_init_ok -ne 0 ]] && failures+=("cloud-init reported an error - check: ssh nixndme@$ip 'sudo cloud-init status --long'")

  local installed_count
  installed_count=$(ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$ip" \
    "dpkg -s ${REQUIRED_PKGS[*]} 2>/dev/null | grep -c '^Status: install ok installed'")
  [[ "$installed_count" != "${#REQUIRED_PKGS[@]}" ]] && failures+=("packages incomplete ($installed_count/${#REQUIRED_PKGS[@]})")

  local mgmt_if storage_if overlay_if compute_if
  mgmt_if=$(iface_for_mac "$ip" "$mac_mgmt")
  storage_if=$(iface_for_mac "$ip" "$mac_storage")
  overlay_if=$(iface_for_mac "$ip" "$mac_overlay")
  compute_if=$(iface_for_mac "$ip" "$mac_compute")
  [[ -z "$mgmt_if" ]] && failures+=("management interface not found by MAC")
  [[ -z "$storage_if" ]] && failures+=("storage interface not found by MAC")
  [[ -z "$overlay_if" ]] && failures+=("overlay interface not found by MAC")
  [[ -z "$compute_if" ]] && failures+=("compute interface not found by MAC")

  local actual_ip=""
  [[ -n "$mgmt_if" ]] && actual_ip=$(ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$ip" \
    "ip -4 -o addr show '$mgmt_if' | awk '{print \$4}' | cut -d/ -f1")
  [[ "$actual_ip" != "$ip" ]] && failures+=("management IP mismatch (expected $ip, got '${actual_ip:-none}')")

  local actual_storage_ip=""
  [[ -n "$storage_if" ]] && actual_storage_ip=$(ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$ip" \
    "ip -4 -o addr show '$storage_if' | awk '{print \$4}' | cut -d/ -f1")
  [[ "$actual_storage_ip" != "$expected_storage_ip" ]] && \
    failures+=("storage IP mismatch (expected $expected_storage_ip, got '${actual_storage_ip:-none}') - Morpheus's Ceph bootstrap reads this directly")

  local root_disk
  root_disk=$(ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$ip" \
    "lsblk -no pkname \$(findmnt -n -o SOURCE /) 2>/dev/null")
  [[ -n "$root_disk" ]] && root_disk="/dev/${root_disk}" || root_disk="unknown"

  local data_disk
  data_disk=$(ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$ip" \
    "lsblk -ndo NAME | grep -v \"\$(basename $root_disk)\" | grep '^vd' | head -n1")
  [[ -n "$data_disk" ]] && data_disk="/dev/${data_disk}" || { data_disk="NOT FOUND"; failures+=("data disk not found"); }

  local status reason
  if [[ ${#failures[@]} -eq 0 ]]; then
    status="OK"; reason="all checks passed"
  else
    status="FAILED"; reason=$(IFS='; '; echo "${failures[*]}")
  fi

  {
    echo "STATUS=$status"
    echo "REASON=$reason"
    echo "MGMT_IP=$actual_ip"
    echo "MGMT_IF=$mgmt_if"
    echo "STORAGE_IF=$storage_if"
    echo "OVERLAY_IF=$overlay_if"
    echo "COMPUTE_IF=$compute_if"
    echo "DISK=$root_disk"
    echo "DATA_DISK=$data_disk"
    echo "PKGS=${installed_count}/${#REQUIRED_PKGS[@]}"
  } > "$resultfile"
}

verify_peer_ssh() {
  local from_ip="$1"
  shift
  local plain_opts="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -o BatchMode=yes -o LogLevel=ERROR"
  local ok=0 total=0
  for target_ip in "$@"; do
    total=$((total+1))
    if ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$from_ip" \
        "ssh $plain_opts -i ~/.ssh/hvm_cluster_key nixndme@$target_ip true" &>/dev/null; then
      ok=$((ok+1))
    fi
  done
  echo "${ok}/${total}"
}

# =====================================================================
#  Main flow
# =====================================================================
echo ""
echo "${C_CYAN}==> Building all 3 VMs in parallel${C_RESET}"
for node in "${NODES[@]}"; do
  read -r n_name n_ip n_mgmt n_storage n_overlay n_storage_ip n_compute <<< "$node"
  create_hvm_vm "$n_name" "$n_ip" "$n_mgmt" "$n_storage" "$n_overlay" "$n_storage_ip" "$n_compute" &
done
wait
build_failed=false
for node in "${NODES[@]}"; do
  read -r n_name _ <<< "$node"
  [[ -f "$BUILD_STATUS_DIR/$n_name.CREATE_FAILED" ]] && build_failed=true
done
if [[ "$build_failed" == true ]]; then
  echo "${ICON_WARN} One or more VMs failed to build — see per-node messages above and logs in $BUILD_STATUS_DIR"
else
  echo "${ICON_OK} All 3 VMs created."
fi

RESULTS_DIR=$(mktemp -d)
echo ""
echo "${C_CYAN}==> Verifying all 3 in parallel${C_RESET} (live output interleaves per-node, prefixed by hostname)"
for node in "${NODES[@]}"; do
  read -r n_name n_ip n_mgmt n_storage n_overlay n_storage_ip n_compute <<< "$node"
  verify_vm "$n_name" "$n_ip" "$n_mgmt" "$n_storage" "$n_overlay" "$n_storage_ip" "$n_compute" &
done
wait

for node in "${NODES[@]}"; do
  read -r n_name _ <<< "$node"
  resultfile="$RESULTS_DIR/$n_name.result"
  [[ -f "$resultfile" ]] || continue
  while IFS='=' read -r key val; do
    case "$key" in
      STATUS)      RESULT_STATUS[$n_name]="$val" ;;
      REASON)      RESULT_REASON[$n_name]="$val" ;;
      MGMT_IP)     RESULT_MGMT_IP[$n_name]="$val" ;;
      MGMT_IF)     RESULT_MGMT_IF[$n_name]="$val" ;;
      STORAGE_IF)  RESULT_STORAGE_IF[$n_name]="$val" ;;
      OVERLAY_IF)  RESULT_OVERLAY_IF[$n_name]="$val" ;;
      COMPUTE_IF)  RESULT_COMPUTE_IF[$n_name]="$val" ;;
      DISK)        RESULT_DISK[$n_name]="$val" ;;
      DATA_DISK)   RESULT_DATA_DISK[$n_name]="$val" ;;
      PKGS)        RESULT_PKGS[$n_name]="$val" ;;
    esac
  done < "$resultfile"
done
rm -rf "$RESULTS_DIR"

# Clean up seed ISOs for nodes that verified OK (no longer needed post-first-boot)
for node in "${NODES[@]}"; do
  read -r n_name _ <<< "$node"
  if [[ "${RESULT_STATUS[$n_name]:-}" == "OK" ]]; then
    sudo rm -f "$IMAGES_DIR/$n_name-seed.iso"
  fi
done

echo ""
echo "${C_CYAN}==> Checking node-to-node SSH${C_RESET} (via dedicated cluster key)"
IPS=()
for node in "${NODES[@]}"; do read -r _ n_ip _ <<< "$node"; IPS+=("$n_ip"); done
for node in "${NODES[@]}"; do
  read -r n_name n_ip _ <<< "$node"
  others=()
  for ip in "${IPS[@]}"; do [[ "$ip" != "$n_ip" ]] && others+=("$ip"); done
  RESULT_PEER_SSH[$n_name]=$(verify_peer_ssh "$n_ip" "${others[@]}")
done

# ---- Summary ----
ok_count=0
echo ""
echo "${C_BOLD}══════════════════════════════════════════════════════════════════════════${C_RESET}"
echo "${C_BOLD}  HVM NODE BUILD SUMMARY${C_RESET}"
echo "${C_BOLD}══════════════════════════════════════════════════════════════════════════${C_RESET}"
for node in "${NODES[@]}"; do
  read -r n_name n_ip n_mgmt n_storage n_overlay n_storage_ip n_compute <<< "$node"
  status="${RESULT_STATUS[$n_name]:-FAILED}"
  if [[ "$status" == "OK" ]]; then
    status_line="${ICON_OK} ${C_GREEN}OK${C_RESET}"
    ok_count=$((ok_count+1))
  else
    status_line="${ICON_FAIL} ${C_RED}FAILED${C_RESET}"
  fi
  echo ""
  echo "  ${C_BOLD}${n_name}${C_RESET}  —  ${status_line}"
  echo "  ├─ Management   ${RESULT_MGMT_IF[$n_name]:-?} → ${RESULT_MGMT_IP[$n_name]:-?}  ${C_DIM}(Morpheus 'Management Net Interface')${C_RESET}"
  echo "  ├─ Storage      ${RESULT_STORAGE_IF[$n_name]:-?} → ${n_storage_ip}  ${C_DIM}(Morpheus 'Storage Net Interface')${C_RESET}"
  echo "  ├─ Overlay      ${RESULT_OVERLAY_IF[$n_name]:-?}  ${C_DIM}(Morpheus 'Overlay Net Interface')${C_RESET}"
  echo "  ├─ Compute      ${RESULT_COMPUTE_IF[$n_name]:-?}  ${C_DIM}(dedicated NIC, keeps VXLAN off Storage)${C_RESET}"
  echo "  ├─ Root disk    ${RESULT_DISK[$n_name]:-?}  ${C_DIM}(OS - do NOT use as Data Device)${C_RESET}"
  echo "  ├─ Data disk    ${RESULT_DATA_DISK[$n_name]:-?}  ${C_DIM}(use as Morpheus 'Data Device')${C_RESET}"
  echo "  ├─ Packages     ${RESULT_PKGS[$n_name]:-?}"
  echo "  └─ Peer SSH     ${RESULT_PEER_SSH[$n_name]:-?}"
  if [[ "$status" != "OK" ]]; then
    echo "     ${ICON_WARN} ${C_YELLOW}Reason:${C_RESET} ${RESULT_REASON[$n_name]:-unknown}"
  fi
done

ELAPSED=$(( $(date +%s) - SCRIPT_START ))
echo ""
echo "${C_BOLD}══════════════════════════════════════════════════════════════════════════${C_RESET}"
if [[ "$ok_count" -eq ${#NODES[@]} ]]; then
  echo "  ${ICON_OK} ${C_GREEN}${C_BOLD}All ${#NODES[@]} nodes OK${C_RESET}  ·  Total time: $((ELAPSED / 60))m $((ELAPSED % 60))s"
else
  echo "  ${ICON_WARN} ${C_YELLOW}${C_BOLD}${ok_count}/${#NODES[@]} nodes OK${C_RESET}  ·  Total time: $((ELAPSED / 60))m $((ELAPSED % 60))s"
  echo "  ${C_DIM}See 'Reason' lines above for what to fix on the failed node(s).${C_RESET}"
fi
echo "  SSH in manually anytime: ssh -i $SSH_KEY nixndme@${MGMT_SUBNET}.201  (and .202, .203)"
echo "  Full log: $LOG_FILE"
echo "  Per-node virt-install logs (if any failures): $BUILD_STATUS_DIR"
echo "${C_BOLD}══════════════════════════════════════════════════════════════════════════${C_RESET}"
