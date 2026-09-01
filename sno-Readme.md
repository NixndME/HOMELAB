# rook-ceph-sno-morpheus

Spin up a **Single Node OpenShift (SNO)** cluster on local libvirt/KVM, layer
on **OpenShift Virtualization (CNV)** and a single-node **Rook-Ceph**, and
walk away with RWX+Block storage and a ready-to-paste Morpheus onboarding
summary — API URL, bearer token, and the StorageClass to pick.

Built for a home-lab / dev box: one VM, one node wearing the control-plane
and worker hat at once, no real hardware required beyond a machine that can
run nested KVM.

## What you get

```
┌─────────────────────────────────────────────────────────┐
│  libvirt VM "sno"  (14 vCPU / 24GB RAM / UEFI / q35)    │
│                                                         │
│   OpenShift SNO  (control-plane + worker, one node)     │
│     ├─ OpenShift Virtualization (CNV / KubeVirt)        │
│     └─ Rook-Ceph (1 mon, 1 mgr, 1 osd)                  │
│          └─ StorageClass "rook-ceph-block"              │
│             RBD, RWX + Block, default                   │
└─────────────────────────────────────────────────────────┘
                        ▲
                        │  API + bearer token
                        │
                 ┌──────┴──────┐
                 │  Morpheus   │
                 └─────────────┘
```

One script, `deploy-sno.sh`, does the whole thing end to end — run `./deploy-sno.sh all` and it builds the VM, installs OpenShift, installs CNV, installs Rook-Ceph, and prints your Morpheus connection details.

## Prerequisites

This was built and tested on Arch Linux; adjust package names for your distro.

- `libvirtd` running, and your user in the `libvirt` group
- A libvirt storage pool (default works: `virsh pool-define-as default dir --target /var/lib/libvirt/images && virsh pool-autostart default`)
- Nested virtualization enabled (`kvm_intel`/`kvm_amd` module param `nested=1`) — CNV needs to run KVM *inside* the VM
- **`edk2-ovmf` version 202411 or earlier.** Versions 202505 and newer hit a
  known upstream GRUB/EDK2 incompatibility that hangs RHCOS boot right after
  the kernel line prints. See [Troubleshooting](#troubleshooting) if you hit this.
- `nmstatectl` (AUR-only on Arch — `yay -S nmstate`). The agent installer
  shells out to it to validate the network config; there's no way around it.
- `virsh`, `virt-install`, `qemu-img`, `curl`, `tar`, `jq`, `openssl`, `python3`, `ssh-keygen`
- ~24GB free RAM (VM RAM + host headroom) and ~250GB free disk for the VM's two disks
- An OpenShift pull secret at `./pull-secret.json` or `~/.openshift/pull-secret.json`
  (grab one free from [console.redhat.com](https://console.redhat.com/openshift/install/pull-secret))
- An SSH keypair — `deploy-sno.sh` generates one at `~/.ssh/id_ed25519` for you if none exists

`./deploy-sno.sh preflight` checks all of this and tells you exactly what's missing.

## Quick start

```bash
git clone <this-repo>
cd rook-ceph-sno-morpheus
cp /path/to/your/pull-secret.json .

./deploy-sno.sh all
```

That runs every phase in order. Expect it to take **45–90 minutes** — most of
it is the OpenShift install itself (bootstrap, control-plane formation,
cluster operators settling), which is inherently slow on a single nested VM.

When it finishes, you'll see a summary with the console URL, kubeadmin
credentials, and a Morpheus ServiceAccount token — see
[Adding the cluster to Morpheus](#adding-the-cluster-to-morpheus).

### Running phases individually

Useful while debugging, or if something fails partway through — every phase
except `destroy` is safe to re-run on its own:

```bash
./deploy-sno.sh preflight               # sanity-check your host
./deploy-sno.sh fetch_tools             # download openshift-install + oc
./deploy-sno.sh render_configs          # generate install-config.yaml / agent-config.yaml
./deploy-sno.sh create_iso              # build the agent-based installer ISO
./deploy-sno.sh create_vm               # wire up DNS, define + boot the VM
./deploy-sno.sh wait_install            # block until the SNO install completes
./deploy-sno.sh install_cnv_and_storage # CNV + Rook-Ceph + RWX/Block smoke test
./deploy-sno.sh morpheus_sa             # create the Morpheus SA/token, print summary
./deploy-sno.sh destroy                 # tear down the VM, disks, DNS entries
```

## Configuration

Everything's overridable via environment variables — defaults are tuned for
a comfortable dev-box footprint:

| Variable | Default | What it is |
|---|---|---|
| `CLUSTER_NAME` | `sno` | Cluster name (also the VM name) |
| `BASE_DOMAIN` | `lab.local` | Base domain — cluster lives at `<CLUSTER_NAME>.<BASE_DOMAIN>` |
| `VM_VCPUS` | `14` | vCPUs given to the VM |
| `VM_RAM_MB` | `24576` | RAM given to the VM (24GB) |
| `VM_DISK1_GB` / `VM_DISK2_GB` | `130` / `100` | The VM's two disks — RHCOS claims one, Ceph claims the other |
| `STATIC_IP` | `192.168.122.50` | Rendezvous/API IP on the libvirt `default` network |
| `CEPH_DEVICE` | `vda` | Which guest disk Ceph claims — see the naming caveat below |
| `OCP_CHANNEL` | `stable-4.21` | openshift-install/oc release channel |
| `ROOK_VERSION` | `v1.15.4` | Rook-Ceph operator version |

Example: `VM_RAM_MB=16384 ./deploy-sno.sh all` for a lighter footprint.

## Adding the cluster to Morpheus

`morpheus_sa` (part of `all`) creates a cluster-admin ServiceAccount named
`morpheus` and prints everything you need:

1. Log in to the Morpheus appliance UI.
2. **Infrastructure → Clusters → + Add Cluster**
3. Cluster type: **Red Hat OpenShift Cluster**
4. API URL: `https://api.<cluster>.<domain>:6443`
5. Bearer token: paste the contents of `sno-deploy/morpheus-sa-token.txt`
6. Default StorageClass: **`rook-ceph-block`** (RWX + Block capable)
7. Save, then confirm the cluster shows **Ready/Healthy** before deploying
   any VM workload through it.

## Troubleshooting

### RHCOS boot hangs forever at "Booting `RHEL CoreOS (Live)`"

This is the big one. If the VM console freezes right after GRUB prints the
boot entry — screen static, qemu pinned at ~100% CPU, node never comes up on
the network — it's a **known upstream `edk2-ovmf` 202505+ incompatibility
with GRUB**, not a problem with this script or your VM config. It's
independent of Secure Boot, TPM, CPU model, video device, KVM vs TCG, vCPU
count, or network/disk config — we verified all of those while chasing it
down.

**Fix:** downgrade `edk2-ovmf` to `202411-1`:

```bash
curl -LO https://archive.archlinux.org/packages/e/edk2-ovmf/edk2-ovmf-202411-1-any.pkg.tar.zst
sudo pacman -U edk2-ovmf-202411-1-any.pkg.tar.zst
```

(Leave `qemu-full` at whatever version pacman gives you — the bug is in
EDK2/GRUB, not qemu.)

### Storage shows "no storage configured" / CephCluster has 0 OSDs

Guest-visible virtio-blk disk naming (`vda`/`vdb`) isn't strictly guaranteed
to match the order the disks were attached in — which disk the kernel calls
`vda` depends on PCI enumeration order, not disk-definition order. If
`coreos-installer` happens to install RHCOS onto the disk this script
expected to be empty for Ceph, you'll get a healthy cluster with **zero
OSDs** and no StorageClass options in the UI.

Check which device is actually empty:

```bash
export KUBECONFIG=sno-deploy/ocp-install/build/auth/kubeconfig
oc logs -n rook-ceph $(oc get pods -n rook-ceph -o name | grep osd-prepare) \
  | grep -E "is available|skipping device"
```

That shows exactly which device Ceph found free ("is available") versus
which ones it skipped and why (already has a filesystem, is mounted, etc).

Then re-run with the corrected device:

```bash
CEPH_DEVICE=vdb ./deploy-sno.sh install_cnv_and_storage
```

(If Ceph was already applied with the wrong device, patch the live
CephCluster instead of re-running the whole phase:
`oc patch cephcluster rook-ceph -n rook-ceph --type=json -p='[{"op":"replace","path":"/spec/storage/nodes/0/devices/0/name","value":"vdb"}]'`)

### VM drops to the UEFI shell instead of booting

Usually means the VM was killed uncleanly mid-install (power loss, a manual
`virsh destroy`, host suspend) — or, more commonly, RHCOS issued a full
ACPI poweroff instead of a reboot right after `coreos-installer` finished
writing the disk. Because the VM's `on_poweroff` policy is `destroy`,
libvirt stops the domain instead of restarting it, and it comes back up
without a registered UEFI boot entry.

`wait_install` now watches for exactly this during the bootstrap-complete
wait: if the VM goes to `shut off` mid-install, it restarts the VM and
replays the recovery keystrokes below over the console automatically — you
shouldn't need to do anything. If you ever do hit it by hand (e.g. running
phases individually and the VM sits `shut off` with no wait loop watching
it), at the `Shell>` prompt:

```
FS0:
\EFI\BOOT\BOOTX64.EFI
```

That boots the installed OS directly; it'll register the boot entry
properly on its own from there and won't need this again.

### `nmstatectl not found`

It's AUR-only on Arch, not in core/extra/multilib: `yay -S nmstate` (or
`paru -S nmstate`).

### `OCP_CHANNEL` 404s

`stable-4.21` may not exist yet on `mirror.openshift.com` depending on
timing — override with an available channel/version, e.g.
`OCP_CHANNEL=stable-4.20 ./deploy-sno.sh fetch_tools`.

## Cleaning up

```bash
./deploy-sno.sh destroy   # asks to confirm; FORCE=1 skips the prompt
```

Tears down the VM, its disks, and the DNS entries this script added. The
`sno-deploy/` directory (downloaded binaries, install logs, kubeconfig,
tokens) is left in place afterward — remove it manually for a full purge.

## Repo layout

```
deploy-sno.sh     The whole pipeline: VM -> OpenShift -> CNV -> Rook-Ceph -> Morpheus summary
sno-deploy/       Generated at runtime — binaries, ISO, logs, kubeconfig, tokens.
                  Gitignored; never commit it (it holds live cluster credentials).
```
