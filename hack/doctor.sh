#!/usr/bin/env bash
# Check that the local toolchain and Docker can run the platform.
set -uo pipefail

fail=0
ok()   { printf '  ok    %s\n' "$*"; }
warn() { printf '  warn  %s\n' "$*"; }
bad()  { printf '  FAIL  %s\n' "$*"; fail=1; }

need() {
  if command -v "$1" >/dev/null 2>&1; then ok "$1: $($2 2>&1 | head -1)"; else bad "$1 not found ($3)"; fi
}

echo "tools:"
need go      "go version"                     "brew install go"
need kind    "kind version"                   "brew install kind"
need helm    "helm version --short"           "brew install helm"
need kubectl "kubectl version --client"       "brew install kubectl"
need docker  "docker version --format {{.Client.Version}}" "install Docker Desktop"
need git     "git --version"                  "xcode-select --install"
need node    "node --version"                 "brew install node"

if command -v go >/dev/null 2>&1; then
  gov="$(go env GOVERSION | sed 's/^go//')"
  if [[ "$(printf '%s\n1.26\n' "$gov" | sort -V | head -1)" != "1.26" ]]; then bad "go $gov is older than 1.26"; fi
fi

echo "docker:"
if info="$(docker info --format '{{.NCPU}} {{.MemTotal}} {{.Architecture}}' 2>&1)"; then
  read -r cpus mem arch <<<"$info"
  gib=$((mem / 1024 / 1024 / 1024))
  ok "daemon reachable: ${cpus} CPUs, ${gib} GiB, ${arch}"
  if (( gib < 7 )); then bad "Docker has ${gib} GiB; give it at least 8 (10 recommended)"
  elif (( gib < 9 )); then warn "Docker has ${gib} GiB; 10 GiB recommended for the full stack"; fi
  if (( cpus < 4 )); then warn "Docker has ${cpus} CPUs; 6 recommended"; fi
else
  bad "docker daemon not reachable: $info"
fi

exit "$fail"
