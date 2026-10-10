#!/usr/bin/env bash
# Required post-run host process census. A missing/broken ps is a FAILURE.
set -euo pipefail
snapshot="$(ps -eo pid=,args=)" || {
  echo "cannot enumerate host processes after real VM test" >&2
  exit 1
}
# Both the self-reexec helper and custom helper --config VM processes are
# included. Match argv, not an exit code or a guest command's success.
if printf '%s\n' "$snapshot" | grep -E '(__krunlet_self_helper|__helper[[:space:]]+--config)' ; then
  echo "leftover krunlet helper/VM host processes after run" >&2
  exit 1
fi
