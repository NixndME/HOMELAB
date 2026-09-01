#!/usr/bin/env bash
#
# deploy-sno.sh — spins up a Single Node OpenShift cluster on local libvirt,
# then layers on OpenShift Virtualization (CNV) and a single-node Rook-Ceph
# so it has RWX+Block storage, and finishes with a ready-to-paste Morpheus
# onboarding summary (API URL + ServiceAccount token).
#
# Usage: ./deploy-sno.sh <phase>
#   phases: preflight fetch_tools render_configs create_iso create_vm
#           wait_install install_cnv_and_storage morpheus_sa destroy all
#
# Each phase is safe to re-run on its own (idempotent-ish) except `destroy`,
# which asks for confirmation unless FORCE=1 is set. Run `all` for the full
# ride, or step through phases individually while debugging.
set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration (override any of these via environment variables)
# ---------------------------------------------------------------------------
CLUSTER_NAME="${CLUSTER_NAME:-sno}"
BASE_DOMAIN="${BASE_DOMAIN:-lab.local}"
CLUSTER_DOMAIN="${CLUSTER_NAME}.${BASE_DOMAIN}"

VM_NAME="${VM_NAME:-sno}"
VM_VCPUS="${VM_VCPUS:-14}"
VM_RAM_MB="${VM_RAM_MB:-24576}"
VM_DISK1_GB="${VM_DISK1_GB:-130}"     # RHCOS installs itself onto one of the two disks
VM_DISK2_GB="${VM_DISK2_GB:-100}"     # ...and Rook-Ceph claims whichever one is left over
LIBVIRT_NET="${LIBVIRT_NET:-default}"
STORAGE_POOL="${STORAGE_POOL:-default}"
STATIC_IP="${STATIC_IP:-192.168.122.50}"
GATEWAY="${GATEWAY:-192.168.122.1}"
DNS_SERVER="${DNS_SERVER:-192.168.122.1}"
PREFIX_LEN="${PREFIX_LEN:-24}"
MAC_ADDRESS="${MAC_ADDRESS:-52:54:00:aa:bb:50}"
IFACE_NAME="${IFACE_NAME:-enp1s0}"    # first virtio NIC on q35 under RHCOS predictable naming

# OCP client/installer channel. 4.21 may not yet exist on mirror.openshift.com
# at the time you run this — override OCP_CHANNEL if fetch_tools() 404s.
OCP_CHANNEL="${OCP_CHANNEL:-stable-4.21}"
OCP_MIRROR_BASE="${OCP_MIRROR_BASE:-https://mirror.openshift.com/pub/openshift-v4/x86_64/clients/ocp}"

ROOK_VERSION="${ROOK_VERSION:-v1.15.4}"
CEPH_IMAGE="${CEPH_IMAGE:-quay.io/ceph/ceph:v18.2.4}"
# Guest-visible virtio-blk naming (vda/vdb) isn't guaranteed to match the
# order disks were defined in — coreos-installer picks whichever disk it
# finds first, which isn't always the "first" one. If storage comes up with
# 0 OSDs, check `oc logs -n rook-ceph <osd-prepare pod>` to see which device
# is actually empty, then override this.
CEPH_DEVICE="${CEPH_DEVICE:-vda}"

WORKDIR="${WORKDIR:-$(pwd)/sno-deploy}"
BIN_DIR="${WORKDIR}/bin"
ASSETS_DIR="${WORKDIR}/ocp-install"
SRC_DIR="${ASSETS_DIR}/sources"
BUILD_DIR="${ASSETS_DIR}/build"
ISO_DIR="${WORKDIR}/iso"
LOG_DIR="${WORKDIR}/logs"
MANIFEST_DIR="${WORKDIR}/manifests"
KUBECONFIG_PATH="${BUILD_DIR}/auth/kubeconfig"

export PATH="${BIN_DIR}:${PATH}"

HOSTS_MARKER="# added-by-deploy-sno.sh:${CLUSTER_DOMAIN}"

# ---------------------------------------------------------------------------
# Logging / helpers
# ---------------------------------------------------------------------------
log()   { printf '\n\033[1;34m[%s]\033[0m %s\n' "$(date +%H:%M:%S)" "$*"; }
warn()  { printf '\n\033[1;33m[WARN]\033[0m %s\n' "$*" >&2; }
fail()  { printf '\n\033[1;31m[ERROR]\033[0m %s\n' "$*" >&2; exit 1; }

require_cmd() {
  local missing=()
  for c in "$@"; do
    command -v "$c" >/dev/null 2>&1 || missing+=("$c")
  done
  if [ "${#missing[@]}" -gt 0 ]; then
    printf '%s\n' "${missing[@]}"
    return 1
  fi
}

# ---------------------------------------------------------------------------
# preflight
# ---------------------------------------------------------------------------
preflight() {
  log "Running pre-flight checks..."
  mkdir -p "$WORKDIR" "$BIN_DIR" "$ASSETS_DIR" "$SRC_DIR" "$ISO_DIR" "$LOG_DIR" "$MANIFEST_DIR"

  local problems=()

  # --- required host tooling ---
  local missing_cmds
  if ! missing_cmds=$(require_cmd virsh virt-install qemu-img curl tar jq openssl python3 ssh-keygen); then
    while IFS= read -r m; do problems+=("missing command: ${m} (install via pacman)"); done <<<"$missing_cmds"
  fi

  # Confirmed hard requirement: 'openshift-install agent create image' shells
  # out to nmstatectl to validate agent-config.yaml's static networkConfig,
  # and fails outright without it. It's AUR-only on Arch (not in
  # core/extra/multilib).
  if ! command -v nmstatectl >/dev/null 2>&1; then
    local aur_helper="yay -S nmstate"
    command -v yay >/dev/null 2>&1 || { command -v paru >/dev/null 2>&1 && aur_helper="paru -S nmstate"; }
    problems+=("nmstatectl not found (AUR-only package) — install with: ${aur_helper}")
  fi

  # edk2-ovmf firmware for UEFI boot
  local ovmf_found=0
  for p in /usr/share/edk2/x64/OVMF_CODE.4m.fd /usr/share/edk2-ovmf/x64/OVMF_CODE.fd \
           /usr/share/OVMF/OVMF_CODE.fd /usr/share/edk2/ovmf/OVMF_CODE.fd; do
    [ -f "$p" ] && ovmf_found=1 && break
  done
  [ "$ovmf_found" -eq 1 ] || problems+=("edk2-ovmf firmware not found — install with: sudo pacman -S edk2-ovmf")

  # --- libvirt reachability ---
  if ! virsh -c qemu:///system list >/dev/null 2>&1; then
    problems+=("cannot reach qemu:///system — is libvirtd running and is \$USER in the 'libvirt' group?")
  fi
  if ! virsh -c qemu:///system net-info "$LIBVIRT_NET" >/dev/null 2>&1; then
    problems+=("libvirt network '${LIBVIRT_NET}' not found/active — check 'virsh net-list --all'")
  fi
  local pool_state
  pool_state=$(virsh -c qemu:///system pool-info "$STORAGE_POOL" 2>/dev/null | awk -F': +' '/^State/{print $2}')
  if [ -z "$pool_state" ]; then
    problems+=("libvirt storage pool '${STORAGE_POOL}' not found — create it (e.g. 'virsh pool-define-as default dir --target /var/lib/libvirt/images && virsh pool-autostart default') or check 'virsh pool-list --all'")
  elif [ "$pool_state" != "running" ]; then
    problems+=("libvirt storage pool '${STORAGE_POOL}' exists but is not running (state: ${pool_state}) — 'virsh pool-start ${STORAGE_POOL}'")
  fi

  # --- nested virtualization ---
  local nested="0"
  if [ -f /sys/module/kvm_intel/parameters/nested ]; then
    nested=$(cat /sys/module/kvm_intel/parameters/nested)
  elif [ -f /sys/module/kvm_amd/parameters/nested ]; then
    nested=$(cat /sys/module/kvm_amd/parameters/nested)
  fi
  case "$nested" in
    1|Y|y) ;;
    *) problems+=("nested virtualization not enabled (kvm_intel/kvm_amd 'nested' != 1) — required for CNV/KubeVirt inside this VM") ;;
  esac

  # --- host resources ---
  local total_mem_mb
  total_mem_mb=$(awk '/MemTotal/{print int($2/1024)}' /proc/meminfo)
  local need_mem_mb=$((VM_RAM_MB + 4096))
  [ "$total_mem_mb" -ge "$need_mem_mb" ] || \
    problems+=("host has ${total_mem_mb}MB RAM, need >= ${need_mem_mb}MB (VM RAM + 4GB headroom)")

  local host_cpus
  host_cpus=$(nproc)
  [ "$host_cpus" -ge "$VM_VCPUS" ] || \
    warn "host has ${host_cpus} logical CPUs but VM requests ${VM_VCPUS} vCPUs — will run but oversubscribed"

  local images_dir avail_gb
  images_dir=$(libvirt_images_dir)
  avail_gb=$(df --output=avail -BG "$images_dir" 2>/dev/null | tail -1 | tr -dc '0-9')
  local need_gb=$((VM_DISK1_GB + VM_DISK2_GB + 15))
  if [ -n "${avail_gb:-}" ] && [ "$avail_gb" -lt "$need_gb" ]; then
    problems+=("only ${avail_gb}GB free at ${images_dir}, need >= ${need_gb}GB")
  fi

  # --- pull secret / ssh key resolution (fills globals, also validates) ---
  resolve_pull_secret || problems+=("no pull secret found at ./pull-secret.json or ~/.openshift/pull-secret.json")
  resolve_ssh_key

  if [ "${#problems[@]}" -gt 0 ]; then
    warn "Pre-flight found ${#problems[@]} problem(s):"
    printf '  - %s\n' "${problems[@]}" >&2
    fail "resolve the above before continuing"
  fi

  log "Pre-flight OK. Node: ${VM_NAME}, ${VM_VCPUS} vCPU / ${VM_RAM_MB}MB, images dir: $(libvirt_images_dir)"
}

libvirt_images_dir() {
  local p
  p=$(virsh -c qemu:///system pool-dumpxml "$STORAGE_POOL" 2>/dev/null | sed -n 's:.*<path>\(.*\)</path>.*:\1:p')
  echo "${p:-/var/lib/libvirt/images}"
}

resolve_pull_secret() {
  for f in ./pull-secret.json "${HOME}/.openshift/pull-secret.json"; do
    if [ -s "$f" ]; then
      PULL_SECRET_FILE=$(cd "$(dirname "$f")" && pwd)/$(basename "$f")
      jq -e . "$PULL_SECRET_FILE" >/dev/null 2>&1 || fail "pull secret at ${PULL_SECRET_FILE} is not valid JSON"
      return 0
    fi
  done
  return 1
}

resolve_ssh_key() {
  for f in "${HOME}/.ssh/id_ed25519.pub" "${HOME}/.ssh/id_rsa.pub"; do
    if [ -s "$f" ]; then
      SSH_PUB_KEY=$(cat "$f")
      return 0
    fi
  done
  log "No SSH public key found — generating ed25519 keypair for node debug access..."
  ssh-keygen -t ed25519 -N "" -f "${HOME}/.ssh/id_ed25519" -C "deploy-sno.sh"
  SSH_PUB_KEY=$(cat "${HOME}/.ssh/id_ed25519.pub")
}

# ---------------------------------------------------------------------------
# fetch_tools
# ---------------------------------------------------------------------------
fetch_tools() {
  mkdir -p "$BIN_DIR"
  log "Fetching openshift-install and oc (${OCP_CHANNEL}) into ${BIN_DIR}..."

  if [ ! -x "${BIN_DIR}/openshift-install" ]; then
    curl -fL "${OCP_MIRROR_BASE}/${OCP_CHANNEL}/openshift-install-linux.tar.gz" \
      -o /tmp/openshift-install.tar.gz \
      || fail "download failed — check OCP_CHANNEL=${OCP_CHANNEL} exists at ${OCP_MIRROR_BASE}"
    tar -xzf /tmp/openshift-install.tar.gz -C "$BIN_DIR" openshift-install
    rm -f /tmp/openshift-install.tar.gz
  else
    log "openshift-install already present, skipping download."
  fi

  if [ ! -x "${BIN_DIR}/oc" ]; then
    curl -fL "${OCP_MIRROR_BASE}/${OCP_CHANNEL}/openshift-client-linux.tar.gz" \
      -o /tmp/openshift-client.tar.gz \
      || fail "download failed for oc client"
    tar -xzf /tmp/openshift-client.tar.gz -C "$BIN_DIR" oc kubectl
    rm -f /tmp/openshift-client.tar.gz
  else
    log "oc already present, skipping download."
  fi

  chmod +x "${BIN_DIR}/openshift-install" "${BIN_DIR}/oc" "${BIN_DIR}/kubectl" 2>/dev/null || true
  "${BIN_DIR}/openshift-install" version
  "${BIN_DIR}/oc" version --client
}

# ---------------------------------------------------------------------------
# render_configs
# ---------------------------------------------------------------------------
render_configs() {
  resolve_pull_secret || fail "no pull secret found"
  resolve_ssh_key
  mkdir -p "$SRC_DIR"

  log "Rendering install-config.yaml and agent-config.yaml into ${SRC_DIR}..."

  cat > "${SRC_DIR}/install-config.yaml" <<EOF
apiVersion: v1
baseDomain: ${BASE_DOMAIN}
metadata:
  name: ${CLUSTER_NAME}
compute:
- name: worker
  replicas: 0
controlPlane:
  name: master
  replicas: 1
networking:
  networkType: OVNKubernetes
  clusterNetwork:
  - cidr: 10.128.0.0/14
    hostPrefix: 23
  serviceNetwork:
  - 172.30.0.0/16
  machineNetwork:
  - cidr: 192.168.122.0/24
platform:
  none: {}
pullSecret: '$(cat "$PULL_SECRET_FILE")'
sshKey: "${SSH_PUB_KEY}"
EOF

  cat > "${SRC_DIR}/agent-config.yaml" <<EOF
apiVersion: v1alpha1
metadata:
  name: ${CLUSTER_NAME}
rendezvousIP: ${STATIC_IP}
hosts:
  - hostname: ${VM_NAME}
    role: master
    interfaces:
      - name: ${IFACE_NAME}
        macAddress: "${MAC_ADDRESS}"
    networkConfig:
      interfaces:
        - name: ${IFACE_NAME}
          type: ethernet
          state: up
          mac-address: "${MAC_ADDRESS}"
          ipv4:
            enabled: true
            dhcp: false
            address:
              - ip: ${STATIC_IP}
                prefix-length: ${PREFIX_LEN}
      dns-resolver:
        config:
          server:
            - ${DNS_SERVER}
      routes:
        config:
          - destination: 0.0.0.0/0
            next-hop-address: ${GATEWAY}
            next-hop-interface: ${IFACE_NAME}
EOF

  if command -v nmstatectl >/dev/null 2>&1; then
    log "Validating networkConfig with nmstatectl..."
    python3 - "$SRC_DIR/agent-config.yaml" <<'PYEOF'
import sys, subprocess
try:
    import yaml
except ImportError:
    sys.exit(0)  # PyYAML not installed — skip local validation, installer still validates at build time
with open(sys.argv[1]) as f:
    doc = yaml.safe_load(f)
net = doc["hosts"][0]["networkConfig"]
p = subprocess.run(["nmstatectl", "show"], capture_output=True, text=True)
print("(nmstatectl available; full network state validation happens during 'agent create image')")
PYEOF
  fi

  log "Config rendered. IMPORTANT: verify interface name '${IFACE_NAME}' matches what RHCOS assigns your virtio NIC on q35 (check after first boot with 'nmcli device' over the console if networking doesn't come up)."
}

# ---------------------------------------------------------------------------
# create_iso
# ---------------------------------------------------------------------------
create_iso() {
  [ -f "${SRC_DIR}/install-config.yaml" ] || fail "run render_configs first"
  [ -x "${BIN_DIR}/openshift-install" ] || fail "run fetch_tools first"

  log "Preparing build dir and generating agent ISO (this fetches the release payload — needs internet access once, not during node boot)..."
  rm -rf "$BUILD_DIR"
  mkdir -p "$BUILD_DIR"
  cp "${SRC_DIR}/install-config.yaml" "${SRC_DIR}/agent-config.yaml" "$BUILD_DIR/"

  "${BIN_DIR}/openshift-install" agent create image --dir="$BUILD_DIR" --log-level=info \
    | tee "${LOG_DIR}/create-image.log"

  [ -f "${BUILD_DIR}/agent.x86_64.iso" ] || fail "ISO generation did not produce agent.x86_64.iso"
  cp "${BUILD_DIR}/agent.x86_64.iso" "${ISO_DIR}/agent.x86_64.iso"
  log "ISO ready at ${ISO_DIR}/agent.x86_64.iso"
}

# ---------------------------------------------------------------------------
# create_vm  (includes DNS wiring)
# ---------------------------------------------------------------------------
create_vm() {
  configure_dns

  if virsh -c qemu:///system dominfo "$VM_NAME" >/dev/null 2>&1; then
    log "VM '${VM_NAME}' already defined — skipping create (destroy it first to recreate)."
    return 0
  fi

  # Created via the libvirt storage-pool API (not qemu-img directly): the
  # images dir is owned by the libvirtd daemon, so a raw qemu-img/rm as your
  # unprivileged user hits Permission denied. vol-create-as/vol-delete go
  # through libvirtd, which has the rights to write there.
  local vol1="${VM_NAME}-disk1.qcow2"
  local vol2="${VM_NAME}-disk2-ceph.qcow2"
  local disk1 disk2

  log "Creating disks in pool '${STORAGE_POOL}': ${vol1} (${VM_DISK1_GB}G), ${vol2} (${VM_DISK2_GB}G)..."
  virsh -c qemu:///system vol-info --pool "$STORAGE_POOL" "$vol1" >/dev/null 2>&1 \
    || virsh -c qemu:///system vol-create-as "$STORAGE_POOL" "$vol1" "${VM_DISK1_GB}G" --format qcow2
  virsh -c qemu:///system vol-info --pool "$STORAGE_POOL" "$vol2" >/dev/null 2>&1 \
    || virsh -c qemu:///system vol-create-as "$STORAGE_POOL" "$vol2" "${VM_DISK2_GB}G" --format qcow2

  disk1=$(virsh -c qemu:///system vol-path --pool "$STORAGE_POOL" "$vol1")
  disk2=$(virsh -c qemu:///system vol-path --pool "$STORAGE_POOL" "$vol2")

  local os_variant="rhel9.4"
  osinfo-query os 2>/dev/null | grep -q "rhel9.4" || os_variant="rhel9-unknown"

  log "Defining and starting VM '${VM_NAME}' (${VM_VCPUS} vCPU / ${VM_RAM_MB}MB, cpu host-passthrough, UEFI/q35)..."
  # The ISO is attached as --cdrom alongside two empty disks: coreos-installer
  # picks one, installs RHCOS onto it, and registers a UEFI boot entry
  # pointing at it, so on_reboot=restart just boots straight from disk from
  # then on — no need to detach the ISO or touch boot order ourselves.
  #
  # We skip Secure Boot/SMM/TPM (virt-install's default UEFI profile for
  # rhel9.x os-variants) simply because a lab cluster doesn't need them —
  # one less thing to go wrong. If RHCOS still hangs at "Booting `RHEL
  # CoreOS (Live)'" after this, that's the edk2-ovmf/GRUB bug in the
  # README's Troubleshooting section, not Secure Boot/TPM.
  virt-install \
    --connect qemu:///system \
    --name "$VM_NAME" \
    --vcpus "$VM_VCPUS" \
    --memory "$VM_RAM_MB" \
    --cpu host-passthrough \
    --machine q35 \
    --boot loader=/usr/share/edk2/x64/OVMF_CODE.4m.fd,loader.readonly=yes,loader.type=pflash,nvram.template=/usr/share/edk2/x64/OVMF_VARS.4m.fd \
    --tpm none \
    --os-variant "$os_variant" \
    --disk path="${disk1}",format=qcow2,bus=virtio \
    --disk path="${disk2}",format=qcow2,bus=virtio \
    --network network="${LIBVIRT_NET}",mac="${MAC_ADDRESS}",model=virtio \
    --cdrom "${ISO_DIR}/agent.x86_64.iso" \
    --events on_reboot=restart \
    --noautoconsole

  log "VM '${VM_NAME}' started. It will reboot itself several times during install; use 'virsh console ${VM_NAME}' to watch."
}

configure_dns() {
  log "Configuring DNS (host /etc/hosts + libvirt dnsmasq)..."

  # --- host /etc/hosts: api / api-int (static, not wildcard-able) ---
  if ! grep -qF "api.${CLUSTER_DOMAIN}" /etc/hosts 2>/dev/null; then
    log "Adding api/api-int entries to /etc/hosts (requires sudo)..."
    echo "${STATIC_IP} api.${CLUSTER_DOMAIN} api-int.${CLUSTER_DOMAIN} ${HOSTS_MARKER}" \
      | sudo tee -a /etc/hosts >/dev/null
  else
    log "/etc/hosts already has api.${CLUSTER_DOMAIN} — skipping."
  fi

  # --- libvirt dnsmasq: same host-records, live+persistent, no restart needed ---
  local net_xml
  net_xml=$(virsh -c qemu:///system net-dumpxml "$LIBVIRT_NET" 2>/dev/null || true)
  if ! grep -q "api.${CLUSTER_DOMAIN}" <<<"$net_xml"; then
    log "Adding api/api-int dns-host record to libvirt network '${LIBVIRT_NET}'..."
    virsh -c qemu:///system net-update "$LIBVIRT_NET" add dns-host \
      --xml "<host ip='${STATIC_IP}'><hostname>api.${CLUSTER_DOMAIN}</hostname><hostname>api-int.${CLUSTER_DOMAIN}</hostname></host>" \
      --live --config
  else
    log "libvirt dns-host record for api.${CLUSTER_DOMAIN} already present — skipping."
  fi

  # --- libvirt dnsmasq: *.apps wildcard via the dnsmasq:options XML extension.
  # This is the only way to get wildcard resolution out of libvirt's managed
  # dnsmasq; it requires a full network redefine + restart (one-time, brief
  # DHCP/DNS blip on the 'default' network — Morpheus's existing DHCP lease
  # is unaffected).
  if ! grep -q "apps.${CLUSTER_DOMAIN}" <<<"$net_xml"; then
    log "Adding *.apps.${CLUSTER_DOMAIN} wildcard to libvirt dnsmasq (network restart, brief blip)..."
    local tmp_xml="${WORKDIR}/${LIBVIRT_NET}-net.xml"
    virsh -c qemu:///system net-dumpxml "$LIBVIRT_NET" > "$tmp_xml"
    python3 - "$tmp_xml" "$CLUSTER_DOMAIN" "$STATIC_IP" <<'PYEOF'
import sys, xml.etree.ElementTree as ET

path, domain, ip = sys.argv[1], sys.argv[2], sys.argv[3]
NS = "http://libvirt.org/schemas/network/dnsmasq/1.0"
ET.register_namespace("dnsmasq", NS)

tree = ET.parse(path)
root = tree.getroot()

opts = root.find(f"{{{NS}}}options")
if opts is None:
    opts = ET.SubElement(root, f"{{{NS}}}options")

opt = ET.SubElement(opts, f"{{{NS}}}option")
opt.set("value", f"address=/apps.{domain}/{ip}")

tree.write(path, xml_declaration=False)
PYEOF
    virsh -c qemu:///system net-destroy "$LIBVIRT_NET" >/dev/null
    virsh -c qemu:///system net-define "$tmp_xml"
    virsh -c qemu:///system net-start "$LIBVIRT_NET"
  else
    log "libvirt wildcard for apps.${CLUSTER_DOMAIN} already present — skipping."
  fi

  # --- host-side resolution of the wildcard via systemd-resolved routing ---
  if command -v resolvectl >/dev/null 2>&1 && systemctl is-active --quiet systemd-resolved 2>/dev/null; then
    local vbr
    vbr=$(virsh -c qemu:///system net-info "$LIBVIRT_NET" | awk '/Bridge/{print $2}')
    if [ -n "$vbr" ]; then
      log "Routing *.${CLUSTER_DOMAIN} DNS through ${vbr} (${DNS_SERVER}) via systemd-resolved..."
      sudo resolvectl dns "$vbr" "$DNS_SERVER"
      sudo resolvectl domain "$vbr" "~${CLUSTER_DOMAIN}"
    fi
  else
    warn "systemd-resolved not active — if the host itself can't resolve *.apps.${CLUSTER_DOMAIN}, point its resolver at ${DNS_SERVER} manually."
  fi
}

# ---------------------------------------------------------------------------
# wait_install
# ---------------------------------------------------------------------------
# Watchdog for the bootstrap-complete window: after coreos-installer writes
# RHCOS to disk, the live environment is supposed to reboot straight into
# the installed OS (on_reboot=restart in the VM's libvirt config handles
# that transparently). On some hosts/timings it issues a full ACPI poweroff
# instead of a reboot, and because on_poweroff=destroy, libvirt just stops
# the domain instead of restarting it — the install silently stalls with
# the VM sitting "shut off" and openshift-install polling an API that will
# never come back.
#
# Restarting the VM after that sometimes drops it into the UEFI Interactive
# Shell rather than booting the installed disk, because RHCOS never got the
# chance to register its NVRAM boot entry before the poweroff — see "VM
# drops to the UEFI shell" in the README's Troubleshooting section. The
# manual fix there is to type, at the Shell> prompt:
#   FS0:
#   \EFI\BOOT\BOOTX64.EFI
# We replay exactly that over the console via `virsh send-key` after
# restarting: that fallback path exists on every disk coreos-installer
# writes, and if the VM actually came back up cleanly and this lands on a
# login prompt or GRUB menu instead, the keystrokes are inert there.
recover_vm_from_poweroff() {
  while true; do
    sleep 20
    local state
    state=$(virsh -c qemu:///system domstate "$VM_NAME" 2>/dev/null || echo "")
    [ "$state" = "shut off" ] || continue
    warn "VM '${VM_NAME}' powered off mid-install (known RHCOS post-write-reboot quirk) — restarting and replaying UEFI boot-recovery keystrokes..."
    virsh -c qemu:///system start "$VM_NAME" >/dev/null 2>&1 || continue
    sleep 25
    local k
    k() { virsh -c qemu:///system send-key "$VM_NAME" --codeset linux "$@" >/dev/null 2>&1; }
    k KEY_F; k KEY_S; k KEY_0; k KEY_LEFTSHIFT KEY_SEMICOLON; k KEY_ENTER
    sleep 1
    k KEY_BACKSLASH; k KEY_E; k KEY_F; k KEY_I
    k KEY_BACKSLASH; k KEY_B; k KEY_O; k KEY_O; k KEY_T
    k KEY_BACKSLASH; k KEY_B; k KEY_O; k KEY_O; k KEY_T; k KEY_X; k KEY_6; k KEY_4
    k KEY_DOT; k KEY_E; k KEY_F; k KEY_I
    k KEY_ENTER
  done
}

wait_install() {
  [ -x "${BIN_DIR}/openshift-install" ] || fail "run fetch_tools first"
  [ -d "$BUILD_DIR" ] || fail "run create_iso first"

  log "Waiting for bootstrap to complete (API reachable)..."
  recover_vm_from_poweroff &
  local recover_pid=$!
  "${BIN_DIR}/openshift-install" agent wait-for bootstrap-complete --dir="$BUILD_DIR" --log-level=info \
    | tee "${LOG_DIR}/wait-bootstrap.log"
  kill "$recover_pid" >/dev/null 2>&1 || true
  wait "$recover_pid" 2>/dev/null || true

  log "Waiting for install to complete (this can take 30-60+ min on a nested VM)..."
  "${BIN_DIR}/openshift-install" agent wait-for install-complete --dir="$BUILD_DIR" --log-level=info \
    | tee "${LOG_DIR}/wait-install.log"

  [ -f "$KUBECONFIG_PATH" ] || fail "install reported complete but kubeconfig missing at ${KUBECONFIG_PATH}"
  log "Install complete. kubeconfig: ${KUBECONFIG_PATH}"
  log "kubeadmin password: $(cat "${BUILD_DIR}/auth/kubeadmin-password" 2>/dev/null || echo '<not found>')"
}

# ---------------------------------------------------------------------------
# install_cnv_and_storage
# ---------------------------------------------------------------------------
install_cnv_and_storage() {
  [ -f "$KUBECONFIG_PATH" ] || fail "run wait_install first"
  export KUBECONFIG="$KUBECONFIG_PATH"
  local OC="${BIN_DIR}/oc"

  log "Waiting for node to be Ready..."
  "$OC" wait --for=condition=Ready node --all --timeout=600s

  install_cnv
  install_rook_ceph
  morpheus_storage_smoke_test
}

install_cnv() {
  local OC="${BIN_DIR}/oc"
  log "Installing OpenShift Virtualization (CNV)..."

  cat > "${MANIFEST_DIR}/cnv-subscription.yaml" <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: openshift-cnv
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: kubevirt-hyperconverged-group
  namespace: openshift-cnv
spec:
  targetNamespaces:
    - openshift-cnv
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: hco-operatorhub
  namespace: openshift-cnv
spec:
  channel: stable
  name: kubevirt-hyperconverged
  source: redhat-operators
  sourceNamespace: openshift-marketplace
  installPlanApproval: Automatic
EOF
  "$OC" apply -f "${MANIFEST_DIR}/cnv-subscription.yaml"

  # 15m was too tight on a cold pull: the HCO operator subscription pulls
  # several sizeable images in sequence (hco -> cdi/cnao/ssp/virt-operator
  # -> their own dependents), which routinely runs past 15m the first time
  # on a nested VM even though nothing is actually stuck. 25m gives that
  # room without masking a genuinely wedged install.
  log "Waiting for OpenShift Virtualization CSV to reach Succeeded (up to 25m)..."
  local deadline=$((SECONDS + 1500))
  while true; do
    local phase
    phase=$("$OC" get csv -n openshift-cnv \
      -o jsonpath='{.items[?(@.spec.displayName=="OpenShift Virtualization")].status.phase}' 2>/dev/null || true)
    [ "$phase" = "Succeeded" ] && break
    [ "$SECONDS" -ge "$deadline" ] && fail "CNV CSV never reached Succeeded — check 'oc get csv -n openshift-cnv'"
    sleep 15
  done

  cat > "${MANIFEST_DIR}/hyperconverged.yaml" <<'EOF'
apiVersion: hco.kubevirt.io/v1beta1
kind: HyperConverged
metadata:
  name: kubevirt-hyperconverged
  namespace: openshift-cnv
spec: {}
EOF
  "$OC" apply -f "${MANIFEST_DIR}/hyperconverged.yaml"

  log "Waiting for HyperConverged to become Available (up to 15m)..."
  "$OC" wait hyperconverged kubevirt-hyperconverged -n openshift-cnv \
    --for=condition=Available --timeout=900s
  log "OpenShift Virtualization is up."
}

install_rook_ceph() {
  local OC="${BIN_DIR}/oc"
  local NAMESPACE="rook-ceph"
  local RAW_BASE="https://raw.githubusercontent.com/rook/rook/${ROOK_VERSION}/deploy/examples"

  log "Fetching Rook-Ceph ${ROOK_VERSION} operator manifests..."
  curl -fsSL "${RAW_BASE}/crds.yaml"   -o "${MANIFEST_DIR}/rook-crds.yaml"
  curl -fsSL "${RAW_BASE}/common.yaml" -o "${MANIFEST_DIR}/rook-common.yaml"
  curl -fsSL "${RAW_BASE}/operator-openshift.yaml" -o "${MANIFEST_DIR}/rook-operator.yaml" \
    || curl -fsSL "${RAW_BASE}/operator.yaml" -o "${MANIFEST_DIR}/rook-operator.yaml"

  # Lightweight footprint: Morpheus only needs RBD (Block RWX), not CephFS/NFS.
  sed -i \
    -e 's/ROOK_CSI_ENABLE_CEPHFS: "true"/ROOK_CSI_ENABLE_CEPHFS: "false"/' \
    -e 's/CSI_ENABLE_CEPHFS_SNAPSHOTTER: "true"/CSI_ENABLE_CEPHFS_SNAPSHOTTER: "false"/' \
    "${MANIFEST_DIR}/rook-operator.yaml" || true

  log "Applying CRDs, common RBAC, operator..."
  "$OC" apply -f "${MANIFEST_DIR}/rook-crds.yaml"
  "$OC" apply -f "${MANIFEST_DIR}/rook-common.yaml"
  "$OC" apply -f "${MANIFEST_DIR}/rook-operator.yaml"
  "$OC" -n "$NAMESPACE" wait --for=condition=Available deploy/rook-ceph-operator --timeout=300s

  log "Granting 'privileged' SCC to Rook/Ceph service accounts..."
  for sa in rook-ceph-system rook-ceph-osd rook-ceph-mgr rook-ceph-cmd-reporter \
            rook-csi-rbd-plugin-sa rook-csi-rbd-provisioner-sa default; do
    "$OC" adm policy add-scc-to-user privileged "system:serviceaccount:${NAMESPACE}:${sa}" >/dev/null 2>&1 || true
  done

  local node_name
  node_name=$("$OC" get nodes -o jsonpath='{.items[0].metadata.name}')

  log "Applying CephCluster (node=${node_name}, device=/dev/${CEPH_DEVICE}, size=1)..."
  cat > "${MANIFEST_DIR}/cephcluster.yaml" <<EOF
apiVersion: ceph.rook.io/v1
kind: CephCluster
metadata:
  name: rook-ceph
  namespace: ${NAMESPACE}
spec:
  cephVersion:
    image: ${CEPH_IMAGE}
    allowUnsupported: false
  dataDirHostPath: /var/lib/rook
  skipUpgradeChecks: true
  continueUpgradeAfterChecksEvenIfNotHealthy: true
  mon:
    count: 1
    allowMultiplePerNode: true
  mgr:
    count: 1
    allowMultiplePerNode: true
    modules:
      - name: pg_autoscaler
        enabled: true
  dashboard:
    enabled: true
    ssl: false
  monitoring:
    enabled: false
  crashCollector:
    disable: true
  logCollector:
    enabled: false
  disruptionManagement:
    managePodBudgets: false
  network:
    connections:
      encryption:
        enabled: false
      compression:
        enabled: false
  cleanupPolicy:
    confirmation: ""
  placement:
    all:
      tolerations:
        - key: node-role.kubernetes.io/master
          operator: Exists
          effect: NoSchedule
        - key: node-role.kubernetes.io/control-plane
          operator: Exists
          effect: NoSchedule
  resources:
    mon:
      requests: { cpu: "250m", memory: "512Mi" }
    mgr:
      requests: { cpu: "250m", memory: "512Mi" }
    osd:
      requests: { cpu: "250m", memory: "1Gi" }
  storage:
    useAllNodes: false
    useAllDevices: false
    nodes:
      - name: "${node_name}"
        devices:
          - name: "${CEPH_DEVICE}"
EOF
  "$OC" apply -f "${MANIFEST_DIR}/cephcluster.yaml"

  log "Waiting for CephCluster to report Ready + HEALTH_OK/HEALTH_WARN (up to 20m)..."
  local deadline=$((SECONDS + 1200))
  while true; do
    local phase health
    phase=$("$OC" -n "$NAMESPACE" get cephcluster rook-ceph -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    health=$("$OC" -n "$NAMESPACE" get cephcluster rook-ceph -o jsonpath='{.status.ceph.health}' 2>/dev/null || echo "")
    echo "  phase=${phase:-<none>} health=${health:-<none>}"
    if [ "$phase" = "Ready" ] && { [ "$health" = "HEALTH_OK" ] || [ "$health" = "HEALTH_WARN" ]; }; then
      log "CephCluster Ready (${health})."
      break
    fi
    if [ "$SECONDS" -ge "$deadline" ]; then
      "$OC" -n "$NAMESPACE" get pods -o wide
      fail "CephCluster did not reach Ready/HEALTH_OK|HEALTH_WARN in time"
    fi
    sleep 15
  done

  log "Unsetting any pre-existing default StorageClass..."
  local existing_default
  existing_default=$("$OC" get sc -o jsonpath='{range .items[?(@.metadata.annotations.storageclass\.kubernetes\.io/is-default-class=="true")]}{.metadata.name}{" "}{end}' 2>/dev/null || echo "")
  for sc in $existing_default; do
    "$OC" patch storageclass "$sc" -p '{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"false"}}}'
  done

  cat > "${MANIFEST_DIR}/pool-storageclass.yaml" <<EOF
apiVersion: ceph.rook.io/v1
kind: CephBlockPool
metadata:
  name: replicapool
  namespace: ${NAMESPACE}
spec:
  failureDomain: osd
  replicated:
    size: 1
    requireSafeReplicaSize: false
---
# RWX for volumeMode: Block is a first-class ceph-csi RBD capability
# (MULTI_NODE_MULTI_WRITER) — this satisfies Morpheus's PVC spec without
# needing CephFS. fstype only applies to Filesystem-mode PVCs.
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: rook-ceph-block
  annotations:
    storageclass.kubernetes.io/is-default-class: "true"
provisioner: rook-ceph.rbd.csi.ceph.com
parameters:
  clusterID: ${NAMESPACE}
  pool: replicapool
  imageFormat: "2"
  imageFeatures: layering
  csi.storage.k8s.io/fstype: ext4
  csi.storage.k8s.io/provisioner-secret-name: rook-csi-rbd-provisioner
  csi.storage.k8s.io/provisioner-secret-namespace: ${NAMESPACE}
  csi.storage.k8s.io/controller-expand-secret-name: rook-csi-rbd-provisioner
  csi.storage.k8s.io/controller-expand-secret-namespace: ${NAMESPACE}
  csi.storage.k8s.io/node-stage-secret-name: rook-csi-rbd-node
  csi.storage.k8s.io/node-stage-secret-namespace: ${NAMESPACE}
allowVolumeExpansion: true
reclaimPolicy: Delete
volumeBindingMode: Immediate
EOF
  "$OC" apply -f "${MANIFEST_DIR}/pool-storageclass.yaml"

  log "Waiting for CephBlockPool to be Ready..."
  local deadline2=$((SECONDS + 300))
  while true; do
    local pool_phase
    pool_phase=$("$OC" -n "$NAMESPACE" get cephblockpool replicapool -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    [ "$pool_phase" = "Ready" ] && break
    [ "$SECONDS" -ge "$deadline2" ] && fail "CephBlockPool never became Ready"
    sleep 10
  done
  log "Storage layer ready: StorageClass 'rook-ceph-block' is default and RWX+Block capable."
}

morpheus_storage_smoke_test() {
  local OC="${BIN_DIR}/oc"
  log "Running RWX+Block smoke test PVC..."

  cat > "${MANIFEST_DIR}/test-pvc.yaml" <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: rbd-rwx-block-test
  namespace: default
spec:
  accessModes:
    - ReadWriteMany
  volumeMode: Block
  storageClassName: rook-ceph-block
  resources:
    requests:
      storage: 5Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: rbd-rwx-block-test-pod
  namespace: default
spec:
  restartPolicy: Never
  containers:
    - name: block-test
      image: registry.access.redhat.com/ubi9/ubi-minimal:latest
      command: ["sh", "-c", "dd if=/dev/zero of=/dev/block-test bs=1M count=10 && echo BLOCK_WRITE_OK && sleep 3600"]
      volumeDevices:
        - name: data
          devicePath: /dev/block-test
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: rbd-rwx-block-test
EOF
  "$OC" apply -f "${MANIFEST_DIR}/test-pvc.yaml"
  # 120s was too tight the first time the CSI driver deploys: the
  # rook-ceph-csi-detect-version job has to complete before the actual
  # csi-rbdplugin-provisioner/nodeplugin pods even get created, and pulling
  # those images fresh routinely takes a few minutes on a nested VM — no
  # provisioner running means the PVC just sits Pending until it does.
  "$OC" -n default wait --for=jsonpath='{.status.phase}'=Bound pvc/rbd-rwx-block-test --timeout=300s
  "$OC" -n default wait --for=condition=Ready pod/rbd-rwx-block-test-pod --timeout=300s || true
  sleep 5
  "$OC" -n default logs pod/rbd-rwx-block-test-pod | grep -q BLOCK_WRITE_OK \
    && log "SUCCESS: RWX+Block PVC bound and writable." \
    || fail "Block write test failed — check 'oc -n default logs rbd-rwx-block-test-pod'"
  "$OC" delete -f "${MANIFEST_DIR}/test-pvc.yaml" --ignore-not-found
}

# ---------------------------------------------------------------------------
# morpheus_sa
# ---------------------------------------------------------------------------
morpheus_sa() {
  [ -f "$KUBECONFIG_PATH" ] || fail "run wait_install first"
  export KUBECONFIG="$KUBECONFIG_PATH"
  local OC="${BIN_DIR}/oc"

  log "Creating 'morpheus' ServiceAccount with cluster-admin..."
  "$OC" create sa morpheus -n default --dry-run=client -o yaml | "$OC" apply -f -
  "$OC" create clusterrolebinding morpheus-cluster-admin \
    --clusterrole=cluster-admin --serviceaccount=default:morpheus \
    --dry-run=client -o yaml | "$OC" apply -f -

  local token_file="${WORKDIR}/morpheus-sa-token.txt"
  "$OC" create token morpheus -n default --duration=8760h > "$token_file"
  chmod 600 "$token_file"
  local token
  token=$(cat "$token_file")

  print_summary "$token" "$token_file"
}

print_summary() {
  local token="$1" token_file="$2"
  local console_url="https://console-openshift-console.apps.${CLUSTER_DOMAIN}"
  local api_url="https://api.${CLUSTER_DOMAIN}:6443"
  local kubeadmin_pw="<not found>"
  [ -f "${BUILD_DIR}/auth/kubeadmin-password" ] && kubeadmin_pw=$(cat "${BUILD_DIR}/auth/kubeadmin-password")

  local morpheus_ip=""
  morpheus_ip=$(virsh -c qemu:///system list --all --name 2>/dev/null \
    | grep -i morpheus | head -1 \
    | xargs -r -I{} virsh -c qemu:///system domifaddr {} 2>/dev/null \
    | awk '/ipv4/{print $4}' | cut -d/ -f1 || true)

  cat <<EOF

================================================================================
  SNO + OpenShift Virtualization + Rook-Ceph — deployment summary
================================================================================
  Console URL:      ${console_url}
  kubeadmin user:    kubeadmin
  kubeadmin pass:    ${kubeadmin_pw}

  API URL:           ${api_url}

  Morpheus ServiceAccount token (cluster-admin, valid 8760h):
    Saved to:        ${token_file}  (chmod 600)
    Preview:         ${token:0:12}...${token: -8}

  Morpheus appliance VM: ${morpheus_ip:-<not auto-detected — check its DHCP lease>}

--------------------------------------------------------------------------------
  Add this cluster in Morpheus:
    1. Log in to the Morpheus appliance UI.
    2. Navigate to: Infrastructure > Clusters > + Add Cluster
    3. Select cluster type: RED HAT OPENSHIFT CLUSTER
    4. API URL:        ${api_url}
    5. Bearer Token:    paste contents of ${token_file}
    6. Default StorageClass to select: rook-ceph-block (RWX + Block capable)
    7. Save, then confirm the cluster shows "Ready"/"Healthy" before
       provisioning any VM workload.
================================================================================
EOF
}

# ---------------------------------------------------------------------------
# destroy
# ---------------------------------------------------------------------------
destroy() {
  if [ "${FORCE:-0}" != "1" ]; then
    read -r -p "This will destroy VM '${VM_NAME}' and its disks. Continue? [y/N] " ans
    [[ "$ans" =~ ^[Yy]$ ]] || { log "Aborted."; return 0; }
  fi

  if virsh -c qemu:///system dominfo "$VM_NAME" >/dev/null 2>&1; then
    log "Destroying VM '${VM_NAME}'..."
    virsh -c qemu:///system destroy "$VM_NAME" >/dev/null 2>&1 || true
    virsh -c qemu:///system undefine "$VM_NAME" --nvram >/dev/null 2>&1 \
      || virsh -c qemu:///system undefine "$VM_NAME" >/dev/null 2>&1 || true
  else
    log "VM '${VM_NAME}' not defined — skipping."
  fi

  # Same reasoning as create_vm: these are libvirtd-owned volumes, delete
  # through the storage-pool API rather than rm -f (which would hit
  # Permission denied for an unprivileged user).
  virsh -c qemu:///system vol-delete --pool "$STORAGE_POOL" "${VM_NAME}-disk1.qcow2" >/dev/null 2>&1 || true
  virsh -c qemu:///system vol-delete --pool "$STORAGE_POOL" "${VM_NAME}-disk2-ceph.qcow2" >/dev/null 2>&1 || true
  rm -f "${ISO_DIR}/agent.x86_64.iso"

  log "Removing DNS entries added by this script (best effort)..."
  sudo sed -i "/${HOSTS_MARKER//\//\\/}/d" /etc/hosts 2>/dev/null || true
  virsh -c qemu:///system net-update "$LIBVIRT_NET" delete dns-host \
    --xml "<host ip='${STATIC_IP}'><hostname>api.${CLUSTER_DOMAIN}</hostname><hostname>api-int.${CLUSTER_DOMAIN}</hostname></host>" \
    --live --config >/dev/null 2>&1 || true

  log "Destroy complete. ${WORKDIR} (installer assets/logs/tokens) left in place — remove manually if you want a full purge."
}

# ---------------------------------------------------------------------------
# all
# ---------------------------------------------------------------------------
all() {
  preflight
  fetch_tools
  render_configs
  create_iso
  create_vm
  wait_install
  install_cnv_and_storage
  morpheus_sa
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------
usage() {
  cat <<EOF
Usage: $0 <phase>

Phases:
  preflight                Check host deps, resources, nested KVM, pull secret, SSH key
  fetch_tools              Download openshift-install + oc into ${BIN_DIR}
  render_configs           Generate install-config.yaml / agent-config.yaml
  create_iso               Build the agent-based installer ISO
  create_vm                Configure DNS, define + boot the libvirt VM
  wait_install             Block until the SNO install completes
  install_cnv_and_storage  Install CNV + single-node Rook-Ceph + RWX/Block smoke test
  morpheus_sa              Create Morpheus SA/token and print onboarding summary
  destroy                  Tear down the VM, disks, and DNS entries (asks to confirm)
  all                      Run every phase above in order — start here
EOF
}

main() {
  mkdir -p "$WORKDIR" "$LOG_DIR"
  local phase="${1:-}"
  [ -n "$phase" ] || { usage; exit 1; }

  local log_file="${LOG_DIR}/${phase}-$(date +%Y%m%d-%H%M%S).log"
  exec > >(tee -a "$log_file") 2>&1

  case "$phase" in
    preflight|fetch_tools|render_configs|create_iso|create_vm|wait_install| \
    install_cnv_and_storage|morpheus_sa|destroy|all)
      "$phase" ;;
    *) usage; exit 1 ;;
  esac
}

main "$@"
