#!/usr/bin/env bash
# Prove the exact same post-run detector FAILS on a deliberately leaked fake.
set -euo pipefail
bash -c 'exec -a "__krunlet_self_helper --config /tmp/krunlet-deliberate-leak" sleep 30' &
leak_pid=$!
cleanup() {
  kill "$leak_pid" 2>/dev/null || true
  wait "$leak_pid" 2>/dev/null || true
}
trap cleanup EXIT
sleep 0.2
if bash scripts/check-no-helper-processes.sh; then
  echo "LEAK DETECTOR FALSE GREEN: deliberately leaked helper was not detected" >&2
  exit 1
fi
cleanup
trap - EXIT
bash scripts/check-no-helper-processes.sh
