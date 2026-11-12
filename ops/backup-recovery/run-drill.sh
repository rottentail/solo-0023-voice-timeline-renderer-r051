#!/usr/bin/env bash
#
# run-drill.sh — build the operational tools and run the fully isolated
# backup/restore drill, refreshing the replayable evidence files under
# docs/backup-recovery/evidence/.
#
# Everything runs against throwaway data directories created by backupdrill
# itself; no operator data path is ever passed in, so a drill cannot touch
# production or any other legitimate data.
#
# Usage:
#   run-drill.sh [-s all|closed-history|torn-copies|live-write|restore-guard]
set -euo pipefail

SCENARIO="all"
while getopts ":s:" opt; do
  case "${opt}" in
    s) SCENARIO="${OPTARG}" ;;
    *) echo "usage: $0 [-s SCENARIO]" >&2; exit 1 ;;
  esac
done

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${ROOT_DIR}"

# CGO_ENABLED=0 keeps the build independent of local C toolchain flags.
export CGO_ENABLED=0
GO="${GO:-go}"

echo "[drill] building verifier and drill binaries"
"${GO}" build -buildvcs=false -o bin/backupverify ./cmd/backupverify
"${GO}" build -buildvcs=false -o bin/backupdrill ./cmd/backupdrill

echo "[drill] running scenario: ${SCENARIO}"
./bin/backupdrill -scenario "${SCENARIO}" -evidence-dir docs/backup-recovery/evidence

echo "[drill] evidence refreshed in docs/backup-recovery/evidence/"
