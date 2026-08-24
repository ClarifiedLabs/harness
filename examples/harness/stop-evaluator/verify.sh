#!/bin/sh

set -u

# Stop also notifies hooks after cancellation, provider failure, and exhausted
# budgets. Those notifications cannot continue the model loop, so avoid doing
# expensive repository work for them. An empty payload means a person or the
# agent invoked this verifier directly.
payload=""
if [ ! -t 0 ]; then
  IFS= read -r payload || true
fi
case "$payload" in
  *'"can_block":false'*) exit 0 ;;
esac

verify_log=$(mktemp "${TMPDIR:-/tmp}/harness-verify.XXXXXX") || exit 1
trap 'rm -f "$verify_log"' 0 HUP INT TERM

run_check() {
  printf '$' >>"$verify_log"
  printf ' %s' "$@" >>"$verify_log"
  printf '\n' >>"$verify_log"
  "$@" >>"$verify_log" 2>&1
}

status=0
run_check go build ./... || status=1
run_check go vet ./... || status=1
run_check go test ./... || status=1

if [ "$status" -eq 0 ]; then
  exit 0
fi

printf '%s\n' 'Repository verification failed. Fix the failures below, rerun ./examples/harness/stop-evaluator/verify.sh directly, and finish only after it exits 0.'
tail -n 160 "$verify_log" | tail -c 32768
exit 2
