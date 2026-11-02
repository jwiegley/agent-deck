#!/usr/bin/env bash
set -euo pipefail

# Conductor Teardown Script (Multi-Profile)
# Stops the bridge daemon and per-profile conductor sessions.

AGENT_DECK_DIR="${HOME}/.agent-deck"
CONDUCTOR_DIR="${AGENT_DECK_DIR}/conductor"
CONFIG_PATH="${AGENT_DECK_DIR}/config.toml"
PLIST_NAME="com.agentdeck.conductor-bridge"
PLIST_PATH="${HOME}/Library/LaunchAgents/${PLIST_NAME}.plist"
GHW_PLIST_PATH="${HOME}/Library/LaunchAgents/com.agentdeck.gh-watcher.plist"
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

# sha256_of FILE
#
# Prints FILE's sha256 as 64 lower-case hex digits and a newline: the format of
# the .bridge.py.sha256 stamp that agent-deck keeps beside a bridge it wrote.
# FILE goes in on stdin, so that the line the tool prints carries no file name:
# GNU sha256sum and shasum start that line with a backslash when the name holds
# a backslash or a newline, and the hash would no longer be the first field.
# setup.sh carries an identical copy; keep the two the same.
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
# BSD and GNU stat differ. setup.sh carries an identical copy.
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

# remove_bridge_script
#
# Removes ${CONDUCTOR_DIR}/bridge.py, its stamp (.bridge.py.sha256) and
# .bridge.py.previous, but only when agent-deck owns the bridge, by the rule of
# the Go RemoveBridgeScript: bridge.py equals the checkout's canonical bridge
# (internal/session/conductor_bridge.py) or still matches its stamp. A symlinked
# bridge.py is never removed, and bridge.py.backup, which may hold the user's own
# bridge, is never touched. Without the checkout the comparison with the
# canonical file fails, and only the stamp rule applies.
remove_bridge_script() {
    local src="${SCRIPT_DIR}/../internal/session/conductor_bridge.py"
    local dest="${CONDUCTOR_DIR}/bridge.py"
    local stamp="${CONDUCTOR_DIR}/.bridge.py.sha256"
    local owned=""

    if [[ -L "${dest}" ]]; then
        echo "[skip] bridge.py is a symlink; kept"
        return 0
    fi
    if [[ ! -e "${dest}" ]]; then
        return 0
    fi
    # The stamp matches when it is a regular file whose bytes, newline included,
    # are the sha256 of bridge.py, as the Go installer compares them.
    if cmp -s "${src}" "${dest}"; then
        owned="yes"
    elif [[ -f "${stamp}" && ! -L "${stamp}" ]] && sha256_of "${dest}" | cmp -s - "${stamp}"; then
        owned="yes"
    fi
    if [[ -z "${owned}" ]]; then
        echo "[skip] bridge.py has local changes; kept"
        return 0
    fi
    rm -f "${dest}" "${stamp}" "${CONDUCTOR_DIR}/.bridge.py.previous"
}

# remove_launch_agent LABEL PLIST
#
# Unloads the launchd agent LABEL and deletes its PLIST. A PLIST that is a
# symlink or read-only belongs to another manager, home-manager say: it is
# neither unloaded nor deleted.
remove_launch_agent() {
    local label="$1" plist="$2"
    local reason

    if reason="$(service_file_external "${plist}")"; then
        echo "[skip] ${label}: managed externally (${reason})"
        return 0
    fi
    if [[ ! -f "${plist}" ]]; then
        info "No ${label} plist found (already removed)"
        return 0
    fi

    info "Stopping ${label}..."
    launchctl unload "${plist}" 2>/dev/null || true
    rm -f "${plist}"
    ok "${label} stopped and plist removed"
}

# --------------------------------------------------------------------------
# Step 1: Stop launchd daemon
# --------------------------------------------------------------------------

remove_launch_agent "${PLIST_NAME}" "${PLIST_PATH}"

# Opt-in GitHub watcher unit (issue #2134); present only when it was enabled.
if GHW_REASON="$(service_file_external "${GHW_PLIST_PATH}")"; then
    echo "[skip] com.agentdeck.gh-watcher: managed externally (${GHW_REASON})"
elif [[ -f "${GHW_PLIST_PATH}" ]]; then
    launchctl unload "${GHW_PLIST_PATH}" 2>/dev/null || true
    /usr/bin/trash "${GHW_PLIST_PATH}" 2>/dev/null || true
    ok "gh-watcher unit stopped and plist moved to Trash"
fi

# --------------------------------------------------------------------------
# Step 2: Read profiles and stop conductor sessions
# --------------------------------------------------------------------------

# Extract profiles from config (fall back to default if parsing fails)
PROFILES="default"
if command -v python3 >/dev/null 2>&1 && [[ -f "${CONFIG_PATH}" ]]; then
    PROFILES=$(python3 -c "
import toml, sys
config = toml.load('${CONFIG_PATH}')
profiles = config.get('conductor', {}).get('profiles', ['default'])
print(' '.join(profiles))
" 2>/dev/null || echo "default")
fi

if command -v agent-deck >/dev/null 2>&1; then
    for PROFILE in ${PROFILES}; do
        SESSION_TITLE="conductor-${PROFILE}"
        info "Stopping conductor session: ${SESSION_TITLE} (profile: ${PROFILE})"
        agent-deck -p "${PROFILE}" session stop "${SESSION_TITLE}" 2>/dev/null || true
        ok "  ${SESSION_TITLE} stopped"
    done
fi

# --------------------------------------------------------------------------
# Step 3: Ask about cleanup
# --------------------------------------------------------------------------

echo ""
echo -e "${YELLOW}Optional cleanup:${NC}"
echo "  1. Remove conductor directories (per-profile dirs in ${CONDUCTOR_DIR})"
echo "  2. Remove [conductor] section from config.toml"
echo "  3. Remove conductor sessions from agent-deck"
echo ""
read -rp "Remove conductor directories? (y/N): " REMOVE_DIR
read -rp "Remove conductor sessions from agent-deck? (y/N): " REMOVE_SESSION

if [[ "${REMOVE_DIR}" =~ ^[Yy] ]]; then
    for PROFILE in ${PROFILES}; do
        PROFILE_DIR="${CONDUCTOR_DIR}/${PROFILE}"
        if [[ -d "${PROFILE_DIR}" ]]; then
            rm -rf "${PROFILE_DIR}"
            ok "Removed ${PROFILE_DIR}"
        fi
    done
    # bridge.py goes only when it is the checkout's canonical file or matches its stamp
    remove_bridge_script
    rm -f "${CONDUCTOR_DIR}/bridge.log"
    # Remove conductor dir if empty
    rmdir "${CONDUCTOR_DIR}" 2>/dev/null && ok "Removed empty ${CONDUCTOR_DIR}" || true
else
    info "Conductor directories kept at ${CONDUCTOR_DIR}"
fi

if [[ "${REMOVE_SESSION}" =~ ^[Yy] ]]; then
    for PROFILE in ${PROFILES}; do
        SESSION_TITLE="conductor-${PROFILE}"
        agent-deck -p "${PROFILE}" remove "${SESSION_TITLE}" 2>/dev/null && \
            ok "Removed session ${SESSION_TITLE} from ${PROFILE}" || \
            warn "Could not remove ${SESSION_TITLE} (remove manually from TUI)"
    done
fi

echo ""
echo -e "${GREEN}Teardown complete.${NC}"
echo ""
echo "Note: The [conductor] section in config.toml was NOT removed."
echo "Edit ${AGENT_DECK_DIR}/config.toml manually if you want to remove it."
