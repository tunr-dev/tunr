#!/usr/bin/env bash
# server-setup.sh — provision a FRESH Ubuntu box for the tunr relay + cloud runner.
#
# Run this ONCE on a brand-new server (as root) before starting the stack.
# It is idempotent: safe to re-run. It sets up the host layer only — Docker,
# gVisor (runsc) for app isolation, the isolated `tunr-apps` network, and the
# /opt/tunr directory skeleton. Configuration and the compose stack come next
# (see docs/SELF_HOSTING.md, and "Next steps" printed at the end).
#
# Why gVisor: the cloud runner executes arbitrary / AI-generated user code. Bare
# Docker shares the host kernel; gVisor (runsc) intercepts syscalls in userspace
# and dramatically shrinks the attack surface. Combined with no-new-privileges,
# read-only rootfs, pid/mem/cpu limits and an icc=false bridge (all applied by the
# DockerDriver at `docker run`), this is the v0 isolation story.
#
# Usage (on the server):
#   curl -fsSL https://raw.githubusercontent.com/tunr-dev/tunr/main/scripts/server-setup.sh | sudo bash
#   # or: scp this file over and run:  sudo bash server-setup.sh
#
# Tested on Ubuntu 24.04 / 26.04 (Hetzner Cloud, KVM guest → gVisor systrap platform).

set -euo pipefail

APPS_NETWORK="${TUNR_APPS_NETWORK:-tunr-apps}"
OPT_DIR="${TUNR_OPT_DIR:-/opt/tunr}"

log()  { printf '\033[1;35m[setup]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[setup]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[setup]\033[0m %s\n' "$*" >&2; exit 1; }

[[ "$(id -u)" -eq 0 ]] || die "run as root (sudo bash server-setup.sh)"

# ── 1. Base packages ──────────────────────────────────────────────────────────
log "Installing base packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq apt-transport-https ca-certificates curl gnupg rsync jq lsb-release >/dev/null

# ── 2. Docker Engine + Compose plugin ─────────────────────────────────────────
if command -v docker >/dev/null 2>&1; then
  log "Docker already present: $(docker --version)"
else
  log "Installing Docker Engine (get.docker.com)"
  curl -fsSL https://get.docker.com | sh
  systemctl enable --now docker
fi
docker compose version >/dev/null 2>&1 || warn "docker compose plugin missing — install docker-compose-plugin"

# ── 3. gVisor (runsc) ─────────────────────────────────────────────────────────
if command -v runsc >/dev/null 2>&1; then
  log "gVisor already installed: $(runsc --version | head -1)"
else
  log "Installing gVisor (runsc) from the official apt repo"
  curl -fsSL https://gvisor.dev/archive.key | gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" \
    > /etc/apt/sources.list.d/gvisor.list
  apt-get update -qq
  apt-get install -y -qq runsc >/dev/null
fi

# Register runsc as a Docker runtime (writes /etc/docker/daemon.json) + reload.
log "Registering runsc as a Docker runtime"
runsc install
systemctl reload docker || systemctl restart docker
sleep 2

# Verify gVisor actually runs. On Hetzner Cloud (KVM guest) the default systrap
# platform works without nested virt.
log "Verifying gVisor (expect 'gVisor' in dmesg)"
if docker run --rm --runtime=runsc alpine sh -c 'dmesg 2>/dev/null | grep -qi gvisor'; then
  log "gVisor OK — containers can run under runsc"
else
  warn "gVisor smoke test did not confirm runsc. Check: docker run --rm --runtime=runsc alpine dmesg | grep -i gvisor"
  warn "If runsc fails on this kernel, the DockerDriver can fall back to runc temporarily (weaker isolation)."
fi

# ── 4. Isolated app network ───────────────────────────────────────────────────
# icc=false: containers on this bridge cannot talk to each other directly; the
# relay reaches them by name, tenants stay isolated. shim HMAC covers the rest.
if docker network inspect "$APPS_NETWORK" >/dev/null 2>&1; then
  log "Network '$APPS_NETWORK' already exists"
else
  log "Creating isolated app network '$APPS_NETWORK' (icc disabled)"
  docker network create --opt com.docker.network.bridge.enable_icc=false "$APPS_NETWORK" >/dev/null
fi

# ── 5. Directory skeleton ─────────────────────────────────────────────────────
log "Ensuring $OPT_DIR skeleton"
mkdir -p "$OPT_DIR/src" "$OPT_DIR/build-work"

# ── 6. Summary + next steps ───────────────────────────────────────────────────
cat <<EOF

$(log "Provisioning complete.")

Host layer ready:
  • Docker:   $(docker --version)
  • gVisor:   $(runsc --version | head -1)
  • Network:  $APPS_NETWORK (icc=false)
  • Dir:      $OPT_DIR (src, build-work)

Next steps (see docs/SELF_HOSTING.md for the full walkthrough):
  1. Density prerequisites — zram + cgroup soft limits (not optional for the
     cloud runner):   sudo ./scripts/host-density.sh
  2. Put the repo in $OPT_DIR/src and write $OPT_DIR/.env
     (TUNR_DOMAIN, TUNR_JWT_SECRET, DATABASE_URL, RUNNER_URL, RUNNER_SECRET).
  3. Start the stack:
       docker compose up -d                                   # relay + Caddy + Postgres
       docker compose -f docker-compose.runner.yml up -d      # cloud runner (tunr deploy)
  4. Attach the relay to the app network so it can reach deployed apps:
       docker network connect $APPS_NETWORK <relay-container>
EOF
