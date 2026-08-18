#!/usr/bin/env bash
# HVM network toolkit: diagnose (pre-fix capture) or verify (post-fix check).
# Read-only, no configuration changes. Customer-agnostic — auto-detects where
# possible, all variables below are optional overrides. Run with sudo, per host.
#
# Usage:
#   sudo ./hvm-toolkit.sh diagnose
#   sudo ./hvm-toolkit.sh verify
#   sudo ./hvm-toolkit.sh            (interactive menu)

set -uo pipefail

MORPHEUS_MANAGER_IP=""     # optional, auto-detected from an active outbound :443 connection if blank
PEER_HOST_IPS=""           # optional, auto-detected from /etc/corosync/corosync.conf if blank
NFS_SERVER_IP=""           # optional, auto-detected from existing NFS mounts if blank
VLAN_EXPECT=""             # optional, space separated vlan ids to check for. blank = discover and report all tagged ports found
BOND_CHECKS=""             # optional, "bond:expected_bridge" pairs, space separated. blank = discover all bonds, report actual state
STORAGE_IF=""              # optional, auto-detected if exactly one candidate (has IP, outside OVS, not default route) is found

HOSTNAME_TAG=$(hostname)
TS=$(date +%Y%m%d-%H%M%S)

check_port() {
  timeout 3 bash -c "echo > /dev/tcp/$1/$2" 2>/dev/null
}

run_cmd() {
  local desc="$1"; shift
  echo -e "\n--- $desc ---" >> "$OUT"
  if command -v "$1" &>/dev/null; then
    "$@" >> "$OUT" 2>&1
  else
    echo "[skipped: $1 not found]" >> "$OUT"
  fi
}

section() { echo -e "\n===== $1 =====" >> "$OUT"; }

run_diagnose() {
  OUT="hvm-report-${HOSTNAME_TAG}-${TS}.txt"
  echo "HVM diagnostic report — $HOSTNAME_TAG — $(date)" > "$OUT"

  section "HOST BASICS"
  run_cmd "hostname" hostname -f
  run_cmd "os-release" cat /etc/os-release
  run_cmd "uptime" uptime
  run_cmd "kernel" uname -a

  section "NETWORK INTERFACES"
  run_cmd "ip addr" ip -d addr show
  run_cmd "ip link brief" ip -br link show
  run_cmd "bonding" bash -c 'shopt -s nullglob; for b in /proc/net/bonding/*; do echo "== $b =="; cat "$b"; done'

  section "ROUTING"
  run_cmd "ipv4 routes" ip route show
  run_cmd "ipv6 routes" ip -6 route show
  run_cmd "arp" ip neigh show

  section "NETPLAN"
  run_cmd "yaml files" bash -c 'shopt -s nullglob; for f in /etc/netplan/*.yaml /etc/netplan/*.yml; do echo "== $f =="; cat "$f"; done'
  run_cmd "netplan status" netplan status

  section "OVS"
  run_cmd "ovs-vsctl show" ovs-vsctl show
  run_cmd "list-br" ovs-vsctl list-br
  run_cmd "ports per bridge" bash -c 'for br in $(ovs-vsctl list-br 2>/dev/null); do echo "== $br =="; ovs-vsctl list-ports "$br"; done'
  run_cmd "bond-to-bridge + lacp" bash -c 'for b in $(ip -br link show type bond 2>/dev/null | awk "{print \$1}"); do echo "== $b =="; ovs-vsctl port-to-br "$b" 2>&1; ovs-appctl bond/show "$b" 2>&1; done'

  section "VLANS"
  if [[ -n "$VLAN_EXPECT" ]]; then
    for v in $VLAN_EXPECT; do
      echo -e "\n--- $v ---" >> "$OUT"
      ip -d link show | grep -B2 "vlan.*id $v" >> "$OUT" 2>&1
      command -v ovs-vsctl &>/dev/null && ovs-vsctl --columns=name,tag find Port tag="$v" >> "$OUT" 2>&1
    done
  else
    echo -e "\n--- all tagged OVS ports ---" >> "$OUT"
    command -v ovs-vsctl &>/dev/null && ovs-vsctl --format=csv --no-headings --columns=name,tag list Port 2>/dev/null | awk -F, '$2!="" && $2!="[]"' >> "$OUT"
    echo -e "\n--- all 802.1q sub-interfaces ---" >> "$OUT"
    ip -d link show | grep -B2 "vlan" >> "$OUT" 2>&1
  fi

  section "NFS MOUNTS"
  run_cmd "findmnt" bash -c 'findmnt -t nfs,nfs4 -o SOURCE,TARGET,FSTYPE,OPTIONS 2>&1'
  run_cmd "fstab" bash -c 'grep -E "\snfs4?\s" /etc/fstab 2>/dev/null || echo "none"'
  if [[ -z "$NFS_SERVER_IP" ]]; then
    AUTO_NFS=$(findmnt -t nfs,nfs4 -no SOURCE 2>/dev/null | head -1 | cut -d: -f1)
    [[ -n "$AUTO_NFS" ]] && NFS_SERVER_IP="$AUTO_NFS" && echo -e "\n[auto NFS_SERVER_IP: $NFS_SERVER_IP]" >> "$OUT"
  fi

  section "HPE-VM"
  run_cmd "hpe-vm" bash -c 'command -v hpe-vm && echo present || echo "not found"'

  section "FIREWALL"
  run_cmd "ufw" ufw status verbose
  run_cmd "iptables" iptables -L -n -v
  run_cmd "nftables" nft list ruleset

  section "DNS"
  run_cmd "resolv.conf" cat /etc/resolv.conf
  run_cmd "hosts" cat /etc/hosts

  section "REACHABILITY"
  if [[ -z "$PEER_HOST_IPS" && -f /etc/corosync/corosync.conf ]]; then
    MY_IPS=$(hostname -I 2>/dev/null)
    NODES=$(grep -oE 'ring0_addr:\s*[0-9.]+' /etc/corosync/corosync.conf 2>/dev/null | awk '{print $2}')
    FOUND=""
    for n in $NODES; do echo "$MY_IPS" | grep -qw "$n" || FOUND="$FOUND $n"; done
    PEER_HOST_IPS=$(echo "$FOUND" | xargs)
    [[ -n "$PEER_HOST_IPS" ]] && echo -e "\n[auto-detected peers from corosync.conf: $PEER_HOST_IPS]" >> "$OUT"
  fi

  if [[ -z "$MORPHEUS_MANAGER_IP" ]]; then
    MORPHEUS_MANAGER_IP=$(ss -tn state established '( dport = :443 )' 2>/dev/null | grep -oE '[0-9]{1,3}(\.[0-9]{1,3}){3}:443' | cut -d: -f1 | head -1)
    if [[ -n "$MORPHEUS_MANAGER_IP" ]]; then
      echo -e "\n[auto-detected manager from active :443 connection: $MORPHEUS_MANAGER_IP]" >> "$OUT"
    else
      DEF_IF=$(ip route show default 2>/dev/null | awk '{print $5}' | head -1)
      GW_IP=$(ip route show default 2>/dev/null | awk '{print $3}' | head -1)
      if [[ -n "$DEF_IF" ]]; then
        NEIGHS=$(ip neigh show dev "$DEF_IF" 2>/dev/null | awk '$NF ~ /REACHABLE|STALE|DELAY|PERMANENT/ {print $1}')
        MGR_CANDIDATES=""
        for n in $NEIGHS; do
          [[ "$n" == "$GW_IP" ]] && continue
          echo "$PEER_HOST_IPS" | grep -qw "$n" && continue
          MGR_CANDIDATES="$MGR_CANDIDATES $n"
        done
        MGR_CANDIDATES=$(echo "$MGR_CANDIDATES" | xargs)
        if [[ -n "$MGR_CANDIDATES" ]]; then
          echo -e "\n[manager candidates via ARP on $DEF_IF, excluding gateway $GW_IP and known peers: $MGR_CANDIDATES]" >> "$OUT"
          echo "[not auto-selected -- ARP presence alone isn't specific enough, confirm which one is actually the manager and set MORPHEUS_MANAGER_IP]" >> "$OUT"
        fi
      fi
    fi
  fi
  if [[ -n "$MORPHEUS_MANAGER_IP" ]]; then
    echo -e "\n--- manager $MORPHEUS_MANAGER_IP ---" >> "$OUT"
    for p in 22:ssh 443:agent 7443:console; do
      port="${p%%:*}"; label="${p##*:}"
      if check_port "$MORPHEUS_MANAGER_IP" "$port"; then
        echo "  [OK]   $MORPHEUS_MANAGER_IP:$port ($label)" >> "$OUT"
      else
        echo "  [FAIL] $MORPHEUS_MANAGER_IP:$port ($label)" >> "$OUT"
      fi
    done
  else
    echo -e "\n[no manager IP set or detected -- see ARP candidates above if listed]" >> "$OUT"
  fi

  if [[ -n "$PEER_HOST_IPS" ]]; then
    echo -e "\n--- peers ---" >> "$OUT"
    for peer in $PEER_HOST_IPS; do
      echo " $peer" >> "$OUT"
      for p in 3300:ceph-mon 6789:ceph-mon-legacy 2224:pcsd 3121:pacemaker-remote 9929:qdevice 21064:dlm; do
        port="${p%%:*}"; label="${p##*:}"
        if check_port "$peer" "$port"; then
          echo "  [OK]   $peer:$port ($label)" >> "$OUT"
        else
          echo "  [FAIL] $peer:$port ($label)" >> "$OUT"
        fi
      done
    done
  else
    echo -e "\n[no peer hosts set or detected]" >> "$OUT"
  fi

  if [[ -n "$NFS_SERVER_IP" ]]; then
    echo -e "\n--- nfs $NFS_SERVER_IP ---" >> "$OUT"
    echo "route used:" >> "$OUT"
    ip route get "$NFS_SERVER_IP" >> "$OUT" 2>&1
    if ping -c 2 -W 2 "$NFS_SERVER_IP" &>/dev/null; then
      echo "  [OK]   $NFS_SERVER_IP ping" >> "$OUT"
    else
      echo "  [FAIL] $NFS_SERVER_IP ping" >> "$OUT"
    fi
    for p in 111:rpcbind-mount 2049:nfs; do
      port="${p%%:*}"; label="${p##*:}"
      if check_port "$NFS_SERVER_IP" "$port"; then
        echo "  [OK]   $NFS_SERVER_IP:$port ($label)" >> "$OUT"
      else
        echo "  [FAIL] $NFS_SERVER_IP:$port ($label)" >> "$OUT"
      fi
    done
    if command -v showmount &>/dev/null; then
      timeout 5 showmount -e "$NFS_SERVER_IP" >> "$OUT" 2>&1
    else
      echo "[showmount not installed]" >> "$OUT"
    fi
  else
    echo -e "\n[no NFS server set or detected]" >> "$OUT"
  fi

  section "MULTIPATH"
  run_cmd "multipath -ll" multipath -ll

  echo -e "\n===== END =====" >> "$OUT"
  echo "Written: $(pwd)/$OUT"
}

run_verify() {
  OUT="hvm-verify-${HOSTNAME_TAG}-${TS}.txt"
  EVID=$(mktemp)
  declare -a CHECK_NAMES CHECK_RESULTS
  pass() { CHECK_NAMES+=("$1"); CHECK_RESULTS+=("PASS"); }
  fail() { CHECK_NAMES+=("$1"); CHECK_RESULTS+=("FAIL"); }
  skip() { CHECK_NAMES+=("$1"); CHECK_RESULTS+=("SKIP"); }
  info() { CHECK_NAMES+=("$1"); CHECK_RESULTS+=("INFO"); }
  log() { echo -e "$1" >> "$EVID"; }

  if [[ -z "$PEER_HOST_IPS" && -f /etc/corosync/corosync.conf ]]; then
    MY_IPS=$(hostname -I 2>/dev/null)
    NODES=$(grep -oE 'ring0_addr:\s*[0-9.]+' /etc/corosync/corosync.conf 2>/dev/null | awk '{print $2}')
    FOUND=""
    for n in $NODES; do echo "$MY_IPS" | grep -qw "$n" || FOUND="$FOUND $n"; done
    PEER_HOST_IPS=$(echo "$FOUND" | xargs)
    [[ -n "$PEER_HOST_IPS" ]] && log "[auto-detected peers from corosync.conf: $PEER_HOST_IPS]"
  fi

  if [[ -z "$MORPHEUS_MANAGER_IP" ]]; then
    AUTO_MGR=$(ss -tn state established '( dport = :443 )' 2>/dev/null | grep -oE '[0-9]{1,3}(\.[0-9]{1,3}){3}:443' | cut -d: -f1 | head -1)
    if [[ -n "$AUTO_MGR" ]]; then
      MORPHEUS_MANAGER_IP="$AUTO_MGR"
      log "[auto-detected manager from active :443 connection: $AUTO_MGR]"
    else
      DEF_IF=$(ip route show default 2>/dev/null | awk '{print $5}' | head -1)
      GW_IP=$(ip route show default 2>/dev/null | awk '{print $3}' | head -1)
      if [[ -n "$DEF_IF" ]]; then
        NEIGHS=$(ip neigh show dev "$DEF_IF" 2>/dev/null | awk '$NF ~ /REACHABLE|STALE|DELAY|PERMANENT/ {print $1}')
        MGR_CANDIDATES=""
        for n in $NEIGHS; do
          [[ "$n" == "$GW_IP" ]] && continue
          echo "$PEER_HOST_IPS" | grep -qw "$n" && continue
          MGR_CANDIDATES="$MGR_CANDIDATES $n"
        done
        MGR_CANDIDATES=$(echo "$MGR_CANDIDATES" | xargs)
        if [[ -n "$MGR_CANDIDATES" ]]; then
          log "[manager candidates via ARP on $DEF_IF, excluding gateway $GW_IP and known peers: $MGR_CANDIDATES]"
          info "manager IP not confirmed, ARP candidates: $MGR_CANDIDATES -- set MORPHEUS_MANAGER_IP to confirm"
        fi
      fi
    fi
  fi


  if [[ -z "$NFS_SERVER_IP" ]]; then
    AUTO_NFS=$(findmnt -t nfs,nfs4 -no SOURCE 2>/dev/null | head -1 | cut -d: -f1)
    [[ -n "$AUTO_NFS" ]] && NFS_SERVER_IP="$AUTO_NFS" && log "[auto-detected NFS server from existing mount: $AUTO_NFS]"
  fi

  log "=== BONDS ==="
  if [[ -n "$BOND_CHECKS" ]]; then
    PAIRS="$BOND_CHECKS"
  else
    PAIRS=$(ip -br link show type bond 2>/dev/null | awk '{print $1":"}')
  fi
  [[ -z "$PAIRS" ]] && skip "bond checks (no bonds found)"
  for pair in $PAIRS; do
    BOND="${pair%%:*}"
    EXPECT="${pair#*:}"
    [[ "$EXPECT" == "$pair" ]] && EXPECT=""

    log "\n--- $BOND ---"
    if ip -br link show "$BOND" &>/dev/null; then
      log "$(ip -br link show "$BOND")"
      STATE=$(ip -br link show "$BOND" | awk '{print $2}')
      [[ "$STATE" == "UP" ]] && pass "$BOND up" || fail "$BOND up (state: $STATE)"
    else
      fail "$BOND exists"
      continue
    fi

    if [[ -f "/proc/net/bonding/$BOND" ]]; then
      cat "/proc/net/bonding/$BOND" >> "$EVID"
      DOWN=$(grep -c "MII Status: down" "/proc/net/bonding/$BOND" 2>/dev/null || echo 0)
      [[ "$DOWN" -eq 0 ]] && pass "$BOND all slaves up" || fail "$BOND has $DOWN slave(s) down"
    else
      skip "$BOND slave check"
    fi

    if command -v ovs-vsctl &>/dev/null; then
      ACTUAL=$(ovs-vsctl port-to-br "$BOND" 2>/dev/null)
      BOND_RC=$?
      log "port-to-br $BOND: $([[ $BOND_RC -eq 0 ]] && echo "$ACTUAL" || echo "not on any bridge")"
      if [[ -n "$EXPECT" ]]; then
        if [[ $BOND_RC -eq 0 && "$ACTUAL" == "$EXPECT" ]]; then
          pass "$BOND on expected bridge ($EXPECT)"
        else
          fail "$BOND on '${ACTUAL:-none}', expected '$EXPECT'"
        fi
      elif [[ $BOND_RC -eq 0 && -n "$ACTUAL" ]]; then
        info "$BOND currently on bridge: $ACTUAL"
      else
        info "$BOND not attached to any bridge"
      fi
    else
      skip "$BOND OVS check"
    fi
  done

  log "\n=== VLAN SUB-INTERFACE OVS ATTACHMENT ==="
  for v in $(ip -br link show type vlan 2>/dev/null | awk '{print $1}'); do
    if ip -d link show "$v" 2>/dev/null | grep -q "master ovs-system"; then
      pass "$v is OVS-attached"
    else
      HASIP=$(ip -4 -br addr show "$v" 2>/dev/null | awk '{print $3}')
      if [[ -n "$HASIP" ]]; then
        fail "$v is a bare kernel VLAN interface carrying $HASIP, not attached to OVS - can silently intercept tagged traffic meant for an OVS bridge on the same VLAN, with no error anywhere in the stack"
      else
        info "$v is a bare kernel VLAN interface (no IP), not attached to OVS"
      fi
    fi
  done

  log "\n=== VLANS ==="
  if command -v ovs-vsctl &>/dev/null; then
    if [[ -n "$VLAN_EXPECT" ]]; then
      for v in $VLAN_EXPECT; do
        MATCH=$(ovs-vsctl --columns=name,tag find Port tag="$v" 2>&1)
        log "VLAN $v: $MATCH"
        [[ -n "$MATCH" ]] && pass "VLAN $v present on a port" || fail "VLAN $v not found"
      done
    else
      FOUND=$(ovs-vsctl --format=csv --no-headings --columns=name,tag list Port 2>/dev/null | awk -F, '$2!="" && $2!="[]"')
      log "$FOUND"
      if [[ -n "$FOUND" ]]; then
        info "tagged ports found: $(echo "$FOUND" | wc -l), see evidence"
      else
        info "no tagged OVS ports found"
      fi
    fi
  else
    skip "VLAN check (ovs-vsctl not found)"
  fi

  log "\n=== MANAGER REACHABILITY ==="
  if [[ -n "$MORPHEUS_MANAGER_IP" ]]; then
    for p in 22:ssh 443:agent 7443:console; do
      port="${p%%:*}"; label="${p##*:}"
      if check_port "$MORPHEUS_MANAGER_IP" "$port"; then
        log "  [OK] $MORPHEUS_MANAGER_IP:$port ($label)"; pass "manager $port ($label)"
      else
        log "  [FAIL] $MORPHEUS_MANAGER_IP:$port ($label)"; fail "manager $port ($label)"
      fi
    done
  else
    skip "manager reachability (no IP set or detected)"
  fi

  log "\n=== PEER HOST REACHABILITY ==="
  if [[ -n "$PEER_HOST_IPS" ]]; then
    for peer in $PEER_HOST_IPS; do
      for p in 3300:ceph-mon 6789:ceph-mon-legacy 2224:pcsd 3121:pacemaker-remote 9929:qdevice 21064:dlm; do
        port="${p%%:*}"; label="${p##*:}"
        if check_port "$peer" "$port"; then
          log "  [OK] $peer:$port ($label)"; pass "$peer:$port ($label)"
        else
          log "  [FAIL] $peer:$port ($label)"; fail "$peer:$port ($label)"
        fi
      done
    done
  else
    skip "peer reachability (no peers set or detected)"
  fi

  log "\n=== STORAGE INTERFACE (should stay outside OVS) ==="
  if [[ -z "$STORAGE_IF" ]]; then
    DEFAULT_IF=$(ip route show default 2>/dev/null | awk '{print $5}' | head -1)
    BRIDGES=$(ovs-vsctl list-br 2>/dev/null)
    OVS_PORTS=""
    for br in $BRIDGES; do
      OVS_PORTS="$OVS_PORTS $(ovs-vsctl list-ports "$br" 2>/dev/null)"
    done
    OVS_PORTS="$OVS_PORTS $BRIDGES"
    OVS_PORTS=$(echo "$OVS_PORTS" | tr ' ' '\n' | sort -u)
    CANDIDATES=""
    for ifc in $(ip -br addr show 2>/dev/null | awk '$1!="lo"{print $1}'); do
      [[ "$ifc" == "$DEFAULT_IF" ]] && continue
      echo "$OVS_PORTS" | grep -qx "$ifc" && continue
      HASIP=$(ip -4 -br addr show "$ifc" 2>/dev/null | awk '{print $3}')
      [[ -n "$HASIP" ]] && CANDIDATES="$CANDIDATES $ifc"
    done
    CANDIDATES=$(echo "$CANDIDATES" | xargs)
    if [[ $(echo "$CANDIDATES" | wc -w) -eq 1 ]]; then
      STORAGE_IF="$CANDIDATES"
      log "[auto-detected single storage candidate: $STORAGE_IF]"
    elif [[ -n "$CANDIDATES" ]]; then
      log "candidates (has IPv4, outside OVS, not default route): $CANDIDATES"
      info "multiple storage candidates found, set STORAGE_IF to confirm one"
    else
      skip "storage interface check (no candidates found)"
    fi
  fi
  if [[ -n "$STORAGE_IF" ]]; then
    log "$(ip -br addr show "$STORAGE_IF" 2>&1)"
    INBRIDGE=$(ovs-vsctl port-to-br "$STORAGE_IF" 2>/dev/null)
    STOR_RC=$?
    if [[ $STOR_RC -eq 0 && -n "$INBRIDGE" ]]; then
      fail "storage interface ($STORAGE_IF) found inside OVS bridge '$INBRIDGE'"
    else
      pass "storage interface ($STORAGE_IF) outside OVS"
    fi
  fi

  log "\n=== NFS ==="
  if [[ -n "$NFS_SERVER_IP" ]]; then
    log "route used: $(ip route get "$NFS_SERVER_IP" 2>&1)"
    if ping -c 2 -W 2 "$NFS_SERVER_IP" &>/dev/null; then
      log "  [OK] $NFS_SERVER_IP ping"; pass "NFS server ping"
    else
      log "  [FAIL] $NFS_SERVER_IP ping"; fail "NFS server ping"
    fi
    if check_port "$NFS_SERVER_IP" 111; then
      log "  [OK] $NFS_SERVER_IP:111 (rpcbind-mount)"; pass "NFS server port 111 (rpcbind, needed for NFSv3 mount)"
    else
      log "  [FAIL] $NFS_SERVER_IP:111 (rpcbind-mount)"; fail "NFS server port 111 (rpcbind, needed for NFSv3 mount)"
    fi
    if check_port "$NFS_SERVER_IP" 2049; then
      log "  [OK] $NFS_SERVER_IP:2049"; pass "NFS server reachable"
    else
      log "  [FAIL] $NFS_SERVER_IP:2049"; fail "NFS server reachable"
    fi
  else
    skip "NFS reachability (no server set or detected)"
  fi

  log "\n=== CLUSTER MEMBERSHIP ==="
  if command -v corosync-cfgtool &>/dev/null; then
    corosync-cfgtool -s >> "$EVID" 2>&1
    info "corosync-cfgtool ran, read evidence for member/quorum count"
  elif command -v pcs &>/dev/null; then
    pcs status >> "$EVID" 2>&1
    info "pcs status ran, read evidence"
  else
    skip "cluster membership (no corosync-cfgtool/pcs, check Morpheus UI cluster page)"
  fi

  {
    echo "HVM verification — $HOSTNAME_TAG — $(date)"
    echo ""
    echo "=== SUMMARY ==="
    FAILCOUNT=0
    for i in "${!CHECK_NAMES[@]}"; do
      printf "  %-6s %s\n" "${CHECK_RESULTS[$i]}" "${CHECK_NAMES[$i]}"
      [[ "${CHECK_RESULTS[$i]}" == "FAIL" ]] && FAILCOUNT=$((FAILCOUNT+1))
    done
    echo ""
    if [[ "$FAILCOUNT" -eq 0 ]]; then
      echo "RESULT: no failures — safe to move on"
    else
      echo "RESULT: $FAILCOUNT check(s) failed — review before proceeding"
    fi
    echo ""
    echo "=== EVIDENCE ==="
    cat "$EVID"
  } > "$OUT"

  rm -f "$EVID"
  echo "Written: $(pwd)/$OUT"
}

MODE="${1:-}"
if [[ -z "$MODE" ]]; then
  echo "HVM Toolkit — $HOSTNAME_TAG"
  echo "  1) diagnose  (pre-fix capture: interfaces, OVS, routing, firewall, NFS)"
  echo "  2) verify    (post-fix check: bonds/VLANs/reachability, pass/fail/info)"
  read -rp "Choose [1/2]: " CHOICE
  case "$CHOICE" in
    1) MODE="diagnose" ;;
    2) MODE="verify" ;;
    *) echo "Invalid choice."; exit 1 ;;
  esac
fi

case "$MODE" in
  diagnose) run_diagnose ;;
  verify)   run_verify ;;
  *) echo "Usage: $0 [diagnose|verify]"; exit 1 ;;
esac
