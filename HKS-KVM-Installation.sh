#!/usr/bin/env bash
# Creates 4 plain Ubuntu 24.04 VMs (1 master + 3 workers) for Morpheus's HKS
# "existing cluster" wizard to SSH into and build Kubernetes on directly - these
# are NOT provisioned by Morpheus itself, unlike the HVM nodes. Re-runnable:
# destroys/rebuilds existing VMs of the same name (asks first, unless --force).
# Run as your normal user (uses sudo internally).

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

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LOG_DIR="$SCRIPT_DIR/logs"
mkdir -p "$LOG_DIR"
LOG_FILE="$LOG_DIR/hks-build-$(date +%Y%m%d-%H%M%S).log"
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
# reference; if the HVM script already placed one here, this reuses it as-is.
CLOUD_IMG_LIBVIRT="$IMAGES_DIR/noble-server-cloudimg-amd64.img"

VCPUS=2
MEMORY_MB=4096
DISK_SIZE="40G"
DATA_DISK_SIZE="50G"   # LVM-backed container storage for K8s - much lighter than HVM's Ceph sizing

MGMT_SUBNET="192.168.122"   # same network as Morpheus itself - required for it to SSH in
GATEWAY="${MGMT_SUBNET}.1"
DNS="${MGMT_SUBNET}.1"

SSH_KEY="$HOME/.ssh/id_ed25519"   # host verification only - same identity used by the HVM script
REQUIRED_PKGS=(vim curl openssh-server qemu-guest-agent chrony)

SSH_CM_DIR="$HOME/.ssh/cm-sockets"
rm -f "$SSH_CM_DIR"/* 2>/dev/null
mkdir -p "$SSH_CM_DIR"
SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -o BatchMode=yes -o LogLevel=ERROR -o ControlMaster=auto -o ControlPersist=2m -o ControlPath=$SSH_CM_DIR/%r@%h:%p"

# Node definitions: name ip mac
NODES=(
  "hks-master   ${MGMT_SUBNET}.230 52:54:00:bb:01:01"
  "hks-worker-1 ${MGMT_SUBNET}.231 52:54:00:bb:01:02"
  "hks-worker-2 ${MGMT_SUBNET}.232 52:54:00:bb:01:03"
  "hks-worker-3 ${MGMT_SUBNET}.233 52:54:00:bb:01:04"
)

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
  echo "  ${ICON_OK} 'default' network active (same network Morpheus itself is on)"
else
  echo "  ${ICON_FAIL} libvirt's 'default' network isn't active - start it: sudo virsh net-start default"
  exit 1
fi

AVAIL_GB=$(( $(df --output=avail "$IMAGES_DIR" 2>/dev/null | tail -1) / 1024 / 1024 ))
if [[ "$AVAIL_GB" -lt 40 ]]; then
  echo "  ${ICON_WARN} only ${AVAIL_GB}GB free on images filesystem"
else
  echo "  ${ICON_OK} ${AVAIL_GB}GB free on images filesystem"
fi

FREE_MB=$(awk '/MemAvailable/ {print int($2/1024)}' /proc/meminfo)
NEEDED_MB=$(( ${#NODES[@]} * MEMORY_MB + 2048 ))
if [[ "$FREE_MB" -lt "$NEEDED_MB" ]]; then
  echo "  ${ICON_WARN} only ${FREE_MB}MB available RAM (want ~${NEEDED_MB}MB for ${#NODES[@]} VMs) - check what else is running (Morpheus + HVM nodes add up fast)"
else
  echo "  ${ICON_OK} ${FREE_MB}MB available RAM"
fi

# ---- Password: prompted, hashed for the guest, but you'll need to remember the
# plaintext yourself - Morpheus's own HKS wizard needs to type it into its
# SSH PASSWORD field to actually log in and build the cluster ----
if [[ -z "${ADMIN_PASSWORD:-}" ]]; then
  read -rsp "Set admin password for 'nixndme' on all 4 HKS nodes (remember this - Morpheus's wizard needs it too): " ADMIN_PASSWORD
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
echo "  ${ICON_OK} admin password hashed for the guests (remember the plaintext for Morpheus's wizard)"

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
  wget -O "$CLOUD_IMG" "$CLOUD_IMG_URL"
fi

echo "  Verifying SHA256..."
TMP_SHA=$(mktemp)
ACTUAL=""
if wget -q -O "$TMP_SHA" "$CLOUD_SHA_URL"; then
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

echo "  Placing a QEMU-readable copy in $IMAGES_DIR (reused as-is if the HVM script already put one there)..."
if [[ -f "$CLOUD_IMG_LIBVIRT" ]] && [[ -n "$ACTUAL" ]] && [[ "$(sudo sha256sum "$CLOUD_IMG_LIBVIRT" 2>/dev/null | awk '{print $1}')" == "$ACTUAL" ]]; then
  echo "  ${ICON_OK} Already present and matches, skipping copy"
else
  sudo cp "$CLOUD_IMG" "$CLOUD_IMG_LIBVIRT"
  echo "  ${ICON_OK} Copied"
fi
sudo chmod 444 "$CLOUD_IMG_LIBVIRT"

echo "${C_CYAN}==> Reserving static DHCP IPs on 'default'${C_RESET}"
for node in "${NODES[@]}"; do
  read -r n_name n_ip n_mac <<< "$node"
  sudo virsh -c qemu:///system net-update default add ip-dhcp-host \
    "<host mac='$n_mac' ip='$n_ip'/>" --live --config &>/dev/null || true
done
echo "  ${ICON_OK} reservations applied (harmless if already present)"

declare -A RESULT_STATUS RESULT_REASON RESULT_IP RESULT_DISK RESULT_DATA_DISK RESULT_PKGS
BUILD_STATUS_DIR=$(mktemp -d)

existing_found=false
for node in "${NODES[@]}"; do
  read -r n_name _ <<< "$node"
  sudo virsh -c qemu:///system dominfo "$n_name" &>/dev/null && existing_found=true
done
if [[ "$existing_found" == true && "$FORCE" != true ]]; then
  echo ""
  echo "${ICON_WARN} One or more HKS nodes already exist and will be ${C_BOLD}DESTROYED${C_RESET} and rebuilt."
  read -rp "Continue? [y/N] " confirm
  if [[ "$confirm" != "y" && "$confirm" != "Y" ]]; then
    echo "Aborted. Re-run with --force to skip this prompt."
    exit 1
  fi
fi

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
#  Create one VM
# =====================================================================
create_hks_vm() {
  local name="$1" ip="$2" mac="$3"
  local workdir
  workdir=$(mktemp -d)

  echo "  [$name] Building ($ip)..."
  cleanup_existing_vm "$name"

  cat > "$workdir/meta-data" <<EOF
instance-id: $name-$(date +%s)
local-hostname: $name
EOF

  cat > "$workdir/user-data" <<EOF
#cloud-config
hostname: $name
fqdn: $name.local
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
    addresses: [$ip/24]
    routes:
      - to: default
        via: $GATEWAY
    nameservers:
      addresses: [$DNS]
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
    --network network=default,mac="$mac",model=virtio \
    --os-variant ubuntu24.04 \
    --import --noautoconsole > "$BUILD_STATUS_DIR/$name-virt-install.log" 2>&1; then
    echo "  [$name] ${ICON_OK} VM defined and started."
  else
    echo "  [$name] ${ICON_FAIL} virt-install FAILED - see $BUILD_STATUS_DIR/$name-virt-install.log"
    touch "$BUILD_STATUS_DIR/$name.CREATE_FAILED"
  fi
}

wait_for_ssh() {
  local ip="$1" max_attempts=90 attempt=0
  while ! ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$ip" true &>/dev/null; do
    attempt=$((attempt+1))
    [[ $attempt -ge $max_attempts ]] && return 1
    sleep 2
  done
  return 0
}

verify_vm() {
  local name="$1" ip="$2" mac="$3"
  local resultfile="$RESULTS_DIR/$name.result"
  local -a failures=()

  if [[ -f "$BUILD_STATUS_DIR/$name.CREATE_FAILED" ]]; then
    { echo "STATUS=FAILED"; echo "REASON=virt-install failed - see $BUILD_STATUS_DIR/$name-virt-install.log"
      echo "IP=?"; echo "DISK=?"; echo "DATA_DISK=?"; echo "PKGS=?"
    } > "$resultfile"
    return
  fi

  if ! wait_for_ssh "$ip"; then
    { echo "STATUS=FAILED"; echo "REASON=SSH unreachable after 3 minutes"
      echo "IP=?"; echo "DISK=?"; echo "DATA_DISK=?"; echo "PKGS=?"
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

  local actual_ip
  actual_ip=$(ssh $SSH_OPTS -i "$SSH_KEY" "nixndme@$ip" \
    "ip -4 -o addr show enp1s0 | awk '{print \$4}' | cut -d/ -f1")
  [[ "$actual_ip" != "$ip" ]] && failures+=("IP mismatch (expected $ip, got '${actual_ip:-none}')")

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
    echo "STATUS=$status"; echo "REASON=$reason"; echo "IP=$actual_ip"
    echo "DISK=$root_disk"; echo "DATA_DISK=$data_disk"; echo "PKGS=${installed_count}/${#REQUIRED_PKGS[@]}"
  } > "$resultfile"
}

# =====================================================================
#  Main flow
# =====================================================================
echo ""
echo "${C_CYAN}==> Building all 4 VMs in parallel${C_RESET}"
for node in "${NODES[@]}"; do
  read -r n_name n_ip n_mac <<< "$node"
  create_hks_vm "$n_name" "$n_ip" "$n_mac" &
done
wait

build_failed=false
for node in "${NODES[@]}"; do
  read -r n_name _ <<< "$node"
  [[ -f "$BUILD_STATUS_DIR/$n_name.CREATE_FAILED" ]] && build_failed=true
done
if [[ "$build_failed" == true ]]; then
  echo "${ICON_WARN} One or more VMs failed to build — see messages above"
else
  echo "${ICON_OK} All 4 VMs created."
fi

RESULTS_DIR=$(mktemp -d)
echo ""
echo "${C_CYAN}==> Verifying all 4 in parallel${C_RESET}"
for node in "${NODES[@]}"; do
  read -r n_name n_ip n_mac <<< "$node"
  verify_vm "$n_name" "$n_ip" "$n_mac" &
done
wait

for node in "${NODES[@]}"; do
  read -r n_name _ <<< "$node"
  resultfile="$RESULTS_DIR/$n_name.result"
  [[ -f "$resultfile" ]] || continue
  while IFS='=' read -r key val; do
    case "$key" in
      STATUS)    RESULT_STATUS[$n_name]="$val" ;;
      REASON)    RESULT_REASON[$n_name]="$val" ;;
      IP)        RESULT_IP[$n_name]="$val" ;;
      DISK)      RESULT_DISK[$n_name]="$val" ;;
      DATA_DISK) RESULT_DATA_DISK[$n_name]="$val" ;;
      PKGS)      RESULT_PKGS[$n_name]="$val" ;;
    esac
  done < "$resultfile"
done
rm -rf "$RESULTS_DIR"

# ---- Summary ----
ok_count=0
echo ""
echo "${C_BOLD}══════════════════════════════════════════════════════════════════════════${C_RESET}"
echo "${C_BOLD}  HKS NODE BUILD SUMMARY${C_RESET}"
echo "${C_BOLD}══════════════════════════════════════════════════════════════════════════${C_RESET}"
for node in "${NODES[@]}"; do
  read -r n_name n_ip n_mac <<< "$node"
  status="${RESULT_STATUS[$n_name]:-FAILED}"
  if [[ "$status" == "OK" ]]; then
    status_line="${ICON_OK} ${C_GREEN}OK${C_RESET}"; ok_count=$((ok_count+1))
  else
    status_line="${ICON_FAIL} ${C_RED}FAILED${C_RESET}"
  fi
  echo ""
  echo "  ${C_BOLD}${n_name}${C_RESET}  —  ${status_line}"
  echo "  ├─ IP           ${RESULT_IP[$n_name]:-?}  ${C_DIM}(Morpheus wizard: Name=$n_name, IP field)${C_RESET}"
  echo "  ├─ Root disk    ${RESULT_DISK[$n_name]:-?}"
  echo "  ├─ Data disk    ${RESULT_DATA_DISK[$n_name]:-?}  ${C_DIM}(Morpheus wizard 'DATA DEVICE' - use this exact path, NOT the placeholder /dev/sdb)${C_RESET}"
  echo "  └─ Packages     ${RESULT_PKGS[$n_name]:-?}"
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
fi
echo ""
echo "${C_BOLD}Values for Morpheus's Create Cluster wizard (HKS layout):${C_RESET}"
echo "  MASTER HOSTS:   hks-master   → ${RESULT_IP[hks-master]:-?}"
echo "  WORKER HOSTS:   hks-worker-1 → ${RESULT_IP[hks-worker-1]:-?}"
echo "                  hks-worker-2 → ${RESULT_IP[hks-worker-2]:-?}"
echo "                  hks-worker-3 → ${RESULT_IP[hks-worker-3]:-?}"
echo "  SSH PORT:       22"
echo "  SSH USERNAME:   nixndme"
echo "  SSH PASSWORD:   (the one you just typed - not shown here for safety)"
echo "  DATA DEVICE:    /dev/vdb   (NOT the placeholder /dev/sdb shown in the form)"
echo "  NETWORK INTERFACE: enp1s0   (NOT the placeholder eth0 - Ubuntu 24.04 predictable naming)"
echo ""
echo "  Full log: $LOG_FILE"
echo "${C_BOLD}══════════════════════════════════════════════════════════════════════════${C_RESET}"
