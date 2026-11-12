#!/usr/bin/env bash
#
# isolated-restore.sh — restore a verified backup into an EMPTY target
# directory and re-verify it, without touching any existing data.
#
# The restore is refused when the target exists and is non-empty, so neither a
# production data directory nor another drill workspace can be overwritten.
# After restore, a fresh service instance is started on an isolated port for
# smoke checks; the original service is never stopped by this script.
#
# Usage:
#   isolated-restore.sh -b BACKUP_DIR -t RESTORE_TARGET [-p PORT]
#
# Exit codes: 0 restore verified and smoke checks passed, 2 backup failed
# verification, 1 usage/runtime error or target not empty.
set -euo pipefail

BACKUP_DIR=""
RESTORE_TARGET=""
PORT="18099"

while getopts ":b:t:p:" opt; do
  case "${opt}" in
    b) BACKUP_DIR="${OPTARG}" ;;
    t) RESTORE_TARGET="${OPTARG}" ;;
    p) PORT="${OPTARG}" ;;
    *) echo "usage: $0 -b BACKUP_DIR -t RESTORE_TARGET [-p PORT]" >&2; exit 1 ;;
  esac
done

if [[ -z "${BACKUP_DIR}" || -z "${RESTORE_TARGET}" ]]; then
  echo "usage: $0 -b BACKUP_DIR -t RESTORE_TARGET [-p PORT]" >&2
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
VERIFIER="${ROOT_DIR}/bin/backupverify"
SERVICE_BIN="${ROOT_DIR}/bin/voiceservice"
for tool in "${VERIFIER}" "${SERVICE_BIN}"; do
  if [[ ! -x "${tool}" ]]; then
    echo "required binary missing: ${tool}" >&2
    echo "build with: go build -buildvcs=false -o bin/... ./cmd/..." >&2
    exit 1
  fi
done

# Resolve symlinks (e.g. backups/current -> rounds/...) so verification reads
# the real directory rather than the link itself.
BACKUP_DIR="$(readlink -f "${BACKUP_DIR}")"

# Hard guard: never overwrite existing legitimate data.
if [[ -e "${RESTORE_TARGET}" ]]; then
  if [[ -n "$(ls -A "${RESTORE_TARGET}" 2>/dev/null)" ]]; then
    echo "refusing restore: target ${RESTORE_TARGET} exists and is not empty" >&2
    exit 1
  fi
fi

echo "[restore] verifying backup before copying: ${BACKUP_DIR}"
REPORT="$(dirname "${RESTORE_TARGET}")/.$(basename "${RESTORE_TARGET}").pre-restore-verify.json"
if ! "${VERIFIER}" verify -root "${BACKUP_DIR}" -json "${REPORT}"; then
  echo "[restore] FATAL: backup failed verification; restore aborted" >&2
  exit 2
fi

echo "[restore] copying into empty target: ${RESTORE_TARGET}"
"${VERIFIER}" restore -from "${BACKUP_DIR}" -to "${RESTORE_TARGET}"

IN_FLIGHT="$(jq -r '.in_flight_jobs | length' "${REPORT}")"
if [[ "${IN_FLIGHT}" != "0" ]]; then
  echo "[restore] WARNING: ${IN_FLIGHT} in-flight job(s) restored as non-terminal; they are not completed deliverables:"
  jq -r '.in_flight_jobs[] | "  - " + .' "${REPORT}"
fi

echo "[restore] starting isolated service on 127.0.0.1:${PORT} for smoke checks"
"${SERVICE_BIN}" -addr "127.0.0.1:${PORT}" -data "${RESTORE_TARGET}" >"${RESTORE_TARGET}.service.log" 2>&1 &
SVC_PID=$!
trap 'kill ${SVC_PID} 2>/dev/null || true' EXIT

for _ in $(seq 1 20); do
  if curl -sf "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.5
done
if ! curl -sf "http://127.0.0.1:${PORT}/healthz" >/dev/null; then
  echo "[restore] FATAL: restored service did not become healthy; see ${RESTORE_TARGET}.service.log" >&2
  exit 1
fi
echo "[restore] healthz OK"

# Replay every completed render: output bytes must match the receipt digest.
failures=0
while IFS= read -r job_file; do
  render_id="$(basename "${job_file}" .json)"
  state="$(jq -r '.state' "${job_file}")"
  if [[ "${state}" != "completed" ]]; then
    echo "[restore] non-terminal job ${render_id} state=${state} (must be re-submitted with a new render ID)"
    continue
  fi
  expected="$(jq -r '.receipt_digest' "${job_file}")"
  actual="$(curl -sf "http://127.0.0.1:${PORT}/v1/renders/${render_id}/output" | sha256sum | cut -d' ' -f1)"
  if [[ "${expected}" != "${actual}" ]]; then
    echo "[restore] digest mismatch for ${render_id}: receipt=${expected} actual=${actual}" >&2
    failures=$((failures + 1))
  else
    echo "[restore] replay OK ${render_id} digest=${actual}"
  fi
done < <(find "${RESTORE_TARGET}/jobs" -name '*.json' -type f | sort)

if [[ ${failures} -gt 0 ]]; then
  echo "[restore] FATAL: ${failures} replayed render(s) failed verification" >&2
  exit 1
fi
echo "[restore] PASS: target ${RESTORE_TARGET} is a verified, replayable recovery point"
