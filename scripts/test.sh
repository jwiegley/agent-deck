#!/usr/bin/env bash
set -euo pipefail

# Resolve populated build caches before replacing the caller's HOME.
gocache=$(go env GOCACHE)
gomodcache=$(go env GOMODCACHE)
sandbox=$(mktemp -d /tmp/agent-deck-tests.XXXXXX)
trap 'rm -rf -- "$sandbox"' EXIT
mkdir -p "$sandbox/home" "$sandbox/tmp" "$sandbox/tmux"

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
)
for name in GOTOOLCHAIN GOFLAGS CGO_ENABLED CC CXX SDKROOT DEVELOPER_DIR MACOSX_DEPLOYMENT_TARGET CI GITHUB_ACTIONS; do
    if [[ ${!name+x} ]]; then
        test_env+=("$name=${!name}")
    fi
done
if [[ $# -eq 0 ]]; then
    set -- -race -count=1 ./...
fi
env -i "${test_env[@]}" go test "$@"
