#!/usr/bin/env bash
#
# online-backup.sh — online, no-downtime backup of a running voice-timeline
# service data directory.
#
# The service keeps accepting sources, timelines, and renders while this
# script runs. A plain recursive copy can observe a torn state (a job file
# captured between its state transition and its atomic render-folder rename),
# so every round is independently verified. A round is PROMOTED only when
# verification reports status=valid; rounds with blockers are retained for
# forensics but never replace the current good snapshot. A valid round that
# still lists in-flight jobs is promoted because its closed history is
# consistent, and the in-flight list is printed for re-submission handling.
#
# Usage:
#   online-backup.sh -d DATA_DIR -b BACKUP_ROOT [-r MAX_ROUNDS] [-s SLEEP_SECONDS]
#
# Exit codes: 0 promotion done, 2 no round converged to a verifiable backup,
# 1 usage/runtime error.
set -euo pipefail

DATA_DIR=""
BACKUP_ROOT=""
MAX_ROUNDS=3
SLEEP_SECONDS=2

while getopts ":d:b:r:s:" opt; do
  case "${opt}" in
    d) DATA_DIR="${OPTARG}" ;;
    b) BACKUP_ROOT="${OPTARG}" ;;
    r) MAX_ROUNDS="${OPTARG}" ;;
    s) SLEEP_SECONDS="${OPTARG}" ;;
    *) echo "usage: $0 -d DATA_DIR -b BACKUP_ROOT [-r MAX_ROUNDS] [-s SLEEP_SECONDS]" >&2; exit 1 ;;
  esac
done

if [[ -z "${DATA_DIR}" || -z "${BACKUP_ROOT}" ]]; then
  echo "usage: $0 -d DATA_DIR -b BACKUP_ROOT [-r MAX_ROUNDS] [-s SLEEP_SECONDS]" >&2
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VERIFIER="${SCRIPT_DIR}/../../bin/backupverify"
if [[ ! -x "${VERIFIER}" ]]; then
  echo "verifier not found or not executable: ${VERIFIER}" >&2
  echo "build it with: go build -buildvcs=false -o bin/backupverify ./cmd/backupverify" >&2
  exit 1
fi
if [[ ! -d "${DATA_DIR}" ]]; then
  echo "data directory does not exist: ${DATA_DIR}" >&2
  exit 1
fi

mkdir -p "${BACKUP_ROOT}/rounds"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"

promoted=0
for round in $(seq 1 "${MAX_ROUNDS}"); do
  ROUND_DIR="${BACKUP_ROOT}/rounds/${STAMP}-round${round}"
  mkdir -p "${ROUND_DIR}"
  echo "[online-backup] round ${round}/${MAX_ROUNDS}: copying live data (service stays running) -> ${ROUND_DIR}"

  # Read-only recursive copy. The service is never stopped or paused.
  cp -a "${DATA_DIR}/." "${ROUND_DIR}/"

  # Reports and manifests live beside the round directory, never inside it, so
  # the copy itself matches the documented data layout exactly.
  REPORT="${BACKUP_ROOT}/rounds/${STAMP}-round${round}.verify.json"
  set +e
  "${VERIFIER}" verify -root "${ROUND_DIR}" -json "${REPORT}"
  verify_status=$?
  set -e

  if [[ ${verify_status} -eq 0 ]]; then
    in_flight="$(jq -r '.in_flight_jobs | length' "${REPORT}")"
    completed="$(jq -r '.closed_history.completed_renders' "${REPORT}")"
    echo "[online-backup] round ${round}: VALID (completed_renders=${completed}, in_flight=${in_flight})"
    if [[ "${in_flight}" != "0" ]]; then
      echo "[online-backup] in-flight jobs captured mid-progress (NOT promised as completed; re-submit with new render IDs):"
      jq -r '.in_flight_jobs[] | "  - " + .' "${REPORT}"
    fi
    "${VERIFIER}" manifest -root "${ROUND_DIR}" -out "${BACKUP_ROOT}/rounds/${STAMP}-round${round}.manifest.json" >/dev/null
    ln -sfn "${ROUND_DIR}" "${BACKUP_ROOT}/current"
    ln -sfn "${BACKUP_ROOT}/rounds/${STAMP}-round${round}.manifest.json" "${BACKUP_ROOT}/current.manifest.json"
    promoted=1
    echo "[online-backup] promoted ${ROUND_DIR} as ${BACKUP_ROOT}/current"
    break
  fi

  echo "[online-backup] round ${round}: INVALID (exit ${verify_status}); retaining for forensics, retrying" >&2
  jq -r '.issues[] | select(.severity=="blocker") | "  blocker: " + .code + " " + .path + " " + .message' "${REPORT}" >&2 || true
  sleep "${SLEEP_SECONDS}"
done

if [[ ${promoted} -ne 1 ]]; then
  echo "[online-backup] FATAL: none of the ${MAX_ROUNDS} rounds produced a verifiable backup; existing current/ (if any) was not touched" >&2
  exit 2
fi
