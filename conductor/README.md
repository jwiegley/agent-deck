# conductor/

Runtime assets and helper scripts for the conductor feature (Telegram / Slack /
Discord ↔ agent-deck conductor sessions).

## Where is `bridge.py`?

The conductor bridge script has **exactly one canonical source** in the repo:

```
internal/session/conductor_bridge.py
```

It is embedded into the agent-deck binary (`//go:embed`, see
`internal/session/conductor_bridge_embed.go`). `agent-deck conductor setup`,
`agent-deck update` and `agent-deck conductor migrate-dir` write it only while
the runtime copy is missing, byte-identical to the embedded bridge, or unchanged
since agent-deck stamped it: the sha256 of each copy written goes to
`.bridge.py.sha256` beside it, and the copy it replaces is kept as
`.bridge.py.previous`. A customized or symlinked `bridge.py` is kept, as is an
unedited copy from a release that predates the stamp; move it aside and re-run
`agent-deck conductor setup <name>` to take the shipped bridge.
`conductor/setup.sh` applies the same rule to the copy it writes under
`~/.agent-deck/conductor` (its kept line names `conductor/setup.sh` as the
remedy), and `conductor/teardown.sh` removes only a bridge that rule would
replace. The copy agent-deck writes lives at:

```
<data-dir>/conductor/bridge.py        # e.g. ~/.local/share/agent-deck/conductor/bridge.py
```

So there is **no `conductor/bridge.py` checked into the repo** — editing the
embedded canonical file is the only place to change the bridge. Tests under
`conductor/tests/` load that same canonical file (see
`conductor/tests/conftest.py`), so the tested bytes are exactly the deployed
bytes.

## Stage events

Per-PR pipeline timing is recorded as JSONL, one line per stage transition,
instead of per-merge receipts and metrics prose. The schema, the
`scripts/stage-event.sh` recorder and the `scripts/stage-report.py` summary
(median and p90 per transition, maintainer versus external) are documented in
[STAGE-EVENTS.md](STAGE-EVENTS.md).
