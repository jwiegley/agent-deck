#!/usr/bin/env bash
set -euo pipefail

# Resolve populated build caches before replacing the caller's HOME.
gocache=$(go env GOCACHE)
gomodcache=$(go env GOMODCACHE)
sandbox=$(mktemp -d /tmp/agent-deck-tests.XXXXXX)
trap 'rm -rf -- "$sandbox"' EXIT
sandbox=$(cd "$sandbox" && pwd -P)
mkdir -p "$sandbox/home" "$sandbox/tmp" "$sandbox/tmux"

# tmux falls back to the account's login shell when SHELL is unset, and a
# login shell reading the empty sandbox HOME may prompt (zsh-newuser-install
# consumes the first keystroke of a typed command). Pin the shell the tests
# are written against.
bash_path=$(command -v bash)

# Leave XDG unset so tests that replace HOME also isolate their XDG paths.
test_env=(
    "PATH=$PATH"
    "HOME=$sandbox/home"
    "USERPROFILE=$sandbox/home"
    "TMPDIR=$sandbox/tmp"
    "TMP=$sandbox/tmp"
    "TEMP=$sandbox/tmp"
    "TMUX_TMPDIR=$sandbox/tmux"
    "GOENV=off"
    "GOCACHE=$gocache"
    "GOMODCACHE=$gomodcache"
    "LANG=en_US.UTF-8"
    "SHELL=$bash_path"
)
# Toolchain settings and the test knobs a caller sets on purpose (CI's
# PERF_BUDGET_MULTIPLIER, tools/visualcheck's binary and sandbox switches).
for name in GOTOOLCHAIN GOFLAGS CGO_ENABLED CC CXX SDKROOT DEVELOPER_DIR MACOSX_DEPLOYMENT_TARGET CI GITHUB_ACTIONS \
    PERF_BUDGET_MULTIPLIER VISUALCHECK_BINARY VISUALCHECK_KEEP_SANDBOX; do
    if [[ ${!name+x} ]]; then
        test_env+=("$name=${!name}")
    fi
done
if [[ $# -eq 0 ]]; then
    set -- -race -count=1 ./...
fi
env -i "${test_env[@]}" go test "$@"
