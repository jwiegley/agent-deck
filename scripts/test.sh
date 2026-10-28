#!/usr/bin/env bash
set -euo pipefail

# Resolve populated build caches before replacing the caller's HOME.
gocache=$(go env GOCACHE)
gomodcache=$(go env GOMODCACHE)
# The sandbox root is the tests' TMPDIR, and its name is kept short. Upstream's
# tests run in CI with TMPDIR=/tmp and build paths under t.TempDir(), which
# repeats the test's name: a tmux socket there must fit the 107 bytes of a
# Unix socket path (TestLaunchJSON_FolderNotTrustedWarning puts one under
# <TempDir>/tmux/tmux-<uid>/default), and a longer root failed those tests
# with "File name too long".
sandbox=$(mktemp -d /tmp/adt.XXXXXX)
trap 'rm -rf -- "$sandbox"' EXIT
sandbox=$(cd "$sandbox" && pwd -P)
mkdir -p "$sandbox/home" "$sandbox/tmux"

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
    "TMPDIR=$sandbox"
    "TMP=$sandbox"
    "TEMP=$sandbox"
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
# Upstream's pre-push command plus upstream CI's per-package budget
# (.github/workflows/go-test.yml: -race -timeout 20m). go test's default of
# 10 minutes is shorter than internal/session takes on its own.
if [[ $# -eq 0 ]]; then
    set -- -race -count=1 -timeout 20m ./...
fi
status=0
env -i "${test_env[@]}" go test "$@" || status=$?
# tools/visualcheck leaves its contact sheet, report and redraw frames in
# $TMPDIR/visualcheck-artifacts for a reviewer to judge a DIFF. Move them out
# of the sandbox before it is removed, but only when the run failed.
artifact_tmp=${TMPDIR:-/tmp}
if [[ $status -ne 0 && -d $sandbox/visualcheck-artifacts ]] &&
    kept=$(mktemp -d "${artifact_tmp%/}/agent-deck-test-artifacts.XXXXXX") &&
    mv -- "$sandbox/visualcheck-artifacts" "$kept/"; then
    echo "scripts/test.sh: kept visualcheck artifacts in $kept/visualcheck-artifacts" >&2
fi
exit "$status"
