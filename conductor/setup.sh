#!/usr/bin/env bash
set -euo pipefail

# Conductor Setup Script (Multi-Profile)
# Sets up per-profile conductor sessions, Telegram bridge, and launchd daemon.

AGENT_DECK_DIR="${HOME}/.agent-deck"
CONDUCTOR_DIR="${AGENT_DECK_DIR}/conductor"
CONFIG_PATH="${AGENT_DECK_DIR}/config.toml"
PLIST_NAME="com.agentdeck.conductor-bridge"
PLIST_DIR="${HOME}/Library/LaunchAgents"
PLIST_PATH="${PLIST_DIR}/${PLIST_NAME}.plist"
GHW_PLIST_NAME="com.agentdeck.gh-watcher"
GHW_PLIST_PATH="${PLIST_DIR}/${GHW_PLIST_NAME}.plist"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

info()  { echo -e "${CYAN}[info]${NC}  $*"; }
ok()    { echo -e "${GREEN}[ok]${NC}    $*"; }
warn()  { echo -e "${YELLOW}[warn]${NC}  $*"; }
fail()  { echo -e "${RED}[fail]${NC}  $*"; exit 1; }

# register_session <profile> <dir> <title>
#
# Issue #1854: this used to be
#
#   agent-deck add … 2>/dev/null && ok "registered" || warn "Could not register"
#
# and the warn branch told the user to do something that would fail. The python
# existence check above it exits non-zero both when the session genuinely does
# not exist AND when the probe itself could not run, so `add` is reached in two
# very different situations. Since #1850 an exact duplicate exits non-zero with
# an ALREADY_EXISTS payload, which took the warn branch; `2>/dev/null` then
# discarded the message that said so, leaving no way to tell the outcomes apart.
#
# --json carries the distinction directly, so branch on the code. stderr is kept
# on the failure path — discarding it on the one branch whose job is reporting a
# failure is how the original message got lost. Like the original, this never
# fails the script.
register_session() {
    local profile="$1" dir="$2" title="$3"
    local out rc code

    rc=0
    out="$(agent-deck -p "${profile}" add "${dir}" -t "${title}" -c claude -g "infra" --json 2>&1)" || rc=$?

    if [[ ${rc} -eq 0 ]]; then
        ok "  Session ${title} registered in ${profile}"
        return 0
    fi

    code="$(printf '%s' "${out}" | python3 -c "
import sys, json
try:
    print(json.loads(sys.stdin.read()).get('code', ''))
except Exception:
    print('')
" 2>/dev/null)"

    if [[ "${code}" == "ALREADY_EXISTS" ]]; then
        ok "  Session ${title} already registered in ${profile}"
        return 0
    fi

    warn "  Could not register ${title} (add manually from TUI)"
    warn "    ${out}"
    return 0
}

# sha256_of FILE
#
# Prints FILE's sha256 as 64 lower-case hex digits and a newline: the format of
# the .bridge.py.sha256 stamp that agent-deck keeps beside a bridge it wrote.
# FILE goes in on stdin, so that the line the tool prints carries no file name:
# GNU sha256sum and shasum start that line with a backslash when the name holds
# a backslash or a newline, and the hash would no longer be the first field.
# teardown.sh carries an identical copy; keep the two the same.
sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum < "$1" | awk '{print $1}'
    else
        shasum -a 256 < "$1" | awk '{print $1}'
    fi
}

# service_file_external PATH
#
# Exits 0, printing "symlink" or "read-only", when the launchd plist or systemd
# unit at PATH belongs to another manager: home-manager links its files into
# place, or installs them without a write bit. Exits 1, printing nothing, when
# the file is missing or ours to write. Like the Go installer it looks at the
# mode bits before access(2): root can write a 0444 file, but a file that grants
# no write bit to anyone was made to be left alone. find reads the bits because
# BSD and GNU stat differ. teardown.sh carries an identical copy.
service_file_external() {
    if [[ -L "$1" ]]; then
        echo symlink
        return 0
    fi
    if [[ -e "$1" ]] && [[ -n "$(find "$1" -prune ! -perm -0200 ! -perm -0020 ! -perm -0002 -print)" ]]; then
        echo read-only
        return 0
    fi
    if [[ -e "$1" && ! -w "$1" ]]; then
        echo read-only
        return 0
    fi
    return 1
}

# install_bridge_script
#
# Installs the canonical bridge, internal/session/conductor_bridge.py, as
# ${CONDUCTOR_DIR}/bridge.py, by the rule the Go installer (InstallBridgeScript)
# follows: bridge.py is replaced only while it is missing, identical to the
# canonical file, or still the file agent-deck stamped. The stamp,
# .bridge.py.sha256, holds the sha256 of the bridge agent-deck last wrote, in the
# format the Go installer reads, and a replaced bridge is kept as
# .bridge.py.previous. Any other bridge.py is the user's: one edited since, one
# written before the stamp existed, or a symlink. It is left alone. The new
# bridge is staged in a temp file beside bridge.py, and a step that fails stops
# the script (fail) and removes the temp file.
install_bridge_script() {
    local src="${SCRIPT_DIR}/../internal/session/conductor_bridge.py"
    local dest="${CONDUCTOR_DIR}/bridge.py"
    local stamp="${CONDUCTOR_DIR}/.bridge.py.sha256"
    local outcome tmp

    if [[ -L "${dest}" ]]; then
        echo "[skip] bridge.py is a symlink; left unchanged."
        return 0
    fi
    if [[ -e "${dest}" && ! -f "${dest}" ]]; then
        fail "refusing to replace ${dest}: not a regular file"
    fi
    # Without the canonical file the comparisons below would read "different",
    # and a bridge that is not stamped would be reported as local changes.
    [[ -f "${src}" ]] || fail "canonical bridge not found: ${src}"

    # The stamp matches when it is a regular file whose bytes, newline included,
    # are the sha256 of bridge.py, as the Go installer compares them.
    if [[ -e "${dest}" ]] && cmp -s "${src}" "${dest}"; then
        outcome="unchanged"
    elif [[ ! -e "${dest}" ]]; then
        outcome="installed"
    elif [[ -f "${stamp}" && ! -L "${stamp}" ]] && sha256_of "${dest}" | cmp -s - "${stamp}"; then
        outcome="updated"
    else
        echo "[skip] bridge.py has local changes; left unchanged. To take the shipped bridge, move it aside and re-run conductor/setup.sh."
        return 0
    fi

    if [[ "${outcome}" != "unchanged" ]]; then
        tmp="$(mktemp "${CONDUCTOR_DIR}/.bridge.py.new-XXXXXX")"
        if ! { cp "${src}" "${tmp}" && chmod 755 "${tmp}"; }; then
            rm -f "${tmp}"
            fail "could not stage ${tmp}"
        fi
        if [[ "${outcome}" == "updated" ]]; then
            # A hard link keeps the displaced inode, so an editor that still has
            # the old bridge open writes into the kept copy.
            if ! ln -f "${dest}" "${CONDUCTOR_DIR}/.bridge.py.previous"; then
                rm -f "${tmp}"
                fail "could not keep ${dest} as .bridge.py.previous"
            fi
        fi
        if ! mv -f "${tmp}" "${dest}"; then
            rm -f "${tmp}"
            fail "could not replace ${dest}"
        fi
    fi

    # Remove the stamp first, so that a symlinked one is replaced, not written
    # through. A crash before the write heals on the next run, as "unchanged".
    rm -f "${stamp}"
    sha256_of "${dest}" > "${stamp}"
    ok "bridge.py ${outcome}"
}

# render_bridge_plist
#
# Prints the bridge's launchd plist: the template in the checkout, with the
# python3 path, the bridge and log paths and HOME filled in.
render_bridge_plist() {
    sed \
        -e "s|__PYTHON3__|${PYTHON3_PATH}|g" \
        -e "s|__BRIDGE_PATH__|${CONDUCTOR_DIR}/bridge.py|g" \
        -e "s|__LOG_PATH__|${CONDUCTOR_DIR}/bridge.log|g" \
        -e "s|__HOME__|${HOME}|g" \
        "${SCRIPT_DIR}/${PLIST_NAME}.plist"
}

# render_ghw_plist
#
# Prints the GitHub watcher's launchd plist: the template in the checkout, with
# the tick script, watcher home, log path, interval and HOME filled in.
render_ghw_plist() {
    sed \
        -e "s|__TICK_PATH__|${GHW_HOME}/scripts/tick.sh|g" \
        -e "s|__WATCHER_HOME__|${GHW_HOME}|g" \
        -e "s|__LOG_PATH__|${GHW_HOME}/launchd.log|g" \
        -e "s|__INTERVAL__|${GHW_INTERVAL}|g" \
        -e "s|__HOME__|${HOME}|g" \
        "${GHW_SRC}/${GHW_PLIST_NAME}.plist"
}

# install_launch_agent LABEL TEMPLATE_CMD PLIST
#
# Writes what TEMPLATE_CMD (render_bridge_plist or render_ghw_plist) prints to
# PLIST, then loads the launchd agent LABEL, unloading it first when it is
# already loaded. A PLIST that is a symlink or read-only belongs to another
# manager, home-manager say: it is left alone, with no write and no launchctl
# call, and the agent keeps running as that manager configured it. Otherwise the
# steps are the ones the script always had, in the same order, so that a failed
# write stops the script (set -e) before any launchctl call.
install_launch_agent() {
    local label="$1" template_cmd="$2" plist="$3"
    local reason

    if reason="$(service_file_external "${plist}")"; then
        echo "[skip] ${label}: managed externally (${reason})"
        return 0
    fi

    "${template_cmd}" > "${plist}"
    ok "Plist installed at ${plist}"

    # grep reads to the end of the list, with no -q: one that quit at its first
    # match would leave launchctl to die of SIGPIPE when the list outgrows the
    # pipe, pipefail would then read the job as not loaded, and the unload would
    # be skipped.
    if launchctl list 2>/dev/null | grep -F -- "${label}" >/dev/null; then
        info "Reloading existing daemon..."
        launchctl unload "${plist}" 2>/dev/null || true
    fi
    launchctl load "${plist}"
    ok "${label} loaded"
}

# install_ghw_systemd_units UNIT_DIR
#
# Writes the GitHub watcher's systemd service and timer into UNIT_DIR from the
# templates in the checkout. The two files are one service, so when either is a
# symlink or read-only, and so belongs to another manager, each such file is
# reported and neither is written. Enabling the timer is left to the user.
install_ghw_systemd_units() {
    local unit reason
    local external=0

    for unit in agent-deck-gh-watcher.service agent-deck-gh-watcher.timer; do
        if reason="$(service_file_external "$1/${unit}")"; then
            echo "[skip] ${unit}: managed externally (${reason})"
            external=1
        fi
    done
    if [[ "${external}" -eq 1 ]]; then
        return 0
    fi

    for unit in agent-deck-gh-watcher.service agent-deck-gh-watcher.timer; do
        sed \
            -e "s|__TICK_PATH__|${GHW_HOME}/scripts/tick.sh|g" \
            -e "s|__WATCHER_HOME__|${GHW_HOME}|g" \
            -e "s|__LOG_PATH__|${GHW_HOME}/systemd.log|g" \
            -e "s|__INTERVAL__|${GHW_INTERVAL}|g" \
            "${GHW_SRC}/systemd/${unit}" > "$1/${unit}"
    done
    ok "gh-watcher systemd units written to $1"
    echo "  Enable with: systemctl --user daemon-reload && systemctl --user enable --now agent-deck-gh-watcher.timer"
}

# --------------------------------------------------------------------------
# Preflight checks
# --------------------------------------------------------------------------

command -v agent-deck >/dev/null 2>&1 || fail "agent-deck not found in PATH"
command -v python3 >/dev/null 2>&1 || fail "python3 not found in PATH"
command -v tmux >/dev/null 2>&1 || fail "tmux not found in PATH"

# --------------------------------------------------------------------------
# Step 1: Create conductor base directory
# --------------------------------------------------------------------------

info "Creating conductor directory: ${CONDUCTOR_DIR}"
mkdir -p "${CONDUCTOR_DIR}"

# Canonical source: internal/session/conductor_bridge.py (see conductor/README.md).
# Installed only while bridge.py is missing, identical, or matches .bridge.py.sha256.
install_bridge_script

# Copy default heartbeat rules (profiles can override with their own)
cp "${SCRIPT_DIR}/HEARTBEAT_RULES.md" "${CONDUCTOR_DIR}/HEARTBEAT_RULES.md"
ok "HEARTBEAT_RULES.md installed (default)"

# --------------------------------------------------------------------------
# Step 2: Install Python dependencies
# --------------------------------------------------------------------------

info "Installing Python dependencies..."
python3 -m pip install --quiet --user aiogram toml 2>/dev/null || {
    warn "pip install --user failed, trying without --user..."
    python3 -m pip install --quiet aiogram toml 2>/dev/null || {
        fail "Could not install Python dependencies (aiogram, toml). Install manually: pip3 install aiogram toml"
    }
}
ok "Python dependencies installed (aiogram, toml)"

# --------------------------------------------------------------------------
# Step 3: Configure Telegram bot
# --------------------------------------------------------------------------

# Check if [conductor] section already exists
if grep -q '^\[conductor\]' "${CONFIG_PATH}" 2>/dev/null; then
    info "[conductor] section already exists in config.toml"
    if grep -q 'token\s*=' "${CONFIG_PATH}" 2>/dev/null; then
        ok "Telegram config found, skipping prompt"
    else
        warn "No token found. You'll need to add it manually."
    fi
else
    echo ""
    echo -e "${CYAN}Telegram Bot Setup${NC}"
    echo "You need a Telegram bot token and your user ID."
    echo ""
    echo "1. Message @BotFather on Telegram -> /newbot -> copy the token"
    echo "2. Message @userinfobot on Telegram -> copy your user ID"
    echo ""

    read -rp "Telegram bot token: " TG_TOKEN
    read -rp "Your Telegram user ID: " TG_USER_ID

    if [[ -z "${TG_TOKEN}" || -z "${TG_USER_ID}" ]]; then
        fail "Both token and user ID are required"
    fi

    # Backup config before modifying
    cp "${CONFIG_PATH}" "${CONFIG_PATH}.backup-$(date +%Y%m%d-%H%M%S)"

    # Append conductor config
    cat >> "${CONFIG_PATH}" << TOML

# ============================================================================
# Conductor (Meta-Agent Orchestration)
# ============================================================================
# The conductor manages persistent Claude Code sessions that monitor all other
# sessions per profile, auto-respond when possible, and escalate via Telegram.

[conductor]
# The conductor system is active whenever a conductor exists on disk; there is
# no longer an `enabled` flag (#1361).
heartbeat_interval = 15
profiles = ["default", "work"]

[conductor.telegram]
token = "${TG_TOKEN}"
user_id = ${TG_USER_ID}
TOML

    ok "Added [conductor] section to config.toml"
fi

# --------------------------------------------------------------------------
# Step 4: Read profiles from config and set up per-profile directories
# --------------------------------------------------------------------------

# Extract profiles from config using Python (handles TOML parsing properly)
PROFILES=$(python3 -c "
import toml, sys
config = toml.load('${CONFIG_PATH}')
profiles = config.get('conductor', {}).get('profiles', ['default'])
print(' '.join(profiles))
" 2>/dev/null || echo "default")

info "Setting up conductor for profiles: ${PROFILES}"

for PROFILE in ${PROFILES}; do
    PROFILE_DIR="${CONDUCTOR_DIR}/${PROFILE}"
    SESSION_TITLE="conductor-${PROFILE}"

    info "Setting up profile: ${PROFILE}"
    mkdir -p "${PROFILE_DIR}"

    # Copy and substitute CLAUDE.md template
    if [[ -f "${PROFILE_DIR}/CLAUDE.md" ]]; then
        warn "  CLAUDE.md already exists for ${PROFILE}, backing up"
        cp "${PROFILE_DIR}/CLAUDE.md" "${PROFILE_DIR}/CLAUDE.md.bak"
    fi
    sed "s/{PROFILE}/${PROFILE}/g" "${SCRIPT_DIR}/conductor-claude.md" > "${PROFILE_DIR}/CLAUDE.md"
    ok "  CLAUDE.md installed for ${PROFILE} (profile substituted)"

    # Register conductor session in agent-deck for this profile
    info "  Registering conductor session: ${SESSION_TITLE} (profile: ${PROFILE})"

    if agent-deck -p "${PROFILE}" list --json 2>/dev/null | python3 -c "
import sys, json
data = json.load(sys.stdin)
sessions = data.get('sessions', data) if isinstance(data, dict) else data
found = any(s.get('title') == '${SESSION_TITLE}' for s in sessions)
sys.exit(0 if found else 1)
" 2>/dev/null; then
        ok "  Session ${SESSION_TITLE} already registered in ${PROFILE}"
    else
        register_session "${PROFILE}" "${PROFILE_DIR}" "${SESSION_TITLE}"
    fi
done

# --------------------------------------------------------------------------
# Step 5: Install launchd plist
# --------------------------------------------------------------------------

info "Installing launchd daemon..."
mkdir -p "${PLIST_DIR}"

# Resolve python3 path for plist
PYTHON3_PATH="$(command -v python3)"

install_launch_agent "${PLIST_NAME}" render_bridge_plist "${PLIST_PATH}"

# --------------------------------------------------------------------------
# Step 6: GitHub event watcher (opt-in, issue #2134)
# --------------------------------------------------------------------------
# Only acts when config.toml carries [conductor.github_watcher] enabled = true.
# Files go to ${CONDUCTOR_DIR}/gh-watcher; an existing watcher.toml is kept.

GHW_ENABLED=$(python3 -c "
import toml
cfg = toml.load('${CONFIG_PATH}').get('conductor', {}).get('github_watcher', {})
print('true' if cfg.get('enabled') else 'false')
" 2>/dev/null || echo "false")

if [[ "${GHW_ENABLED}" == "true" ]]; then
    GHW_SRC="${SCRIPT_DIR}/gh-watcher"
    GHW_HOME="${CONDUCTOR_DIR}/gh-watcher"
    info "GitHub watcher enabled in config; installing to ${GHW_HOME}"
    mkdir -p "${GHW_HOME}/scripts"
    cp "${GHW_SRC}"/scripts/* "${GHW_HOME}/scripts/"
    cp "${GHW_SRC}/schema.sql" "${GHW_SRC}/classes.json" "${GHW_SRC}/README.md" "${GHW_HOME}/"
    if [[ ! -f "${GHW_HOME}/watcher.toml" ]]; then
        cp "${GHW_SRC}/watcher.example.toml" "${GHW_HOME}/watcher.toml"
        warn "  Wrote ${GHW_HOME}/watcher.toml from the example; set [repo] and [routing] before the first tick"
    fi
    GHW_INTERVAL=$(python3 -c "
import toml
print(int(toml.load('${GHW_HOME}/watcher.toml').get('cadence', {}).get('events_seconds', 30)))
" 2>/dev/null || echo 30)

    if [[ "$(uname -s)" == "Darwin" ]]; then
        install_launch_agent "${GHW_PLIST_NAME}" render_ghw_plist "${GHW_PLIST_PATH}"
    else
        GHW_UNIT_DIR="${XDG_CONFIG_HOME:-${HOME}/.config}/systemd/user"
        mkdir -p "${GHW_UNIT_DIR}"
        install_ghw_systemd_units "${GHW_UNIT_DIR}"
    fi
    echo "  Mode and liveness: ${GHW_HOME}/scripts/status.sh (see ${GHW_HOME}/README.md)"
else
    info "GitHub watcher not enabled ([conductor.github_watcher] enabled = false); skipping"
fi

# --------------------------------------------------------------------------
# Done
# --------------------------------------------------------------------------

echo ""
echo -e "${GREEN}Conductor setup complete!${NC}"
echo ""
echo "Profiles configured: ${PROFILES}"
echo ""
echo "Next steps:"
for PROFILE in ${PROFILES}; do
    echo "  Start conductor:  agent-deck -p ${PROFILE} session start conductor-${PROFILE}"
done
echo ""
echo "  Test from Telegram:   Send /status to your bot"
echo "  View bridge logs:     tail -f ${CONDUCTOR_DIR}/bridge.log"
echo ""
echo "To stop: ./teardown.sh (or: launchctl unload ${PLIST_PATH})"
