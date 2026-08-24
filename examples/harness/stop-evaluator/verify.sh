#!/bin/sh

set -u

# Stop also notifies hooks after cancellation, provider failure, and exhausted
# budgets. Those notifications cannot continue the model loop, so avoid doing
# expensive repository work for them. An empty payload means a person or the
# agent invoked this verifier directly.
payload=""
hook_invocation=false
if [ ! -t 0 ]; then
  IFS= read -r payload || true
  if [ -n "$payload" ]; then
    hook_invocation=true
  fi
fi
case "$payload" in
  *'"can_block":false'*|*'"can_block": false'*) exit 0 ;;
esac

verify_log=$(mktemp "${TMPDIR:-/tmp}/harness-verify.XXXXXX") || exit 1
trap 'rm -f "$verify_log"' 0 HUP INT TERM

run_check() {
  printf '$' >>"$verify_log"
  printf ' %s' "$@" >>"$verify_log"
  printf '\n' >>"$verify_log"
  "$@" >>"$verify_log" 2>&1
}

passed=0
failed=0
if run_check go build ./...; then passed=$((passed + 1)); else failed=$((failed + 1)); fi
if run_check go vet ./...; then passed=$((passed + 1)); else failed=$((failed + 1)); fi
if run_check go test ./...; then passed=$((passed + 1)); else failed=$((failed + 1)); fi

if [ "$failed" -eq 0 ]; then
  if [ "$hook_invocation" = true ]; then
    printf '%s\n' '{"accepted":true,"score":3,"score_direction":"maximize","remaining_requirements":0}'
  else
    printf '%s\n' 'Repository verification passed.'
  fi
  exit 0
fi

if [ "$hook_invocation" = true ]; then
  printf '{"accepted":false,"score":%s,"score_direction":"maximize","remaining_requirements":%s,"reason":"Repository verification failed; run ./examples/harness/stop-evaluator/verify.sh directly for details."}\n' "$passed" "$failed"
  exit 2
fi

printf '%s\n' 'Repository verification failed. Fix the failures below, rerun ./examples/harness/stop-evaluator/verify.sh directly, and finish only after it exits 0.'
tail -n 160 "$verify_log" | tail -c 32768
exit 2
