# Safe `conductor setup` on an existing conductor installation: design

Date: 2026-10-31. Fork-only change (John, Q3). Branch `work/conductor-setup-fix`, base `0953876e`
(fork `main`, v1.16.34). obr: `agent-deck-z8i`.

## 1. Intent and the why

John is about to create two new Claude conductors on hera, `nix-steward` and `ingest-steward`, with
`agent-deck conductor setup <name> -agent claude ...` (steward spec, decision 13 and section 6.4:
`~/Products/agent-mail-conductor/docs/superpowers/specs/2026-10-31-agent-mail-conductors-design.md`).
Hera already runs a conductor installation that agent-deck did not fully create:

- `~/.local/share/agent-deck/conductor/bridge.py` is a hand-edited regular file (0755, 3041 lines,
  last edited 2026-10-31 10:43). It carries John's Discord Q&A hook. nix does not manage it.
  `director/DISCORD-QNA.md` says: "The bridge hook in `../bridge.py` and the monitor integration must
  be preserved if agent-deck regenerates this runtime directory."
- `~/Library/LaunchAgents/com.agentdeck.conductor-bridge.plist` and
  `com.agentdeck.transition-notifier.plist` are written by home-manager (`~/src/nix/config/agent-deck.nix`,
  `launchd.agents`) as read-only regular files (0444). `conductor/venv` is a home-manager symlink into
  `/nix/store` with no `pip` in it.

Running `conductor setup` today damages that installation. John chose to fix the fork rather than
work around it in the steward runbook. Running setup on hera must change nothing outside the new
conductor's own directory and session row. A fresh install must also carry the Q&A hook, so the
hook survives a future regeneration.

## 2. What the code does today (verified 2026-10-31 in this tree and on hera, read-only)

| Step | Where | Effect on hera |
|---|---|---|
| Python deps | `installPythonDeps`, `cmd/agent-deck/conductor_cmd.go:1708`, called at `:735` whenever a chat channel is configured (hera has Discord) | Runs `<conductor>/venv/bin/python3 -m pip install --user ...`, then without `--user`. The nix venv has no `pip`, so both fail, setup prints the pip remediation block, and `depsInstalled` is false. |
| bridge.py | `session.InstallBridgeScript`, `internal/session/conductor.go:2228`, called at `conductor_cmd.go:738` regardless of `depsInstalled` | `os.WriteFile` of the embedded bridge over John's file. The running bridge keeps the old code in memory until its next restart, then comes back without the hook. |
| Bridge agent | `installBridgeDaemonLaunchd`, `conductor.go:2779`, only when `depsInstalled` | `launchctl unload`, then `os.WriteFile` (fails on 0444), so `launchctl load` never runs and the agent stays unloaded. On hera the pip failure happens to skip this step; on a host whose venv has pip, it takes the bridge down. |
| Notifier agent | `installTransitionNotifierLaunchd`, `conductor.go:2885`, called on **every** setup at `conductor_cmd.go:773` | Same unload, failed write, no load. The only guard skips it when setup runs in the launchd Background domain (inside a conductor pane) and the notifier is running. From John's terminal the notifier is unloaded. |
| Heartbeat agent | `installHeartbeatDaemonLaunchd`, `conductor.go:3145`; uninstall at `:3274` | Same pattern, but only for the new conductor's own label; harmless for a new name, and the same bug if a heartbeat plist were ever externally managed. |

Siblings with the same bug class, found by grep:

- `agent-deck update` (`internal/update/update.go:1076` `UpdateBridgePy`, called from `cmd/agent-deck/main.go:4431`,
  `:4528` and the unattended path `update_cli.go:417`) backs up and overwrites `bridge.py`. It cannot run on hera
  (the nix binary cannot replace itself), but it clobbers any customized bridge elsewhere.
- `conductor migrate-dir` reinstalls `bridge.py` at the new base (`internal/session/conductor_migrate_dir.go:372`)
  and reloads the bridge agent if a plist exists (`conductor_cmd.go:1954`).
- `conductor teardown --all` unloads and deletes both agents' plists (`conductor_cmd.go:970-990`, via
  `uninstall*Launchd`), and with `--remove` deletes `bridge.py` (`:1139`).
- The systemd twins (`installBridgeDaemonSystemd`, `installTransitionNotifierSystemd`, heartbeat units) overwrite
  unit files that home-manager on Linux would provide as symlinks into `/nix/store`.
- The checkout installer `conductor/setup.sh` copies the canonical bridge over `bridge.py` (line 95) and unloads,
  regenerates and reloads the bridge plist (lines 216-240) and the gh-watcher plist (lines 276-283);
  `conductor/teardown.sh` unloads and deletes the bridge and gh-watcher plists. Both are maintained:
  `conductor/setup_register_test.go` tests `setup.sh`, and `docs/WATCHER-SETUP.md` tells users to rerun it.
- `conductor migrate-dir` moves every top-level entry of the conductor directory (bridge, venv, conductors) and
  repoints `[conductor].dir`. An externally managed bridge agent hard-codes the old paths: hera's nix module bakes
  `<base>/venv/bin/python3`, `<base>/bridge.py`, `AGENT_DECK_CONDUCTOR_DIR=<base>` and a KeepAlive watch on
  `<base>/bridge.py` into its plist. A move would strand it.

The bridge reads `config.toml` once, at startup (`conductor_bridge.py:5326`), and never reloads `bridge.py`. On an
install agent-deck owns, the reload that setup does today is what makes an upgraded bridge or a newly added channel
take effect.

Already safe, no change: teardown of one named conductor (it removes only that conductor's directory, row and
heartbeat); the shared `CLAUDE.md`/`AGENTS.md` (`writeGeneratedFileOrMigrate`: replaced only when an exact copy of an
earlier generated version), `POLICY.md` (written only if absent), `LEARNINGS.md` (only if absent), the director's
custom `heartbeat.sh` (no managed-script markers, so `MigrateConductorHeartbeatScripts` skips it).
`RebootstrapLaunchAgents` after an update touches only agents whose program is the agent-deck binary; the nix
notifier's program is `/bin/sh`, so it is skipped.

## 3. Goals, non-goals, anti-goals

Goals:

1. `conductor setup <new name>` on hera leaves `bridge.py` byte-identical, never runs a mutating `launchctl`
   command against `com.agentdeck.conductor-bridge` or `com.agentdeck.transition-notifier`, does not try to
   install Python packages, and changes nothing outside `conductor/<new name>/`, the new session row, and the new
   conductor's own heartbeat plist (when heartbeat is on).
2. The same rules hold for every path that writes the bridge or touches those agents: `update`, `migrate-dir`,
   `teardown --all [--remove]`, the checkout scripts `conductor/setup.sh` and `teardown.sh`, on launchd and
   systemd alike.
3. Installs agent-deck fully owns keep working as before. Bridges agent-deck wrote keep being upgraded. Agents
   whose plists agent-deck wrote are regenerated and reloaded on every setup run, as today, so an upgraded bridge
   or a newly added channel takes effect.
4. The embedded bridge carries the Discord Q&A hook, behaving as hera's does, so a fresh install has it.

Non-goals: the nix-config bump and hera's activation (the `nix` and `pi` sessions); the steward plan and its own
bridge patch (steward plan Task 11); shipping `discord_qna.py` (it needs the director's `monitor_sessions.py`,
`notify_discord.py` and SSH config, none of which agent-deck ships); changes to session or inbox code; the
`teardown --all --remove` deletion of the shared `CLAUDE.md`, `POLICY.md` and so on (a different question:
those are template files the user asked to remove); an upstream PR (John, Q3).

Anti-goals, which would count as failure even if the acceptance passed:

- Replacing a read-only plist by rename. An atomic temp-file-and-rename succeeds on a 0444 file in a writable
  directory and would silently replace nix's plist, which is worse than today's failed write.
- Keying ownership on nix-specific content (`/bin/wait4path`, `/nix/store` paths). Ownership comes from what the
  file is (read-only or a symlink), or from what agent-deck recorded writing.
- Any change to `cmd/agent-deck/main.go`, `cmd/agent-deck/version_capabilities.go` or
  `internal/session/remote_channel.go`. nix-config applies `overlays/ai/agent-deck-legacy-remote-capability.patch`
  to them, and this was promised to `nix` by mail on 2026-10-31.
- Setup "succeeding" while leaving an agent unloaded. Nothing is unloaded until its replacement is known to be
  writable.

## 4. Decisions (John, 2026-10-31, in this session)

- **Q1, bridge.py: stamp what agent-deck wrote.** Each time agent-deck writes `bridge.py`, it records the
  file's sha256 in a hidden stamp file beside it. Setup, update and migrate-dir later replace the bridge only
  when it is missing or still matches its stamp. Otherwise they leave it and say how to take the shipped version
  (move yours aside and rerun). A bridge written before this fix has no stamp and counts as customized until it
  is moved aside once. Hera needs exactly this.
- **Q2, plists and units: read-only or symlink means external.** A launchd plist or systemd unit that is
  read-only or a symlink belongs to something else (how "read-only" is tested: section 5.2). Setup, migrate-dir and
  teardown never unload, rewrite, reload or remove it, and they report `[skip] <label>: managed externally`.
  Also stated with the question: no unload before the replacement is known to be writable. The question also
  offered to skip an agent whose plist already matches. **[my call, withdrawn after the builder check]** That skip
  would stop an owned bridge from picking up an upgraded `bridge.py` or a new channel, because the bridge reads
  both only at startup. Owned agents are therefore reloaded on every run, as today.
- **Q3, landing: fork only.** Both parts are fork commits carried in John's rebase series. No upstream PR, so no
  PR body, PR-gate self-check or repro report. The repository's code and test conventions still apply.
- **Q4, Q&A router path: as on hera.** The embedded bridge looks for `<conductor dir>/director/discord_qna.py`;
  when the file is missing the hook is inert.
- **Q5, acceptance: sandbox, then day zero.** A sandbox on hera (section 7) runs before and after the fix. The
  live proof is the steward plan's own day-zero check ("`bridge.py` is unchanged after setup and the Q&A buttons
  still work"). There is no separate live run.

## 5. The design

### 5.1 bridge.py ownership (Q1)

The stamp is `<conductor dir>/.bridge.py.sha256`, holding the lower-case hex sha256 of the bytes agent-deck
wrote, then a newline **[my call: name and format]**.

A write of the embedded bridge sorts the target into one case:

| Target | Action | Reported as |
|---|---|---|
| missing | write it, then stamp | installed |
| a regular file equal to the embedded bytes | stamp only (heals a missing or stale stamp) | unchanged |
| a regular file whose sha256 matches the stamp | replace, then restamp | updated |
| a regular file with no stamp, or not matching it | leave it | kept (local changes) |
| a symlink | leave it | kept (external) |
| anything else (directory, FIFO) | error, as `writeGeneratedFileOrMigrate` does today | error |

- The replacement goes through the code's existing machinery for generated files. **[my call]** Generalize
  `writeGeneratedFileOrMigrate` (`conductor.go:1779`) so the "may I replace this content" test is a predicate,
  and have it report what it did. The current callers pass "matches an earlier template generation"; the bridge
  passes "sha256 equals the stamp". That keeps the atomic exchange and the displaced-file recheck that protect a
  concurrent edit, and avoids a second, parallel writer.
- The displaced copy. The helper deliberately never unlinks the file it displaces (`conductor.go:1849`), since an
  editor may still be writing through an open descriptor, and leaves it as `.<name>.previous-XXXXXX`. For the
  rare `CLAUDE.md` migration that is cheap. The bridge is 226 KB and changed in 6 of the last 12 releases, so it
  would pile up. **[my call]** The generalized helper takes a destination for the verified displaced copy.
  Existing callers keep today's behaviour. The bridge renames its verified displaced copy to one fixed hidden name,
  `.bridge.py.previous`, replacing the previous one. A rename keeps the inode, so an open editor still writes into
  the kept copy, and only this code ever writes that name, so what it replaces is always agent-deck's own earlier
  bytes. A displaced copy that fails the recheck (a concurrent edit) stays where the helper leaves it today, and
  the error names it.
- `UpdateBridgePy` stops writing `bridge.py.backup`, since the installer now keeps the previous version itself and
  only when it replaces the bridge. An existing `bridge.py.backup` is never touched or deleted: on an install
  that a pre-fix update overwrote, it may hold the user's own bridge.
- Stamping comes after the bridge write. A crash between the two heals on the next run, because the file then
  equals the embedded bytes.
- The "kept" message names the file and the remedy:
  `[skip] bridge.py has local changes; left unchanged. To take the shipped bridge, move it aside and re-run 'agent-deck conductor setup <name>'.`
  (and the `update` / `migrate-dir` equivalents).
- `InstallBridgeScript() error` keeps its signature, because `main.go:73` injects it into `update` and
  `main.go` must not change. The kept case returns an error value that satisfies a small interface such as
  `BridgeKept() bool`. `UpdateBridgePy` (`internal/update/update.go`, which may not import `session`) checks for
  it with `errors.As`, prints the "left unchanged" line, and returns nil. Setup and migrate-dir call a variant
  that returns the outcome directly. **[my call: exact names to the builder]**
- `migrate-dir`: the stamp and `.bridge.py.previous` move with the directory, so a stamped bridge stays
  upgradable at the new base. Whether the existing move carries hidden files is for the builder to check; if it
  does not, they are moved explicitly.
- `teardown --all --remove`: deletes `bridge.py`, the stamp and `.bridge.py.previous` only when the bridge is
  stamped-unmodified or equals the embedded bytes; otherwise it keeps all three and says so. It never deletes
  `bridge.py.backup`, as today **[my call: same bug class as the overwrite; the neighbouring template deletions
  are a non-goal]**.

### 5.2 Launch agents and systemd units (Q2)

One helper decides ownership for every agent-deck plist and unit (bridge, notifier, per-conductor heartbeat):

- **External** when `Lstat` shows a symlink, or a regular file this user cannot write, checked with `access(2)`
  `W_OK`. Hera's 0444 files and a root-owned 0644 file both count. **[my call: access(2) rather than the
  owner-write bit, which a file owned by another user would pass]**
- A missing file, or a regular file this user can write, is agent-deck's.

Install, for an agent-deck-owned path: today's sequence, reordered so nothing is unloaded first.

1. Generate the content and write it to a temp file in the target's directory. If that fails, return the error
   with nothing unloaded. Keep the old file's bytes in memory.
2. launchd: unload the agent, rename the temp file over the target, load it. systemd: rename the temp file over
   the unit, then `systemctl --user enable --now` exactly as today, adding no stop, so the two platforms keep
   today's behaviour.
3. If the rename fails after the unload (the old file is still in place, for example because of an immutable
   flag), load the old plist again. If the load of the new plist fails, put the old bytes back (temp file, then
   rename) and load them. Either way the setup warning names the agent and what failed. Exit codes stay as
   today: a daemon failure is a warning and setup exits 0. If even the old plist will not load, the warning gives
   the manual `launchctl load <path>`. **[my call]**

There is no skip for an unchanged plist on an owned agent: it is reloaded on every run, as today (section 4).

Install, for an external path: no launchctl or systemctl command at all, no write, and
`[skip] <label>: managed externally (<read-only|symlink>)`. This is not a warning. Callers must not print
"[ok] ... loaded" or "Warning: failed".

Uninstall (`teardown --all`, heartbeat disable): an external path is neither unloaded nor removed, and is
reported the same way.

- All launchd calls in these paths go through the existing `runLaunchctl` seam (`conductor.go:2860`), and
  systemd calls through an equivalent one. That way the tests can prove no mutating call happens. Today the
  bridge and heartbeat paths call `exec.Command("launchctl", ...)` directly.
- The notifier's Background-domain guard stays as it is. It applies after the external check.
- Setup runs `installPythonDeps` only when it is going to install the bridge agent. If the bridge plist or unit
  is external, the agent's manager owns its interpreter: setup skips pip and prints
  `[skip] bridge daemon managed externally; Python deps left to its manager`. **[my call]**
- `--json`: setup adds `"bridge_script": "installed|updated|unchanged|kept"` and `"external": [<labels>]`, and
  keeps the existing keys. `"daemon"` and `"notifier_daemon"` are set only when agent-deck installed them.
  **[my call]**
- `migrate-dir` refuses to move the directory when the bridge agent, or the heartbeat agent of a conductor being
  moved, is external. Their plists or units hard-code the current paths, and agent-deck may not rewrite them. The
  refusal is a blocker in the migration plan, so dry-run reports it and `--apply` stops before moving anything:
  `com.agentdeck.conductor-bridge is managed externally and points at <old dir>; change its manager's
  configuration first`. The notifier does not reference the conductor directory and does not block.
  **[my call]**

### 5.3 The Discord Q&A hook in the embedded bridge (Q4)

The source for the port is a frozen, read-only copy of hera's files taken on 2026-10-31 at 22:16 PDT. The steward
plan's Task 11 will later patch the live `bridge.py`'s reply path, so the live file is not the reference:

| Copy under `~/Products/agent-deck-conductor-setup-fix/reference/` | sha256 |
|---|---|
| `hera-bridge.py` (the 10:43 bridge; hook at lines 2466-2537) | `47e7629d26b186708843ddf1b85cd84336aa55a6df778e8b1e7bd9fe7533d5e0` |
| `hera-discord_qna.py` (the router; it reads `reference`, `message`, `author`, `channel`, `text`, `custom_id`) | `19ca576ec584578f6876ae5973311e7b03dbdaf43aae639af17148d8e479d6df` |
| `hera-DISCORD-QNA.md` (the director's notes on the feature) | `61d61a54582419021347f8063010d48033981ceab313a7aed138f02a173e37bc` |

The events the router receives, exactly as hera builds them. Every value is a string:

- button: `{"reference": str(interaction.message.id), "message": str(interaction.id), "author": str(interaction.user.id), "channel": str(interaction.channel_id), "text": <the choice digits from the custom id>, "custom_id": <the custom id>}`
- reply: `{"reference": str(message.reference.message_id), "message": str(message.id), "author": str(message.author.id), "channel": str(message.channel.id), "text": message.content}` (the raw content, with mentions not stripped)

Port hera's three pieces into `internal/session/conductor_bridge.py`, faithfully:

1. `route_question(event)`: run `[sys.executable, <CONDUCTOR_DIR>/director/discord_qna.py]` with the event as
   JSON on stdin, `timeout=90`, capture output. A non-zero exit raises. The result is the parsed stdout
   `{"handled": bool, "response": str}`. The router path is a module-level helper, so tests can point it at a
   fixture.
2. `on_interaction`: only component interactions whose `custom_id` starts with `qna:`. Wrong channel or an
   unauthorized user gets an ephemeral "Not authorized to answer this question." A `custom_id` that does not
   fully match `qna:([a-f0-9]{12}):([0-9]{1,2})`, or a missing message, gets "Invalid question button. No input
   sent." Otherwise defer (ephemeral, thinking), build the button event, and route it in a thread. A handled
   result's `response` is shown. An unhandled result shows "Question record unavailable. No input sent." A routing
   exception shows "Answer outcome unconfirmed. Check the session locally; do not retry automatically." The
   original response is edited with mentions disabled. As on hera, this path does not check that the router
   exists: buttons exist only on posts the router published, and a missing router fails as an exception.
3. Reply routing in `on_message`: a message in the target channel from an authorized user that replies to
   another message, when the router file exists, is routed **before** the mention filter and the
   reply-to-others filter, so it works in every listen mode, as on hera. Handled: reply with the response
   (mentions disabled) and stop. Not handled: fall through to normal handling. Exception: reply "Reply routing
   unavailable; nothing forwarded. Please check the session locally." and stop. A routing failure is never
   forwarded to a conductor.

The texts above are hera's, verbatim. Placing the reply route ahead of the mention filter adapts hera's order
(channel, auth, reply route, mention filter) to the embedded bridge's order (channel, mention filter, auth). A
comment block in the bridge states the router contract (stdin event, stdout result) and that the router is not
shipped. No user-doc change, since nothing a user can install depends on it **[my call]**.

### 5.4 The checkout installer scripts (sibling)

`conductor/setup.sh` and `conductor/teardown.sh` follow the same two rules. **[my call: fixed in this series, as
AGENTS.md's sibling rule asks, rather than listed]**

- `setup.sh` installs the canonical bridge only when `bridge.py` is missing, is byte-identical to the canonical
  file (`cmp -s`), or has a `shasum -a 256` equal to `.bridge.py.sha256`. It then writes the stamp in the Go
  side's format and keeps the displaced bridge as `.bridge.py.previous`. Otherwise it prints the same skip line.
- `setup.sh` and `teardown.sh` leave a bridge or gh-watcher plist that is a symlink or not writable (`[ -L ]`, or
  `[ -e ]` and `[ ! -w ]`) alone: no unload, regenerate, load or delete, just a `[skip] ... managed externally`
  line. Everything else keeps today's steps.

## 6. Constraints

- Tests run only hermetically: `bash scripts/test.sh <go test args>` (temp HOME, TMPDIR, TMUX_TMPDIR) or
  `make verify-docker`, never a bare `go test` on hera. Bridge tests: pytest on `conductor/tests/`, which loads
  the embedded bridge (`conductor/tests/conftest.py`).
- Go 1.26.10: `nix shell nixpkgs#go_1_26 -c ...` downloads it through `GOTOOLCHAIN` (verified 2026-10-31).
  Then `gofmt -l .` is empty and `go vet ./...` passes.
- Regression tests fail first on the unfixed code.
- Fix the siblings (section 2) in the same series.
- Build products and scratch go under `~/Products/agent-deck-conductor-setup-fix/`, never `/tmp` or the tree.
- Nothing is pushed, merged or activated without John's word in this session.

## 7. Acceptance (Q5)

Automated, in the series' tests:

- Bridge ownership: every row of the table in 5.1, for setup, update and migrate-dir. A replacement leaves exactly
  one `.bridge.py.previous`, holding the previous bytes. update writes no `bridge.py.backup` and leaves an
  existing one untouched. teardown `--all --remove` keeps a customized bridge and never deletes
  `bridge.py.backup`.
- Plists and units: external (read-only, root-owned, symlink) means zero `runLaunchctl` or systemctl calls and the
  file is byte-identical, for install and uninstall of the bridge, notifier and heartbeat. For an owned plist:
  temp write, unload, rename, load in that order, every run. A failed temp write makes no unload. A failed rename
  reloads the old plist. A failed load restores the old bytes and loads them. systemd adds no stop.
- migrate-dir with an external bridge or heartbeat agent: dry-run reports the blocker, and `--apply` refuses with
  nothing moved and the config unchanged.
- Setup end to end, with Discord configured and both plists read-only: no pip invocation, bridge byte-identical,
  `[skip]` lines, exit 0.
- `setup.sh` and `teardown.sh`: a customized bridge and a read-only plist are left alone. Tested the way
  `conductor/setup_register_test.go` tests `setup.sh`, with a fake `launchctl` on `PATH`.
- Q&A hook (pytest): `route_question` against fixture routers (handled, unhandled, non-zero exit, bad JSON,
  timeout); the custom-id pattern; the interaction and reply paths with fake Discord objects, covering
  authorization, wrong channel, invalid id, handled, unhandled (fall-through for replies, "Question record
  unavailable" for buttons) and router failure. A recording router checks that the event's keys and string
  values equal the shapes in 5.3.

The sandbox on hera, run once with the installed v1.16.34 binary and once with the fixed build:

- `HOME=~/Products/agent-deck-conductor-setup-fix/sandbox/home`, holding copies of `~/.config/agent-deck/config.toml`
  (0600; it holds the Discord token, and the sandbox is deleted afterwards), `~/.local/share/agent-deck/conductor/`
  (with the `venv` symlink kept as a symlink) and both 0444 plists. Also a private `TMUX_TMPDIR`, with `TMUX`,
  `TMUX_PANE`, `AGENTDECK_*`, `CLAUDE_CONFIG_DIR` and `XDG_*` unset.
- A `launchctl` shim first on `PATH` logs every call. It passes `list` and `print` to `/bin/launchctl`, answers
  `managername` with `Aqua`, and refuses (exit 1) `load`, `unload`, `bootstrap`, `bootout`, `kickstart`, `enable`
  and `disable`. A broken build therefore cannot touch the live agents. The `Aqua` answer makes the run behave as
  from John's terminal: an agent session such as this one runs in the `Background` domain (checked 2026-10-31),
  where the notifier guard would skip the notifier and hide the bug.
- Run `agent-deck conductor setup steward-probe -agent claude -no-heartbeat </dev/null`, then
  `agent-deck conductor teardown steward-probe --remove`. Hash every file of the sandbox before and after, and
  record the shim log.
- Expected before the fix: the shim log shows a refused `unload` of the notifier plist and no `load` (the write
  fails first, on the 0444 copy), the copy of `bridge.py` differs from the original, and setup prints the pip
  block. Expected after: no refused call in the shim log; `bridge.py` and both plists byte-identical. Every other
  file outside `conductor/steward-probe/` is identical too, except changes the unfixed run also makes to files this
  fix does not cover (for example, the shared `CLAUDE.md` migration if hera's copy is an unmodified older
  template). Each such change is listed in the evidence. After teardown, everything except agent-deck's own state
  database and logs is back to its post-setup state, minus `conductor/steward-probe/`. Real `launchctl list`
  shows both live agents still loaded (they were never reachable).
- Real-router round trip, after the fix, in the same sandbox: call the built tree's `route_question` with the
  sandbox venv's Python and `AGENT_DECK_CONDUCTOR_DIR` set to the sandbox conductor copy. The event is a button
  event whose `reference` names no stored question. The copy of `discord_qna.py` then reads only the sandbox
  `config.toml` and its SQLite copy, sends nothing to Discord, writes no log, and answers `{"handled": false}`.
  This checks the hook's path, interpreter, stdin and stdout contract against the real router. A handled event
  is never used: it would make the router edit Discord posts.

What stays unproven: nothing runs the embedded hook against a live Discord question until hera runs the embedded
bridge, and nothing in this work plans that switch.

## 8. Interactions with other sessions' work

- `nix` (mailed 2026-10-31): once John has pushed the commits to `jwiegley/agent-deck` `main`, it bumps the pin
  in `sources/ai.json`, and `pi` activates hera. No other nix-config change.
- The steward plan's Task 11 patches hera's live `bridge.py` (case-insensitive prefixes, reply-to-page routing).
  This fix is what keeps that patch alive through `conductor setup`. The embedded bridge does not get Task 11's
  routing in this series. If hera ever moves to the embedded bridge, that patch has to be ported first.
- The steward runbook passes `-shared-instructions-md`, which replaces `conductor/CLAUDE.md` with a symlink
  (`createSymlinkWithExpansion` removes the existing file, with no backup). That is intended by the steward
  design. The runbook may want to copy hera's current `CLAUDE.md` aside first; I'll pass that to `mail-conductor`.

## 9. Left to the builder

The exact Go names and signatures; how the generated-file helper is generalized, as long as the existing callers'
behaviour is unchanged; the systemd seam's shape, and treating a heartbeat service and timer as one pair; locking
around the bridge write and its stamp; the test files (match the neighbours: `conductor_clobber_test.go`,
`conductor_bg_guard_test.go`, `conductor_setup_agent_test.go`, `conductor/setup_register_test.go`, and the
existing bridge tests); the sandbox script itself (kept under `~/Products`, not in the tree).
