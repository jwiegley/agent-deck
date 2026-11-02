package session

import _ "embed"

// conductorBridgePy is the Python bridge script that connects Telegram, Slack,
// and/or Discord to conductor sessions. It is embedded so the binary is
// self-contained: setup, update and migrate-dir write these exact bytes to
// <data>/conductor/bridge.py only while that file is missing, byte-identical,
// or unchanged since agent-deck stamped it (.bridge.py.sha256); a customized or
// symlinked bridge.py is kept (see conductor_bridge_script.go).
//
// There is EXACTLY ONE canonical copy of this script in the repo —
// internal/session/conductor_bridge.py, embedded directly below. No mirror and
// no go:generate sync step: the embedded bytes ARE the canonical file, so they
// cannot drift. conductor/tests load this same file (see conductor/tests/
// conftest.py), and conductor/README points here for discoverability.
//
//go:embed conductor_bridge.py
var conductorBridgePy string
