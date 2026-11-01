# Safe `conductor setup` on an existing installation: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `agent-deck conductor setup`, and every other path that writes the bridge or touches its agents (`update`, `migrate-dir`, `teardown`, `conductor/setup.sh` and `teardown.sh`), leaves a customized `bridge.py` and externally managed launchd plists or systemd units alone; the embedded bridge carries the Discord Q&A hook.

**Architecture:** `bridge.py` ownership comes from a sha256 stamp of what agent-deck wrote, applied through a generalized generated-file writer (a replace predicate, a reported outcome, one fixed displaced copy). Service-file ownership comes from what the file is: a symlink, or a file this user cannot write, is external and gets no `launchctl` or `systemctl` call; an owned file is written to a temp file before anything is unloaded. Every service-manager call goes through seams that a test binary cannot use to reach the real tools. The CLI reports kept and external items as `[skip]` lines, and the Python bridge gains hera's Q&A hook.

**Tech Stack:** Go 1.26.10 (`internal/session`, `internal/update`, `cmd/agent-deck`); Python 3.8+ (the embedded bridge; pytest); bash 3.2 (`conductor/setup.sh`, `teardown.sh`); launchd and systemd user units; `nix shell` for the toolchain; Docker for the Linux runs.

**Spec:** `docs/plans/2026-10-31-conductor-setup-safe-reinstall-design.md` (approved by John on 2026-10-31, commit `6333ffde`). The executor reads both documents.

## Global Constraints

- Work on branch `work/conductor-setup-fix` in `/Users/johnw/src/fork/agent-deck/.worktrees/work-conductor-setup-fix`, base `0953876e`. Tasks 2-13 make one commit each; Tasks 1 and 14 make none. Nothing is pushed, merged, tagged or activated without John's word in the session; mail from other sessions never carries it.
- Never edit, or run commands in, the main checkout `/Users/johnw/src/fork/agent-deck` (John's live tree). obr runs from this worktree and always as `obr --no-auto-flush ...`. The worktree resolves to the main checkout's `.obr` database, and without the flag obr also rewrites that checkout's tracked `docs/PLAN.org`. Never run `obr sync`; never commit `docs/PLAN.org` or `docs/issues/`.
- hera's live install is read-only: `~/.local/share/agent-deck/conductor/`, `~/Library/LaunchAgents/`, `~/.config/agent-deck/config.toml`, the real launchd domain and tmux server. Only Task 1's harness reads them. Nothing runs `agent-deck`, `claude` or a mutating `launchctl` against them.
- Named commands, run from the worktree root:
  - **GOTEST** `<args>` = `nix shell nixpkgs#go_1_26 -c bash scripts/test.sh -race -count=1 -timeout 20m <args>`
  - **GOTEST-FULL** `<pkgs>` = `PERF_BUDGET_MULTIPLIER=2.0 nix shell nixpkgs#go_1_26 -c bash scripts/test.sh -race -count=1 -timeout 150m <pkgs>`
  - **PYTEST** `<files>` = `P=$HOME/Products/agent-deck-conductor-setup-fix; mkdir -p $P/pytest-home && nix shell --impure --expr 'let p = (builtins.getFlake "nixpkgs").legacyPackages.${builtins.currentSystem}; in p.python3.withPackages (ps: with ps; [ pytest toml ])' -c env -u XDG_DATA_HOME -u XDG_CONFIG_HOME HOME=$P/pytest-home AGENT_DECK_CONDUCTOR_DIR=$P/pytest-home/no-conductor PYTHONDONTWRITEBYTECODE=1 PYTEST_DISABLE_PLUGIN_AUTOLOAD=1 python3 -m pytest -p no:cacheprovider --basetemp=$P/pytest-tmp -q <files>`. The overrides matter: agent sessions on hera export `AGENT_DECK_CONDUCTOR_DIR` as the live conductor directory, and the bridge opens its `bridge.log` there at import.
  - **GOFMT** `<files>` = `nix shell nixpkgs#go_1_26 -c gofmt -l <files>`; it must print nothing.
  - **COMMIT** = write the message (a conventional subject, the body, then the attribution lines the executing session is given) to a file under `~/Products/agent-deck-conductor-setup-fix/plan-work/`, then `nix shell nixpkgs#go_1_26 -c git commit -F <file>`. The lefthook pre-commit runs `gofmt -l .` and `go vet ./...`, so `go` must be on PATH.
- Tests: never a bare `go test`, `make test` or `make ci` on hera. GOTEST's `scripts/test.sh` runs under `env -i` with a temporary HOME, TMPDIR and TMUX_TMPDIR; Linux runs go through Docker (Task 14). Every regression test fails on the unfixed code first, and each task shows that failure.
- Service managers. A temporary HOME does not isolate launchd: `launchctl unload <tmp>/com.agentdeck.conductor-bridge.plist` acts on the live agent with that label. So real `launchctl` and `systemctl` stay unreachable from every test, in one of three ways:
  - `internal/session` tests call `installServiceManagerFake(t)` (Task 2) before they reach a seam;
  - `cmd/agent-deck` tests put recording fakes first on PATH (Task 9's helpers) or rely on TestMain's failing stubs;
  - Task 13's shell tests use a recording PATH fake, and the driver refuses to run unless `command -v launchctl` resolves to that fake.
- Root: a 0444 fixture is read-only for every uid, because `mode&0o222 == 0` is checked before `access(2)`, so it needs no root skip. Only a test that needs a truly unwritable path calls `testutil.SkipIfRoot`.
- Order: run the tasks in numeric order. Task 4 needs 3; 5 needs 4; 6 needs 2; 7 needs 2 and 6; 9 needs 4, 6, 7 and 8; 10 needs 4, 6, 7 and 9; 11 needs 4, 7, 8 and 9; 14 needs all. Task 1 must run before Task 12: its router round trip expects the unfixed tree to fail.
- Scratch and build products go under `~/Products/agent-deck-conductor-setup-fix/`, never `/tmp` (the harness scratchpad included) or the tree. There are two exceptions: `scripts/test.sh`'s own sandbox, and the nix gate's Makefile `mktemp -d` (Task 14 Step 6).
- Filesystems without atomic exchange (an NFS home) get no fallback. Replacing a stamped bridge there fails with the move-aside remedy, and setup exits 1 at step 7, as it does today for any bridge write failure (pinned by Task 4).
- Do not touch:
  - `CHANGELOG.md`, `.github/`, the release files, `go.mod` and `go.sum`, the `Makefile`, and the goldens under `cmd/agent-deck/testdata/goldens/`;
  - the files that nix-config's `overlays/ai/agent-deck-legacy-remote-capability.patch` patches: `cmd/agent-deck/main.go`, `cmd/agent-deck/version_capabilities.go` and its test, `internal/session/remote_channel.go`, `internal/session/remote_channel_capabilities_test.go` and `internal/session/remote_poll_transport_test.go`. They were promised to `nix` on 2026-10-31.
- Conventions (AGENTS.md):
  - errors are lowercase, with no trailing punctuation, and wrap with `%w`;
  - tests are table-driven and reuse existing helpers;
  - the bridge keeps its Python 3.8 floor, so `loop.run_in_executor` and never `asyncio.to_thread`.
- User-facing text is exact, as each task pins it. Two lines recur across tasks:
  - `[skip] bridge.py has local changes; left unchanged. To take the shipped bridge, move it aside and re-run 'agent-deck conductor setup <name>'.`
  - `[skip] <label>: managed externally (<read-only|symlink>)`

## Where this plan departs from the design

- **Service-manager test guard (Task 2).** The design asked only for seams. In a test binary, a mutating `launchctl` or `systemctl` verb now panics unless the tool resolves to a fake under the temp dir. Query verbs are an allow-list, so the check fails closed.
- **heartbeat.sh (Task 8).** Design §2 lists the director's custom `heartbeat.sh` as already safe. It is not: `InstallHeartbeatScript` rewrites it on every setup rerun with heartbeat on, and on every `migrate-dir --apply`. A script without the managed markers is now kept.
- **"Read-only" (Tasks 6 and 13).** A file with no write bit at all is external even for root, and `access(2)` `W_OK` decides the rest. The design named only `access(2)`, which root passes on a 0444 file.
- **No atomic exchange (Task 4).** There is no fallback, as the Global Constraints state.
- **A failed first write (Task 3).** A create whose write fails now removes the partial file. Left in place, it would read as the user's bridge on every later run. A crash in the middle of that write can still leave a partial, unstamped `bridge.py`, reported as kept until it is moved aside. Design §5.1's "a crash heals on the next run" covers only a crash between the write and the stamp.
- **Sandbox (Task 1).**
  - The design copied `config.toml` with its Discord token at 0600. The copy now carries a placeholder token instead, and the router round trip builds its own sandbox with an empty token.
  - The `launchctl` shim refuses every verb except `list`, `print` and `managername`, where the design listed seven refused verbs.
- **setup.sh's kept line (Task 13).** It names `conductor/setup.sh` as the remedy, where the design said "the same skip line": the script and the Go command write different directories.
- **Smaller behaviour changes:**
  - Task 6 deletes `RemoveHeartbeatPlist`.
  - Task 7: systemd removal makes no call when no unit file exists, and the heartbeat's enable-failure text becomes `unit written but enable failed: ...`.
  - Task 10: teardown prints `[skip] bridge.py is a symlink; kept` for a symlinked bridge.
  - Task 11: migrate-dir's reload check uses `Lstat`, so a dangling symlinked bridge plist or unit is reported, not silently skipped.
- **Docs (Tasks 4, 9 and 13).** These tasks update the user docs whose facts change. Design §5.3's "no user-doc change" covers only the Q&A hook.
- **Discovered work, filed in obr from `agent-deck-z8i` and not fixed here:**
  - `agent-deck-388`: the update timer rewrites `com.agentdeck.autoupdate` without an ownership check.
  - `agent-deck-9gc`: setup.sh overwrites `HEARTBEAT_RULES.md`.
  - `agent-deck-j5d`: setup.sh rewrites each profile's `CLAUDE.md`.
  - `agent-deck-rke`: `test_discord_typing_best_effort.py` already fails at the base under Python 3.14.
  - `agent-deck-cm3`: the docs list only Telegram and Slack as channels.

## Review Focus

The input classes most likely to bite, each pinned by a test in the task that owns the code:

1. **A conductor directory on a filesystem without atomic exchange (an NFS home).** A stamped bridge is not replaced. The error says to move `bridge.py` aside, and the bridge, the stamp and the directory are left as they were, with no temp file. Pinned by Task 4's `TestBridgeScript_UnsupportedExchangeKeepsBridge`, subtests `EINVAL`, `ENOTSUP` and `other failure`.
2. **An unedited bridge from a release before the stamp (hera's case).** It is kept with the notice, never stamped and never removed. Pinned by Task 4's `TestBridgeScript_InstallOwnership` row `pre-fix unstamped older bridge: kept` and `TestBridgeScript_RemoveOnlyOwnedBridge` row `pre-fix unstamped older bridge`. Task 14 Step 12 then checks hera's real bridge in the sandbox.
3. **Setup run as root against 0444 plists.** The plists still count as external. Pinned by Task 6's `TestServiceFileExternal` rows `root run on 0444` and `root-owned 0644`, and by Task 13's `TestShellServiceFileExternal` row `no write bit`.
4. **A dangling symlink as a plist or unit.** It counts as external: left untouched and reported. Pinned by:
   - Task 6's `externalServiceKinds` kind `dangling symlink`, in the replace, remove and wiring tests;
   - Task 7's `externalUnitKinds`;
   - Task 11's `TestConductorMigrateDir_NoOpApplyReportsKeptBridgeAndExternalAgents/dangling`.
5. **`teardown --all` with mixed ownership.** Each external agent gets a `[skip]` line and no call; each owned agent is stopped and removed; each is reported once. Pinned by Task 10's `TestConductorTeardownExternal_AllSkipsExternalAgents`, subtests `both-external`, `mixed-ownership` and `reverse-mix`.

## Task map

| Task | Delivers | Main files | Needs |
|---|---|---|---|
| 1 | Sandbox harness and the "before" evidence (no commit) | `~/Products/agent-deck-conductor-setup-fix/acceptance/` | none |
| 2 | Service-manager seams and the test-binary guard | `internal/session/service_manager.go` | none |
| 3 | Generated-file writer: predicate, outcome, displaced copy | `internal/session/conductor.go` | none |
| 4 | `bridge.py` ownership by stamp | `internal/session/conductor_bridge_script.go` | 3 |
| 5 | `update` honours a kept bridge; no more `bridge.py.backup` | `internal/update/update.go` | 4 |
| 6 | Launch agents: external check, safe replace and remove | `internal/session/service_files.go` | 2 |
| 7 | systemd units: the same, plus the platform probes | `internal/session/service_files.go` | 2, 6 |
| 8 | Keep a custom `heartbeat.sh` | `internal/session/conductor.go` | none |
| 9 | `conductor setup` wiring and its docs | `cmd/agent-deck/conductor_cmd.go` | 4, 6, 7, 8 |
| 10 | `conductor teardown` wiring | `cmd/agent-deck/conductor_cmd.go` | 4, 6, 7, 9 |
| 11 | `conductor migrate-dir`: external blockers, stamp carried | `internal/session/conductor_migrate_dir.go` | 4, 7, 8, 9 |
| 12 | The Discord Q&A hook in the embedded bridge | `internal/session/conductor_bridge.py` | none |
| 13 | `conductor/setup.sh` and `teardown.sh` | `conductor/setup.sh`, `conductor/teardown.sh` | none |
| 14 | Final verification and the sandbox "after" run | none in the tree | all |

---

### Task 1: Sandbox harness and the "before" run

**Files:** (all outside the repository; nothing goes in the tree, so this task has no COMMIT)
- Create in `/Users/johnw/Products/agent-deck-conductor-setup-fix/acceptance/` (`$ACC`), each `chmod 755` once written (callers still run `bash <script>`): `launchctl-shim.sh`, `sanitize-config.py`, `lib.sh`, `run-sandbox.sh`, `router-roundtrip.sh`.
- Create (generated): `$ACC/evidence-before/`, `$ACC/roundtrip-before/`. The sandbox, `/Users/johnw/Products/agent-deck-conductor-setup-fix/sandbox`, exists only while a runner runs. Modify: none. Test: none in the tree; every step ends with a check.

**Interfaces:**
- Consumes: nothing from other tasks. The installed `/etc/profiles/per-user/johnw/bin/agent-deck` (`readlink -f`: `/nix/store/pfwwhcn8i3gmh0pqzk5d0cq816f2y0j1-agent-deck-1.16.34/bin/agent-deck`; its nix pin, 0953876e, is this branch's base) and hera's live install, read only.
- Produces (Task 14 reuses all of it):
  - `run-sandbox.sh BINARY OUTDIR` (OUTDIR must not exist) writes `binary.txt harness.log live-status.txt launchctl.log plist-modes.txt snap0.txt snap1.txt snap2.txt setup.{stdout,stderr,exit} teardown.{stdout,stderr,exit} setup.diff teardown.diff setup-changed-paths.txt teardown-changed-paths.txt`; last line `setup exit <n>, teardown exit <n>; evidence in <OUTDIR>`. `plist-modes.txt` holds `stat -f '%Lp %N'` of both sandbox plists after each snapshot: six `<mode> <abs path>` lines, two per snapshot in order (a missing plist leaves stat's error line).
  - `router-roundtrip.sh TREE OUTDIR` (OUTDIR must not exist) writes `harness.log director-before.txt director-after.txt roundtrip.stdout roundtrip.stderr roundtrip.out`; last line `python exit <rc>; last stdout line: <line>; director <unchanged|CHANGED>; evidence in <OUTDIR>`; exits 0 only for rc 0, `{"handled": false}` and `unchanged`.
  - `lib.sh`: `ACC SANDBOX PY COND LABELS ENVI`, `need CMD...`, `build_sandbox PLACEHOLDER`, `snapshot ROOT`, `live_status PHASE`, `cleanup_sandbox`. Caller protocol: refuse if `$SANDBOX` exists, set an EXIT trap that runs `cleanup_sandbox`, then build. The sandbox is always removed at exit.
  - `sanitize-config.py SRC DST PLACEHOLDER`. Shim log lines: `answered Aqua | managername`, `refused | <args>`, `pass rc=<n> | <args>`.

Every command block sets `ACC=$HOME/Products/agent-deck-conductor-setup-fix/acceptance` itself (each Bash call is a fresh shell). The scripts start `#!/bin/bash`, which is 3.2.57 (`bash` on PATH, which callers run, is nix bash 5.3), so they use no bash-4 features (`read -d ''`, `<(…)` and here-strings work) and the syntax checks run `/bin/bash -n`. Checks call `/usr/bin/stat`, because John's PATH puts GNU coreutils first.

- [ ] **Step 1: Write `launchctl-shim.sh`, then `chmod 755` it (the check's `cp` keeps that mode).** `build_sandbox` installs it as `<sandbox>/bin/launchctl`, so it logs to `<sandbox>/launchctl.log`. Only `list` and `print` reach `/bin/launchctl`. `managername` answers `Aqua`, as John's terminal would; in an agent's Background domain the notifier guard would hide the bug. Every other verb is refused.
```bash
#!/bin/bash
log="$(cd "$(dirname "$0")/.." && pwd)/launchctl.log"
case "${1-}" in
  list|print) rc=0; /bin/launchctl "$@" || rc=$?; echo "pass rc=$rc | $*" >> "$log"; exit "$rc" ;;
  managername) echo "answered Aqua | $*" >> "$log"; echo Aqua ;;
  *) echo "refused | $*" >> "$log"; echo "launchctl shim: refused: $*" >&2; exit 1 ;;
esac
```
Check it (the refused targets are harmless even if the shim were wrong):
```bash
ACC=$HOME/Products/agent-deck-conductor-setup-fix/acceptance; T=$(mktemp -d "$HOME/Products/agent-deck-conductor-setup-fix/shimcheck.XXXXXX"); mkdir "$T/bin"; cp "$ACC/launchctl-shim.sh" "$T/bin/launchctl"
"$T/bin/launchctl" managername; "$T/bin/launchctl" unload /nonexistent.plist; echo "rc=$?"; "$T/bin/launchctl" bootout gui/501/none; echo "rc=$?"
"$T/bin/launchctl" list com.agentdeck.transition-notifier >/dev/null; echo "rc=$?"; cat "$T/launchctl.log"; rm -rf "$T"
```
Expected, in order: `Aqua`; `launchctl shim: refused: unload /nonexistent.plist` (stderr), `rc=1`; `launchctl shim: refused: bootout gui/501/none`, `rc=1`; `rc=0` (`list` from this session sees the live agents, verified 2026-10-31); then the log's four lines `answered Aqua | managername`, `refused | unload /nonexistent.plist`, `refused | bootout gui/501/none`, `pass rc=0 | list com.agentdeck.transition-notifier`.

- [ ] **Step 2: Write `sanitize-config.py SRC DST PLACEHOLDER`** (stdlib only: json, os, re, sys, tomllib). It never prints a config value. In order:
  1. `tomllib.load(SRC)`: the live file must parse. Exit 1 (`sys.exit(msg)`, so on stderr) with `refusing: [conductor].dir or a value naming the real HOME would escape the sandbox` if `[conductor].dir` is set, or if any string value (recursing through tables and arrays) contains `os.path.expanduser("~")`.
  2. One pass over SRC's lines. A header line matching `^\s*\[\[?([^\[\]]+)\]\]?\s*(#.*)?$` sets the current table (whitespace removed). In table `conductor.discord`, a line matching `^(\s*(?:bot_token|token)\s*=\s*)` becomes that prefix + `json.dumps(PLACEHOLDER)` + `\n`, counted in n; every other line is copied verbatim. Write DST with `os.open(DST, O_WRONLY|O_CREAT|O_EXCL, 0o600)`.
  3. Verify by re-parsing DST: n ≥ 1; each of `bot_token`/`token` that SRC's `[conductor.discord]` has equals PLACEHOLDER; with those keys dropped from both, DST equals SRC. On any failure, unlink DST and exit 1 with `sanitize-config: copy failed verification; removed it`. Otherwise print `sanitized <DST>: replaced <n> token value(s)`.
Check it. hera's config has exactly one token line (`bot_token`, line 53, under the indented `  [conductor.discord]` at line 52), no `dir` key and no `[tmux]` table, so the `grep` cannot show a secret:
```bash
ACC=$HOME/Products/agent-deck-conductor-setup-fix/acceptance; PY=/etc/profiles/per-user/johnw/bin/python3; S=$(mktemp -d "$HOME/Products/agent-deck-conductor-setup-fix/sanitizecheck.XXXXXX"); L=$HOME/.config/agent-deck/config.toml
"$PY" -I "$ACC/sanitize-config.py" "$L" "$S/config.toml" sandbox-not-a-token; /usr/bin/stat -f %Lp "$S/config.toml"; grep -E '^[[:space:]]*bot_token' "$S/config.toml"
diff <(grep -v -E '^[[:space:]]*(bot_)?token[[:space:]]*=' "$L") <(grep -v -E '^[[:space:]]*(bot_)?token[[:space:]]*=' "$S/config.toml") && echo identical-otherwise
printf '[conductor]\ndir = "/x"\n' > "$S/bad.toml"; "$PY" -I "$ACC/sanitize-config.py" "$S/bad.toml" "$S/out.toml" x; echo "rc=$?"; test -e "$S/out.toml" || echo no-out; rm -rf "$S"
```
Expected, in order: `sanitized <S>/config.toml: replaced 1 token value(s)`, `600`, `    bot_token = "sandbox-not-a-token"`, `identical-otherwise`, `refusing: [conductor].dir or a value naming the real HOME would escape the sandbox`, `rc=1`, `no-out`. No line shows the live token.

- [ ] **Step 3: Write `lib.sh`.** Both runners source it. Its own commands use the BSD tools, because John's PATH puts GNU coreutils first and its `find` is bfs:
```bash
export PATH=/usr/bin:/bin:/usr/sbin:/sbin
ACC="$HOME/Products/agent-deck-conductor-setup-fix/acceptance" SANDBOX="$HOME/Products/agent-deck-conductor-setup-fix/sandbox"
PY=/etc/profiles/per-user/johnw/bin/python3 COND=.local/share/agent-deck/conductor
LABELS=(com.agentdeck.conductor-bridge com.agentdeck.transition-notifier)
ENVI=(env -i "PATH=$SANDBOX/bin:/etc/profiles/per-user/johnw/bin:/usr/bin:/bin:/usr/sbin:/sbin" "HOME=$SANDBOX/home" "TMUX_TMPDIR=$SANDBOX/tmux" AGENTDECK_SKIP_UPDATE_CHECK=1)
need() { "$@" || { echo "check failed: $*" >&2; exit 1; }; }
```
- `build_sandbox PLACEHOLDER`, with h=`$SANDBOX/home` and every action through `need`, so it fails closed even when the caller has no `set -e`. In order:
  1. `mkdir -p` `$SANDBOX/bin`, `$h/.local/share/agent-deck`, `$h/.config/agent-deck` and `$h/Library/LaunchAgents`; `mkdir -m 700 $SANDBOX/tmux`. Copy the shim to `$SANDBOX/bin/launchctl` and `chmod 755` it.
  2. `cp -pPR "$HOME/$COND" "$h/.local/share/agent-deck/"` (`venv` and `Agent Deck Conductor Bridge.app` stay symlinks); `cp -p` each live `~/Library/LaunchAgents/<label>.plist` into `$h/Library/LaunchAgents/`; `"$PY" -I "$ACC/sanitize-config.py" ~/.config/agent-deck/config.toml "$h/.config/agent-deck/config.toml" "$1"`.
  3. Checks: the copied `venv` is a symlink, and so is the `.app` whenever the live one is (`test -L`; that stale link may be cleaned up before Task 14); `cmp -s` of the live and copied `bridge.py`; each copied plist's `stat -f %Lp` is `444`; `"${ENVI[@]}" /bin/sh -c 'command -v launchctl'` prints `$SANDBOX/bin/launchctl`.
  4. Last, a `while IFS= read -r -d '' p` loop fed `< <(find "$SANDBOX" -type l -print0)` (so its `exit 1` ends the caller) requires each link to `readlink -f` to a path under `/nix/store/` or `$SANDBOX/`; a writer following any other link would reach the live install. BSD `readlink -f` fails on a dangling link: such a link passes only when its own target (`readlink`) starts with `/nix/store/` and has no `/../`, since johnw cannot create anything in the store (hera's `.app` link dangles into a collected store path, checked 2026-11-01). Otherwise `check failed: <link> resolves outside /nix/store and <SANDBOX>`, exit 1.
- `snapshot ROOT`: inside ROOT, `find . -type f -print0 | xargs -0 shasum -a 256 | LC_ALL=C sort -k 2`, then `symlink ./path -> <readlink target>` for each `find . -type l` entry, `LC_ALL=C sort`ed. It follows no link.
- `live_status PHASE`: queries only, against the real launchd and the live files. It records failures and never aborts. Per label it writes `PHASE list <label> rc=<n>` (`/bin/launchctl list <label>`) and `PHASE print gui/<uid>/<label> rc=<n>` (`/bin/launchctl print gui/$(id -u)/<label>`), with output discarded. Then it writes `PHASE <sha256>  <path>` from `shasum -a 256`, run in `$HOME`, on `$COND/bridge.py` and both `Library/LaunchAgents/<label>.plist`; errors land in the record (`2>&1 … || true`).
- `cleanup_sandbox`: for each socket (`[ -S ]`) under `$SANDBOX/tmux/tmux-*/*` (any socket name), it prints `cleanup: stopping tmux server on <socket>` and runs `/etc/profiles/per-user/johnw/bin/tmux -S <socket> kill-server || true`. It never touches the default socket. Then `rm -rf "$SANDBOX"`.
Check `snapshot` on a fixture with a space in every name and a symlink to a directory:
```bash
ACC=$HOME/Products/agent-deck-conductor-setup-fix/acceptance; T=$(mktemp -d "$HOME/Products/agent-deck-conductor-setup-fix/snapcheck.XXXXXX")
mkdir "$T/d d"; printf x > "$T/d d/a b.txt"; ln -s "$T/d d" "$T/X Y.app"; bash -c '. "$1/lib.sh"; snapshot "$2"' _ "$ACC" "$T"; rm -rf "$T"
```
Expected: exactly `2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881  ./d d/a b.txt` and `symlink ./X Y.app -> <T>/d d`. Nothing is listed under `./X Y.app/`.

- [ ] **Step 4: Write `run-sandbox.sh`** (conductor setup and teardown of `steward-probe` in a fresh copy of hera's install):
```bash
#!/bin/bash
set -euo pipefail; . "$(dirname "$0")/lib.sh"
BIN=${1:?usage: run-sandbox.sh BINARY NEW-OUTDIR} OUT=${2:?usage: run-sandbox.sh BINARY NEW-OUTDIR}
[ -x "$BIN" ] && [ ! -e "$OUT" ] || { echo "run-sandbox: need an executable BINARY and a new OUTDIR" >&2; exit 2; }
[ ! -e "$SANDBOX" ] || { echo "run-sandbox: $SANDBOX exists (aborted run?); inspect and delete it" >&2; exit 2; }
mkdir -p "$OUT"; exec > >(tee "$OUT/harness.log") 2>&1
trap 'cp "$SANDBOX/launchctl.log" "$OUT/" 2>/dev/null || touch "$OUT/launchctl.log"; cleanup_sandbox' EXIT
readlink -f "$BIN" > "$OUT/binary.txt"; build_sandbox sandbox-not-a-token
run() { local n=$1 rc=0; shift; "${ENVI[@]}" "$BIN" "$@" </dev/null >"$OUT/$n.stdout" 2>"$OUT/$n.stderr" || rc=$?; echo "$rc" > "$OUT/$n.exit"; }
snap() { local l; snapshot "$SANDBOX/home" > "$OUT/$1.txt"; for l in "${LABELS[@]}"; do stat -f '%Lp %N' "$SANDBOX/home/Library/LaunchAgents/$l.plist" 2>&1 || true; done >> "$OUT/plist-modes.txt"; }
snap snap0; live_status before > "$OUT/live-status.txt"
run setup conductor setup steward-probe -agent claude -no-heartbeat; snap snap1
run teardown conductor teardown steward-probe --remove; snap snap2; live_status after >> "$OUT/live-status.txt"
for p in setup:snap0:snap1 teardown:snap1:snap2; do IFS=: read -r n a b <<<"$p"; diff -u "$OUT/$a.txt" "$OUT/$b.txt" > "$OUT/$n.diff" || [ $? -eq 1 ]
  sed -n -E 's/^[-+]([0-9a-f]{64}  |symlink )//p' "$OUT/$n.diff" | sed -E 's/ -> .*$//' | LC_ALL=C sort -u > "$OUT/$n-changed-paths.txt"; done
echo "setup exit $(cat "$OUT/setup.exit"), teardown exit $(cat "$OUT/teardown.exit"); evidence in $OUT"
```
Check it:
```bash
ACC=$HOME/Products/agent-deck-conductor-setup-fix/acceptance; for f in launchctl-shim.sh lib.sh run-sandbox.sh; do /bin/bash -n "$ACC/$f"; done
bash "$ACC/run-sandbox.sh" /nonexistent "$ACC/evidence-x"; echo "rc=$?"; [ ! -e "$ACC/evidence-x" ] && [ ! -e "$HOME/Products/agent-deck-conductor-setup-fix/sandbox" ] && echo clean
```
Expected: the `/bin/bash -n` loop prints nothing, then `run-sandbox: need an executable BINARY and a new OUTDIR`, `rc=2` and `clean`.

- [ ] **Step 5: Write `router-roundtrip.sh TREE OUTDIR`.** It has the same frame as run-sandbox.sh: `set -euo pipefail`, it sources lib.sh, and it refuses with exit 2 and `router-roundtrip: need a TREE holding internal/session/conductor_bridge.py and a new OUTDIR` or `router-roundtrip: $SANDBOX exists (aborted run?); inspect and delete it`. Then, in order (every file named below lives in OUTDIR):
  1. `mkdir -p OUTDIR`, then `exec > >(tee OUTDIR/harness.log) 2>&1`; `trap cleanup_sandbox EXIT`; `build_sandbox ""` (the empty token makes the router's `notify_discord.request()` raise before any network call). Set `C=$SANDBOX/home/$COND`.
  2. Check, through `need`, that the token is empty without printing it: `"$PY" -I -c 'import sys, tomllib; d = tomllib.load(open(sys.argv[1], "rb"))["conductor"]["discord"]; sys.exit(d.get("bot_token", "") + d.get("token", "") != "")' "$SANDBOX/home/.config/agent-deck/config.toml"`.
  3. `snapshot "$C/director" > director-before.txt`.
  4. Run the block below with `env -i HOME=$SANDBOX/home AGENT_DECK_CONDUCTOR_DIR=$C PYTHONDONTWRITEBYTECODE=1 "$C/venv/bin/python3" -P -c '<block>' "$TREE/internal/session/conductor_bridge.py" </dev/null >roundtrip.stdout 2>roundtrip.stderr`, and record its rc. `-P` keeps the cwd off `sys.path`; the router child inherits PYTHONDONTWRITEBYTECODE, and reference `"1"` names no stored question.
  5. `snapshot "$C/director" > director-after.txt`, then `tail -n 1 roundtrip.stdout > roundtrip.out` (the bridge logs to stdout too). Print the last line (see Interfaces), then exit 0 when rc is 0, `roundtrip.out` is exactly `{"handled": false}` and `cmp -s` finds the two manifests equal; otherwise exit 1.
```python
import importlib.util, json, sys
spec = importlib.util.spec_from_file_location("bridge", sys.argv[1]); bridge = importlib.util.module_from_spec(spec)
sys.modules["bridge"] = bridge; spec.loader.exec_module(bridge)
print(json.dumps(bridge.route_question({"reference": "1", "message": "2", "author": "3", "channel": "4", "text": "1", "custom_id": "qna:000000000000:1"})))
```
Check it, then run it once on the unfixed tree: Task 12 adds `route_question`, so the run must fail at exactly that point.
```bash
ACC=$HOME/Products/agent-deck-conductor-setup-fix/acceptance; W=/Users/johnw/src/fork/agent-deck/.worktrees/work-conductor-setup-fix; /bin/bash -n "$ACC/router-roundtrip.sh"; /usr/bin/stat -f '%Lp %N' "$ACC"/*.sh "$ACC"/*.py
bash "$ACC/router-roundtrip.sh" /nonexistent "$ACC/roundtrip-x"; echo "rc=$?"; bash "$ACC/router-roundtrip.sh" "$W" "$ACC/roundtrip-before"; echo "rc=$?"
grep -F "AttributeError: module 'bridge' has no attribute 'route_question'" "$ACC/roundtrip-before/roundtrip.stderr"
[ ! -e "$ACC/roundtrip-x" ] && [ ! -e "$HOME/Products/agent-deck-conductor-setup-fix/sandbox" ] && [ -z "$(git -C "$W" status --porcelain --ignored)" ] && echo clean
```
Expected: `755` for each of the five scripts; the TREE refusal and `rc=2`. The unfixed-tree run ends `python exit 1; last stdout line: ; director unchanged; evidence in /Users/johnw/Products/agent-deck-conductor-setup-fix/acceptance/roundtrip-before`, then `rc=1`. The `grep` prints the AttributeError line, and the last line is `clean` (no `__pycache__` in the tree).

- [ ] **Step 6: Do the "before" run, then check design section 7's "before" expectations.** The block's first line runs it. Expected: the run ends `setup exit 0, teardown exit 0; evidence in /Users/johnw/Products/agent-deck-conductor-setup-fix/acceptance/evidence-before`, then `sandbox-gone`. If a check differs, record what the evidence shows in Step 7; never edit the harness to make a check pass. The shim log should be exactly `answered Aqua | managername` and `refused | unload /Users/johnw/Products/agent-deck-conductor-setup-fix/sandbox/home/Library/LaunchAgents/com.agentdeck.transition-notifier.plist`. The Aqua answer short-circuits the guard at conductor.go:2897 before its `list` probe. The unload at :2915 is refused, and the write at :2916 fails on the 0444 copy before the load at :2919.
```bash
ACC=$HOME/Products/agent-deck-conductor-setup-fix/acceptance; E="$ACC/evidence-before"; bash "$ACC/run-sandbox.sh" /etc/profiles/per-user/johnw/bin/agent-deck "$E"; [ ! -e "$HOME/Products/agent-deck-conductor-setup-fix/sandbox" ] && echo sandbox-gone
cat "$E/binary.txt" "$E/launchctl.log" "$E/plist-modes.txt"   # …-agent-deck-1.16.34/bin/agent-deck; the two shim lines; six "444 <sandbox plist>" lines (two per snapshot)
grep -E '^[-+][0-9a-f]{64}  \./\.local/share/agent-deck/conductor/bridge\.py$' "$E/setup.diff"   # a - and a + line, different hashes
grep -c 'Library/LaunchAgents' "$E/setup.diff"                     # 0: both 0444 copies byte-identical
grep -F 'failed to write notifier plist' "$E/setup.stderr"; grep -E 'Python dependencies (install failed|could NOT be installed)' "$E/setup.stderr"; grep -F '[ok] bridge.py installed' "$E/setup.stdout"
diff <(sed -n 's/^before //p' "$E/live-status.txt") <(sed -n 's/^after //p' "$E/live-status.txt") && echo live-unchanged
grep -c -E '^(before|after) list com\.agentdeck\.(conductor-bridge|transition-notifier) rc=0$' "$E/live-status.txt"   # 4: both live agents loaded, before and after
grep '/conductor/' "$E/teardown-changed-paths.txt" | grep -v '/conductor/steward-probe/'   # no output
grep -v '/conductor/steward-probe/' "$E/setup-changed-paths.txt"   # for the record
```
- The three greps on one line print one line each: permission denied on the 0444 copy, the pip block's first line, and `[ok] bridge.py installed`. "Live agents still loaded" means `list` rc=0. The `print gui/<uid>/` lines are recorded too, and `live-unchanged` covers them.
- The record list should hold `bridge.py` and agent-deck's own state, log and bus files. If hera's shared `CLAUDE.md` is an unmodified older template, it also holds that `CLAUDE.md` and its `.CLAUDE.md.previous-*` sibling.

- [ ] **Step 7: Record the verdict (no commit).** Fill in each `<…>` from the evidence, and keep `"`, `$` and backticks out of the text. obr runs from this worktree with `--no-auto-flush` (Global Constraints), so it writes only the obr database and never the main checkout's tracked `docs/PLAN.org`.
```bash
cd /Users/johnw/src/fork/agent-deck/.worktrees/work-conductor-setup-fix && obr --no-auto-flush comments add agent-deck-z8i "Sandbox before-run (Task 1): binary <binary.txt>; evidence ~/Products/agent-deck-conductor-setup-fix/acceptance/evidence-before. setup exit <n>, teardown exit <n>. Shim log: <its lines>. bridge.py <old sha256 12 chars> -> <new 12 chars> (overwritten). Plists 444 and unchanged; <the failed-to-write line>. Pip: <first line of the block>. Live agents: list rc <values>; print rc <values>; live-unchanged <yes|no>. Setup also changed outside conductor/steward-probe/: <paths>. Router round trip on the unfixed tree: <its last line>. Verdict: <matches design section 7 before-expectations | differs: what>." &&
  obr --no-auto-flush comments list agent-deck-z8i | grep -F 'Sandbox before-run (Task 1)'
git status --porcelain --ignored
```
Expected: the `grep` prints the new comment, and `git status` prints nothing.

### Task 2: Service-manager seams and the test-binary guard

**Files:**
- Create: `internal/session/service_manager.go`, `internal/session/service_manager_fake_test.go`
- Modify: `internal/session/conductor.go` (var block :2857-2861; launchctl :2795, 2799, 2869, 2982, 3023, 3048, 3063, 3159, 3163, 3284; systemctl :2757, 2836, 2955, 2987, 2998, 3028, 3039, 3051, 3066, 3204, 3296, 3312)
- Modify: `internal/session/conductor_heartbeat_tick_test.go` (imports :3-11; `TestUninstallHeartbeatDaemon_UnknownAgentDisablesAndPreservesMeta` :353-391), `internal/session/conductor_test.go:243-249` (`TestBridgeDaemonHint`)
- Test: `internal/session/service_manager_test.go` (new)

**Interfaces:**
- Consumes (no earlier task): `withinBase(path, base string) bool` (conductor_migrate_dir.go:555; resolves symlinks on both sides, false when `path` does not resolve); `callRecover(fn func() string) (got, panicMsg string)` (sandbox_guard_test.go:77; returns a string panic verbatim).
- Produces (for Tasks 6, 7 and 10; Task 9's `mutatingServiceCalls` copies the query allow-list):
  - `func serviceManagerCommand(name string, args ...string) *exec.Cmd`, `func isMutatingServiceVerb(name string, args []string) bool`, `var serviceManagerFakeRoot = os.TempDir`, private `func serviceVerb(args []string) string`.
  - Seams in one `var` block: `runLaunchctl = func(args ...string) error` (moved from conductor.go:2860, same name and type), `launchctlOutput = func(args ...string) ([]byte, error)`, `runSystemctl = func(args ...string) error`.
  - `func installServiceManagerFake(t *testing.T) *serviceManagerFake` with `Calls() []string`, `MutatingCalls() []string`, `FailOn(tool, verb string, err error)`, `FailNext(tool, verb string, err error)`, `SetLoaded(label string, loaded bool)`.

- [ ] **Step 1: Write the failing tests for the classifier, the guard and the fake**

`service_manager_fake_test.go`: `type serviceManagerFake` (pointer receivers) holds a `sync.Mutex`, the calls, `FailOn` errors and `FailNext` FIFO queues keyed `tool+" "+serviceVerb(args)`, and the loaded labels. `installServiceManagerFake(t)` saves the three seams, points them at the fake and restores them in `t.Cleanup`; it never starts a process, and its callers never call `t.Parallel()`. Each call, in order: (1) record `strings.Join(append([]string{tool}, args...), " ")`, failed calls included; (2) return a queued `FailNext` error for the key (dequeued), else its `FailOn` error (every matching call); (3) `launchctl managername` → `"Aqua\n"`, as a login terminal and Task 1's shim answer; (4) `launchctl list <label>` → `label+"\n"` when loaded, else the error `launchctl list <label>: not loaded`; (5) systemctl `is-active` → nil when its last argument is loaded, else the error `<unit> inactive`; (6) anything else → nil, no output. `Calls()` returns a copy; `MutatingCalls()` keeps the entries for which `isMutatingServiceVerb(tool, args)` is true.

`service_manager_test.go` (imports errors, os, path/filepath, reflect, slices, strings, testing). `TestIsMutatingServiceVerb` checks `f := strings.Fields(cmdline); isMutatingServiceVerb(f[0], f[1:])`:

| want | cmdlines |
|---|---|
| false | `launchctl list com.agentdeck.conductor-bridge`, `launchctl print gui/501/com.agentdeck.conductor-bridge`, `launchctl print-disabled gui/501`, `launchctl managername`, `systemctl --user is-active --quiet x.service`, `systemctl --user is-enabled x.timer`, `systemctl --user show -p MainPID --value x.scope`, `systemctl cat x.service`, `systemctl --user status x.service`, `systemctl --user list-timers` |
| true | `launchctl load /p/x.plist`, `launchctl unload /p/x.plist`, `launchctl kill SIGTERM gui/501/x`, `launchctl setenv K V`, `systemctl --user enable --now x.timer`, `systemctl --user daemon-reload`, `systemctl --user reset-failed x.service`, `systemctl --user set-environment K=V`, `systemctl --user` (no verb), `loginctl show-user` (no other tool has an allow-list) |

`TestServiceManagerCommand_GuardsMutatingVerbsInTests`: `bin, elsewhere := t.TempDir(), t.TempDir()`; `bin/launchctl` holds `#!/bin/sh\nexit 0\n` (0o755); `serviceManagerFakeRoot` is swapped and restored in `t.Cleanup`; P = `/nonexistent/com.agentdeck.conductor-bridge.plist`. Rows run in order (PATH set with `t.Setenv`), each as `callRecover(func() string { return serviceManagerCommand(argv[0], argv[1:]...).Path })`:

| PATH | root | argv | expect |
|---|---|---|---|
| bin, then the old PATH | elsewhere | `launchctl unload P` | panic contains `launchctl unload P` and `installServiceManagerFake(t)` |
| same | elsewhere | `launchctl list com.agentdeck.conductor-bridge` | no panic (a query) |
| a new empty `t.TempDir()` | elsewhere | `systemctl --user daemon-reload` | panic contains `systemctl --user daemon-reload` (not on PATH) |
| bin | bin | `launchctl unload P` | no panic; Path == `filepath.Join(bin, "launchctl")` |
| bin; `bin/systemctl` is a symlink to `elsewhere/systemctl`, the same script (0o755) | bin | `systemctl --user daemon-reload` | panic contains `systemctl --user daemon-reload` (resolves outside the root) |

`TestInstallServiceManagerFake_RecordsCallsAndRestoresSeams`: snapshot `[3]uintptr` of `reflect.ValueOf(seam).Pointer()` for the three seams. In `t.Run("fake", ...)`: install the fake (the snapshot now differs); `SetLoaded(LaunchdPlistName, true)`, `SetLoaded("agent-deck-conductor-bridge.service", true)`, `FailOn("launchctl", "unload", errors.New("unload refused"))`, `FailNext("launchctl", "load", errors.New("load refused"))`; then, with P = `/abs/com.agentdeck.conductor-bridge.plist`:

| # | call | result |
|---|---|---|
| 1-2 | `runLaunchctl("unload", P)` twice | `unload refused` both times |
| 3-4 | `runLaunchctl("load", P)` twice | `load refused`, then nil |
| 5 | `launchctlOutput("list", LaunchdPlistName)` | `"com.agentdeck.conductor-bridge\n"`, nil |
| 6 | `launchctlOutput("list", TransitionNotifierLaunchdPlistName)` | error `launchctl list com.agentdeck.transition-notifier: not loaded` |
| 7 | `launchctlOutput("managername")` | `"Aqua\n"`, nil |
| 8-9 | `runSystemctl("--user", "is-active", "--quiet", U)`, U = `agent-deck-conductor-bridge.service`, then `agent-deck-transition-notifier.service` | nil, then error `agent-deck-transition-notifier.service inactive` |
| 10 | `runSystemctl("--user", "enable", "--now", "agent-deck-conductor-heartbeat-ops.timer")` | nil |

`Calls()` equals the ten command lines in call order; `MutatingCalls()` equals entries 1-4 and 10. After the subtest the snapshot equals the original.

- [ ] **Step 2: Run them, expect a build failure.** GOTEST `-run '^(TestIsMutatingServiceVerb|TestServiceManagerCommand_GuardsMutatingVerbsInTests|TestInstallServiceManagerFake_RecordsCallsAndRestoresSeams)$' ./internal/session/`. Expected: `FAIL	github.com/asheshgoplani/agent-deck/internal/session [build failed]`, after `undefined:` errors for names Step 3 adds (the compiler stops after ten, so not every name appears).

- [ ] **Step 3: Implement the classifier, the guard and the seams in `internal/session/service_manager.go`**
  - `serviceVerb(args)`: the first argument not starting with `-`, else `""`.
  - `isMutatingServiceVerb` fails closed: false only when `serviceVerb(args)` is in the tool's query allow-list; true for any other verb, an empty verb and any other tool. launchctl: list, print, print-disabled, managername. systemctl: is-active, is-enabled, is-failed, is-system-running, show, cat, status, list-units, list-unit-files, list-timers.
  - `serviceManagerFakeRoot`'s comment names the fakes under it: cmd/agent-deck's `stubServiceManagers` (`os.MkdirTemp("", ...)`) and `t.TempDir()` PATH fakes (Go 1.26 puts those under `$GOTMPDIR` when set; scripts/test.sh and CI leave it unset).
  - `serviceManagerCommand`: when `testing.Testing() && isMutatingServiceVerb(name, args)` (`testing.Testing()` as internal/sandboxguard uses it), `resolved, err := exec.LookPath(name)`; unless `err == nil && withinBase(resolved, serviceManagerFakeRoot())`, `panic` with this string (cmdline: name and args joined by single spaces); then `return exec.Command(name, args...)`.
    `fmt.Sprintf("service manager guard: refusing to run %q in a test binary: %s resolves to %q, not a fake under %q; install a fake through the seam (installServiceManagerFake(t)) before reaching this path", cmdline, name, resolved, serviceManagerFakeRoot())`
  - The seams are one-line closures over `serviceManagerCommand("launchctl"|"systemctl", args...)` ending `.Run()`, `.Output()`, `.Run()`. The block's comment: a temp HOME does not isolate launchd; a real `launchctl unload <tmp>/com.agentdeck.conductor-bridge.plist` acts on the live agent of that label.
  - Delete `runLaunchctl` from conductor.go's var block (:2857-2861); the two Background-guard seams stay, and `withGuardSeams` still swaps `runLaunchctl` by name.

- [ ] **Step 4: Run Step 2's command, expect a pass:** `ok  	github.com/asheshgoplani/agent-deck/internal/session	<time>`.

- [ ] **Step 5: Write the failing routing test** `TestServiceManagerSeams_RouteDaemonCalls` in service_manager_test.go (add import `github.com/asheshgoplani/agent-deck/internal/platform`). Unless `platform.Detect()` is PlatformMacOS, PlatformLinux or PlatformWSL2, `t.Skip("no launchd or systemd branch on this platform")`. `t.Setenv("PATH", t.TempDir())` (until routed, a direct exec finds no tool) and `t.Setenv("XDG_RUNTIME_DIR", t.TempDir())` (no loginctl, so systemdUserAvailable takes the daemon-reload fallback); `fake := installServiceManagerFake(t)`.
  - Nothing loaded: `IsBridgeDaemonRunning()` and `IsTransitionNotifierDaemonRunning()` are false, else `t.Fatalf("nothing loaded: running checks must be false; calls %q", fake.Calls())`.
  - After `SetLoaded(l, true)` for `LaunchdPlistName`, `TransitionNotifierLaunchdPlistName`, `systemdBridgeServiceName`, `systemdTransitionNotifierServiceName`: both are true, `launchctlManagerName() == "Aqua"` and `systemdUserAvailable()`, else `t.Fatalf("running checks, managername and the daemon-reload fallback must go through the seams; calls %q", fake.Calls())`.
  - `len(fake.Calls()) == 6` and `got := fake.MutatingCalls()` equals `[]string{"systemctl --user daemon-reload"}`, else `t.Fatalf("want four running checks, managername and one daemon-reload; got calls %q, mutating %q", fake.Calls(), got)`.

- [ ] **Step 6: Run it, expect a failure.** GOTEST `-run '^TestServiceManagerSeams_RouteDaemonCalls$' ./internal/session/`. Expected: `--- FAIL: TestServiceManagerSeams_RouteDaemonCalls` with `running checks, managername and the daemon-reload fallback must go through the seams; calls []` (conductor.go still execs the tools directly, finds none on the emptied PATH, and the fake records nothing).

- [ ] **Step 7: Route every call site in `internal/session/conductor.go`**, keeping the surrounding `_ =`, `if err := …; err != nil` and `== nil`. Line numbers are the base's; Step 3's deletion moved every site after :2860 up one line.
  - `exec.Command("launchctl", A...).Run()` → `runLaunchctl(A...)` at 2795, 2799, 2982, 3023, 3159, 3163, 3284; `.Output()` → `launchctlOutput(A...)` at 2869, 3048, 3063.
  - `exec.Command("systemctl", A...).Run()` → `runSystemctl(A...)` at the 12 systemctl sites: 2757 (the daemon-reload fallback), 2836, 2955, 2987, 2998, 3028, 3039, 3051, 3066, 3204, 3296, 3312; at 3296 also drop the `//nolint:gosec // G204` comment.
  - Unchanged: `loginctl` at 2755 (a direct query), and update_timer.go (internal/update's Runner behind its own seam `ensureUpdateTimerOnHost`; `update.AutoUpdateSuppressed` returns "running under go test").
  - Greps: `grep -rn -E 'exec\.Command\("(launchctl|systemctl)"' internal/session --include='*.go' | grep -v _test.go` prints nothing; `grep -c -E '\b(runLaunchctl|launchctlOutput|runSystemctl)\(' internal/session/conductor.go` prints `24` (22 routed plus the existing calls at 2915, 2919); `grep -c 'exec.Command("loginctl"' internal/session/conductor.go` prints `1`.

- [ ] **Step 8: Convert the two existing tests that reached a real mutating call** (an unconverted one now panics in the guard)
  - `TestUninstallHeartbeatDaemon_UnknownAgentDisablesAndPreservesMeta` (real daemon-reload at :3312 on Linux): `fake := installServiceManagerFake(t)` after `setupHeartbeatTickTest`; add the imports `"slices"` and `"github.com/asheshgoplani/agent-deck/internal/platform"`. After the `UninstallHeartbeatDaemon("future")` check, pin today's behaviour (Task 7 replaces this block, from the declaration through the `t.Fatalf`): `var wantCalls []string` (launchd: no plist, no call), set to `[]string{"systemctl --user daemon-reload"}` when `platform.Detect()` is PlatformLinux or PlatformWSL2; then `if got := fake.MutatingCalls(); !slices.Equal(got, wantCalls) { t.Fatalf("mutating service-manager calls = %q, want %q", got, wantCalls) }`.
  - `TestBridgeDaemonHint` (daemon-reload fallback at :2757 on Linux when XDG_RUNTIME_DIR survives testutil.IsolateHome and loginctl fails): `installServiceManagerFake(t)` as its first line.
  - This grep finds every service-manager test: `grep -rn -E '\b(Install|Uninstall|install|uninstall)[A-Za-z]*(Daemon|Launchd|Systemd)[A-Za-z]*\(|systemdUserAvailable\(|DaemonHint\(|Is[A-Za-z]*DaemonRunning\(|launchctlManagerName\(|runLaunchctl|exec\.Command\("(launchctl|systemctl)"' internal/session --include='*_test.go'`. Besides this task's files and the two above, it finds these, left as they are: the four `TestInstallTransitionNotifierLaunchd_*` (withGuardSeams swaps runLaunchctl and both guard seams); `TestLaunchdHeartbeat{InstallWritesFreshPlist,UninstallRefusesFailedUnload}`, `TestUninstallHeartbeatDaemon_StopFailureKeepsEnabled` and the three `TestSystemdInstall*` (PATH fakes in `t.TempDir()`, under `os.TempDir()`, which the guard admits); the `TestPersistence_*` tests in session_persistence_test.go and session_persistence_hostsensitive_test.go (their own code runs real `systemd-run`/`systemctl --user` on scopes they start; P0, they keep real systemd).

- [ ] **Step 9: Run the service-manager tests, expect a pass.** GOTEST `-run '^(TestIsMutatingServiceVerb|TestServiceManager.*|TestInstallServiceManagerFake_.*|TestUninstallHeartbeatDaemon_.*|TestBridgeDaemonHint|TestInstallTransitionNotifierLaunchd_.*|TestLaunchdHeartbeat.*|TestSystemdInstall.*)$' ./internal/session/`. Expected: `ok  	github.com/asheshgoplani/agent-deck/internal/session	<time>`; `_StopFailureKeepsEnabled` skips on macOS. hera runs only the launchd side of `platform.Detect()`; the systemd side (that test, the converted test's Linux expectation, the routing test's `is-active` checks) runs in Task 14's Linux container.

- [ ] **Step 10: Run the whole package** (over an hour on hera): GOTEST-FULL `./internal/session/ 2>&1 | tee ~/Products/agent-deck-conductor-setup-fix/logs/task2-session-full.log`. Expected: `ok  	github.com/asheshgoplani/agent-deck/internal/session	<time>`.
  - `service manager guard:` in the log: a test reached a real mutating call and the panic ended the run; its stack names the test. Add `installServiceManagerFake(t)` before the call, add its file to F (Step 11), name it in the commit body, rerun this step.
  - Any other `--- FAIL: TestX`: once, `git worktree add --detach ~/Products/agent-deck-conductor-setup-fix/base-0953876e 0953876e`; then `cd ~/Products/agent-deck-conductor-setup-fix/base-0953876e && PERF_BUDGET_MULTIPLIER=2.0` GOTEST `-run '^TestX$' ./internal/session/`. Fails there: pre-existing, name it in the commit body. Passes there: run the same command in this worktree; a failure is this task's (fix it before Step 11), a pass is a load flake (name it in the commit body). Finally, from this worktree, `git worktree remove ~/Products/agent-deck-conductor-setup-fix/base-0953876e`.

- [ ] **Step 11: Commit.** `F=(internal/session/service_manager.go internal/session/service_manager_fake_test.go internal/session/service_manager_test.go internal/session/conductor.go internal/session/conductor_heartbeat_tick_test.go internal/session/conductor_test.go)` (an array: zsh does not word-split `$F`). GOFMT `"${F[@]}"` prints nothing; `nix shell nixpkgs#go_1_26 -c make lint` prints `0 issues.` (an issue only in a file outside F predates this task: name it in the commit body); `git add "${F[@]}"`; COMMIT with this subject and body, then the two attribution lines:

```
fix(session): route launchctl and systemctl through guarded seams

A temp HOME does not isolate launchd, so in a test binary serviceManagerCommand
panics when a verb outside the query allow-list would reach a tool outside
os.TempDir(). TestUninstallHeartbeatDaemon_UnknownAgentDisablesAndPreservesMeta
and TestBridgeDaemonHint, which could run a real daemon-reload on Linux, use the fake.
```

### Task 3: Generated-file writer: predicate, outcome, displaced destination

**Files:**
- Modify: `internal/session/conductor.go`: the imports (:3-19) gain `"bytes"`; the new declarations go right after the `exchangeGeneratedFiles` seam (:1772); the body of `writeGeneratedFileOrMigrate` (:1779-1867) moves into `replaceGeneratedFile`.
- Create: `internal/session/conductor_generated_file_test.go`
- Test: the new file, plus the 23 unedited tests of the writer and its callers that Step 6 runs, in these `internal/session/` files: `conductor_clobber_test.go`, `conductor_migration_recovery_test.go`, `conductor_stale_instructions_test.go`, `conductor_plain_read_test.go`, `conductor_shipped_templates_test.go`, `background_work_test.go`, `conductor_test.go`.

**Interfaces:**
- Consumes: nothing from earlier tasks (Task 4 needs this one). Unchanged: `var exchangeGeneratedFiles = exchangeGeneratedFile` (conductor.go:1772, called `(tmp, target)`; the platform `exchangeGeneratedFile(left, right string) error` is in generated_exchange_darwin.go and generated_exchange_linux.go), `func createFileIfAbsent(path string, content []byte, perm os.FileMode) (bool, error)` (:1750), `fsyncDir(dir string)` (inbox.go:115), `matchesAnyTemplateContent` (:1020), `sessionLog`; test helper `readFile(t *testing.T, p string) string` (switch_transcript_candidates_test.go:98).
- Produces (Task 4 uses these exact names):
  - `type generatedFileOutcome int`: `generatedFileCreated` (`= iota + 1`), `generatedFileUnchanged`, `generatedFileReplaced`, `generatedFileKept`, `generatedFileSymlink`. The outcome is 0 whenever err != nil.
  - `type generatedFileSpec struct { Path string; Current []byte; Perm os.FileMode; Replaceable func(existing []byte) bool; DisplacedTo string }`. `Perm` goes to today's O_EXCL create, so the umask applies as today. A replacement keeps the old file's mode bits, as today. An empty `DisplacedTo` keeps today's `.<base>.previous-<random>` name.
  - `var createGeneratedFile = createFileIfAbsent`: the create seam. There is no link(2) and no `linkGeneratedFile`.
  - `func replaceGeneratedFile(spec generatedFileSpec) (generatedFileOutcome, error)`.
  - `func writeGeneratedFileOrMigrate(path string, previousGenerations []string, current string, perm os.FileMode) error`: same signature and doc comment, now a wrapper.

- [ ] **Step 1: Write the failing tests** in `internal/session/conductor_generated_file_test.go`, package `session`, importing `bytes`, `errors`, `os`, `path/filepath`, `strings`, `testing`. No `t.Parallel`: tests swap `createGeneratedFile` and `exchangeGeneratedFiles` and restore them with `t.Cleanup`. Nothing here reaches launchctl or systemctl, so no Task 2 fake is needed. Helpers: `acceptAnyGenerated([]byte) bool` (always true); `onlyOldGenerated(existing []byte) bool` (`string(existing) == "old"`); `generatedArtifacts(t *testing.T, dir, pattern string) []string`, modelled on `staleInstructionsBackups` (conductor_stale_instructions_test.go:10): `t.Helper()`, return `filepath.Glob(filepath.Join(dir, pattern))`, `t.Fatal` on error. `dir` is a fresh `t.TempDir()` per test, and per `t.Run` row in the two table tests; `P` is `dir/.managed.previous`; `".managed.*"` matches every temporary (`.previous-*`) and fixed (`.previous`) copy of `dir/managed`.
  - `TestReplaceGeneratedFileCreatesOnlyWhenAbsent`: spec `{Path: dir/managed, Current: "new", Perm: 0o666, Replaceable: onlyOldGenerated}`. A racer row swaps `createGeneratedFile` for a func that runs `os.WriteFile(path, []byte(racer), 0o600)` (returning its error) and then returns `createFileIfAbsent(path, content, perm)`, which is `(false, nil)`. `umasked` is `Mode().Perm()` of a file made by `os.WriteFile(<another t.TempDir()>/ref, nil, 0o666)`. Every row: err nil, no `.managed.*` artifact.

    | row | racer | outcome | content of `dir/managed` | mode |
    |---|---|---|---|---|
    | absent | none (seam untouched) | `generatedFileCreated` | `new` | `umasked` |
    | racer wrote the current bytes | `new` | `generatedFileUnchanged` | `new` | 0o600 |
    | racer wrote other bytes | `racer` | `generatedFileKept` | `racer` | 0o600 |
  - `TestReplaceGeneratedFileRemovesPartialCreate`: `createGeneratedFile` swapped for a func that writes `"ne"` to path (0o600) and returns `(true, errors.New("disk full"))`; the spec above gives outcome 0, an error containing `disk full`, and `os.Lstat(dir/managed)` reports not-exist (a partial file left behind would read as the user's on every later run).
  - `TestReplaceGeneratedFileLeavesNonReplaceableTargetsAlone`: fixture `dir/target` = `old` (0o644), then the row's `dir/managed`; spec `{Path: dir/managed, Current: "new", Perm: 0o644, Replaceable: <row>, DisplacedTo: P}`. Every row: `os.SameFile` of `os.Lstat(path)` before and after; `bytes.Equal` of `os.ReadFile(path)` before and after; `dir/target` still `old`; no `.managed.*` artifact.

    | row | `dir/managed` | Replaceable | outcome | error contains |
    |---|---|---|---|---|
    | current bytes although the predicate accepts them | file `new`, 0o600 | `acceptAnyGenerated` | `generatedFileUnchanged` | (nil error) |
    | predicate refuses | file `user edited`, 0o600 | `onlyOldGenerated` | `generatedFileKept` | (nil error) |
    | symlink to replaceable bytes | symlink to `dir/target` | `acceptAnyGenerated` | `generatedFileSymlink` | (nil error) |
    | dangling symlink | symlink to `dir/missing` | `acceptAnyGenerated` | `generatedFileSymlink` | (nil error) |
    | directory | directory, 0o755 | `acceptAnyGenerated` | 0 | `refusing to replace unsafe generated-file target` |
  - `TestReplaceGeneratedFileKeepsOneDisplacedCopy`: `dir/managed` = `v1` (0o644); spec `{Path: dir/managed, Current: <step>, Perm: 0o644, Replaceable: acceptAnyGenerated, DisplacedTo: P}`. Current `v2`, then `v3`: each returns `generatedFileReplaced`, nil; the path holds the new bytes, `P` the previous ones (`v1`, then `v2`), and `generatedArtifacts(dir, ".managed.*")` is exactly `[P]`. Then `os.Remove(P)` and `os.Mkdir(P, 0o755)`; current `v4`: outcome 0 and an error containing the one `generatedArtifacts(dir, ".managed.previous-*")` match, which holds `v3`; the path holds `v4`.
  - `TestReplaceGeneratedFileConcurrentEditKeepsTemporaryCopy` (the exchange hook of `TestMigrationPreservesBothConcurrentEdits`, with `DisplacedTo` set): `dir/managed` = `old` (0o600). `exchangeGeneratedFiles` counts its calls; on the first it records `recovery = from` and runs `os.WriteFile(to, []byte("edit A"), 0o600)`, then always returns `exchangeGeneratedFile(from, to)`. Spec `{Path: dir/managed, Current: "new", Perm: 0o600, Replaceable: onlyOldGenerated, DisplacedTo: P}`: outcome 0 and an error containing `recovery`; `os.IsNotExist` of `os.Lstat(P)`'s error; `generatedArtifacts(dir, ".managed.*")` and `generatedArtifacts(dir, ".managed.previous-*")` are both exactly `[recovery]`; `recovery` holds `edit A`; the path holds `new`; calls == 1.
  - `TestReplaceGeneratedFileDisplacedToKeepsOpenEditorInode` (the pattern of `TestMigrationRetainsOpenEditorInode`): `dir/managed` = `old` (0o600); `editor := os.OpenFile(path, os.O_WRONLY, 0)`, closed by defer; the same spec, no hook: `generatedFileReplaced`, nil. Then `editor.Truncate(0)`, `editor.WriteAt([]byte("late editor contents"), 0)`, `editor.Sync()`: `P` holds `late editor contents`, the path holds `new`, and `generatedArtifacts(dir, ".managed.*")` is exactly `[P]`.

- [ ] **Step 2: Run them; expect a build failure.** GOTEST `-run '^TestReplaceGeneratedFile' ./internal/session/`. Expected: `FAIL	github.com/asheshgoplani/agent-deck/internal/session [build failed]`, with `undefined:` errors for Task 3's names (`generatedFileOutcome`, the outcome constants, `createGeneratedFile`, `replaceGeneratedFile`, `generatedFileSpec`; the compiler stops after ten). None of them exists at 0953876e.

- [ ] **Step 3: Implement `func replaceGeneratedFile(spec generatedFileSpec) (generatedFileOutcome, error)` in `internal/session/conductor.go`**, with the other new declarations, right after `exchangeGeneratedFiles`, and `"bytes"` imported. In this order:
  1. `os.Lstat(spec.Path)`. If missing, call `createGeneratedFile(spec.Path, spec.Current, spec.Perm)`: `(true, err)` (this call created the file, then its write or close failed) removes `spec.Path` and returns err, so no partial file outlives the failure (`createFileIfAbsent` leaves it); `(false, err)` returns err as is; `true` returns `generatedFileCreated`; `(false, nil)` means a file appeared, so `os.Lstat` once more and classify it below, once (no loop; if it vanished again, that Lstat error is returned as is). Any other Lstat error is returned as is.
  2. A symlink (dangling too) returns `generatedFileSymlink`. Any other non-regular file returns today's `refusing to replace unsafe generated-file target %q (%s)`. `existing, err := os.ReadFile(spec.Path)`: the error is returned as is. `bytes.Equal(existing, spec.Current)` returns `generatedFileUnchanged`, with no temp file and no exchange. `!spec.Replaceable(existing)` returns `generatedFileKept`.
  3. Otherwise run today's lines :1800-1863 (temp `.<base>.previous-*` with the old mode bits, write, sync, close, recheck, exchange, displaced-copy verification) with `[]byte(current)` → `spec.Current`, `path` → `spec.Path`, `matchesAnyTemplateContent(string(x), previousGenerations)` → `spec.Replaceable(x)` at the recheck (:1843) and at the displaced-copy check (:1860), and every return → `return 0, <today's error>`. An exchange error, an unsupported one (EINVAL, ENOTSUP) included, stays `replace generated target: %w`, the temp file is removed as today, and there is no fallback: on a filesystem without atomic exchange (an NFS home) the call fails, and Task 4 adds the remedy text.
  4. Replace the success tail (:1865-1866). When `spec.DisplacedTo != ""`, `os.Rename(tmpPath, spec.DisplacedTo)`. A failure returns `0, fmt.Errorf("moving displaced file %q to %q after replacing %q: %w", tmpPath, spec.DisplacedTo, spec.Path, err)`: the new bytes are already published, and the verified displaced copy stays at `tmpPath`, never unlinked. On success, `fsyncDir(filepath.Dir(spec.DisplacedTo))`. Then `sessionLog.Info("generated_instructions_migrated", slog.String("path", spec.Path), slog.String("previous_path", <DisplacedTo when set, else tmpPath>))` and return `generatedFileReplaced, nil`.

- [ ] **Step 4: Run the new tests; expect a pass.** GOTEST `-run '^TestReplaceGeneratedFile' ./internal/session/`. Expected: `ok  	github.com/asheshgoplani/agent-deck/internal/session`.

- [ ] **Step 5: Make `writeGeneratedFileOrMigrate` a wrapper.** Same signature and doc comment. The body returns only the error of `replaceGeneratedFile(generatedFileSpec{Path: path, Current: []byte(current), Perm: perm, Replaceable: func(existing []byte) bool { return matchesAnyTemplateContent(string(existing), previousGenerations) }})`, with no `DisplacedTo`. Keep `writeFileIfAbsent` and `createFileIfAbsent`: `InstallPolicyMD` (call at conductor.go:1946) and `writeCursorTrustFileExclusive` (cursor_trust.go:139) still use them, and `createGeneratedFile` defaults to `createFileIfAbsent`. Two things differ from today: a file that appears between the Lstat and the O_EXCL create is now classified (and migrated if it is an older generation) instead of left unexamined, and a create whose write fails removes its partial file.

- [ ] **Step 6: Run the new tests with the 23 unedited tests of the writer and its callers.** GOTEST `-run '^(TestReplaceGeneratedFile|TestWriteGeneratedFileOrMigrate|TestMigration(PreservesBothConcurrentEdits|RetainsOpenEditorInode|DefaultRerunPreservesCustomSymlinks)|TestGeneratedConductorInstructionsMigrateExactPriorTemplate|TestSetupConductorWithAgent_(PreservesEditsAndMetaOnRerun|Switch)|TestInstallSharedConductorInstructions_PreservesEditedRegularFile|TestInstallPolicyMD_PreservesEditedRegularFile|TestInstallSharedClaudeMD_|TestConductorReadsArePlainAndQuietGenerationMigrates|TestShippedConductorTemplatesAreRecognizedAsGenerated|TestShippedV11628SharedTemplateMigratesToHarnessControl|TestForkV11629SharedTemplateMigrates|TestNoOpRuleOnlySharedTemplateMigratesToHarnessControl|TestBackgroundWork2473_ConductorTemplateMigratesV1624)' ./internal/session/`. Expected: `ok  	github.com/asheshgoplani/agent-deck/internal/session`, not `[no tests to run]`. Because they are unedited, their pass shows the wrapper kept today's behaviour: a new inode that keeps the old 0o640 mode; the temp file removed after a failed exchange; exactly one exchange and a conflict error naming the retained copy; an open editor's writes landing in the displaced copy; symlinks kept; the template migrations; and shared and per-name instruction files created through `createGeneratedFile`.

- [ ] **Step 7: Commit.** GOFMT `internal/session/conductor.go internal/session/conductor_generated_file_test.go` (no output). `git add` those two files, then COMMIT with this subject and body, followed by the two attribution lines:

```text
refactor(session): add replaceGeneratedFile with a predicate and an outcome

The generated-file exchange now takes its "may I replace this" test as a
predicate, leaves a file already equal to the new bytes in place, reports
what it did, and can move the verified displaced copy to one fixed name.
A missing target is still created with O_EXCL; a file that appears
during that create is now classified instead of left unexamined, and a
create whose write fails removes its partial file. Otherwise
writeGeneratedFileOrMigrate keeps its signature and behaviour as a
wrapper.
```

### Task 4: bridge.py ownership by stamp

**Files:**
- Create: `internal/session/conductor_bridge_script.go`
- Modify: `internal/session/conductor.go`: delete `InstallBridgeScript` and its doc comment (`:2226-2244` at `6333ffde`; Task 3 edits the file first, so search for `func InstallBridgeScript() error`). Its body moves to the new file.
- Modify: `internal/session/conductor_bridge_embed.go:7-8` (doc comment, Step 9)
- Modify: `conductor/README.md:14-17` (Step 9)
- Test: `internal/session/conductor_bridge_script_test.go` (new)

**Interfaces:**
- Consumes (Task 3; G5: 4 needs 3): `type generatedFileSpec struct { Path string; Current []byte; Perm os.FileMode; Replaceable func(existing []byte) bool; DisplacedTo string }`; `func replaceGeneratedFile(spec generatedFileSpec) (generatedFileOutcome, error)` → `generatedFile{Created,Unchanged,Replaced,Kept,Symlink}`, 0 whenever err != nil.
  - Relied on: `bytes.Equal(existing, Current)` is tested before `Replaceable`; a missing target is created through `var createGeneratedFile = createFileIfAbsent` (O_EXCL, mode `Perm` less the umask, as today's `os.WriteFile`; no link); a symlink, even dangling, is Symlink; another non-regular target fails with `refusing to replace unsafe generated-file target %q (%s)`; a failed exchange removes its temp file and returns `replace generated target: %w`; a concurrent edit found in the displaced copy returns an error naming the retained `.bridge.py.previous-*` copy and leaves `DisplacedTo` alone; a replacement keeps the old mode bits.
- Consumes (existing): `var exchangeGeneratedFiles = exchangeGeneratedFile` (conductor.go:1772; platform `exchangeGeneratedFile` in generated_exchange_{linux,darwin}.go), `atomicWriteFile(path string, data []byte, perm os.FileMode) error` (worker_scratch.go:1056), `ConductorDir() (string, error)` (conductor.go:690), `var conductorBridgePy string` (conductor_bridge_embed.go:17). Test helpers: `setupSessionXDGPathEnv(t)` (xdg_paths_test.go:11), `fmtHash(content string) string` (conductor_clobber_test.go:213), `writeTestFile(t, path, content string)` (mode 0644; multi_repo_worktree_test.go:150), `readFileString(t, p string) string` ("" when missing; project_mcp_git_stdin_test.go:470), `assertNotExist(t, path)` (conductor_migrate_dir_test.go:39).
- Produces (contract names, verbatim): unexported `bridgeStampName = ".bridge.py.sha256"`, `bridgePreviousName = ".bridge.py.previous"`; `type BridgeScriptOutcome string` with typed `BridgeScriptInstalled = "installed"`, `BridgeScriptUpdated = "updated"`, `BridgeScriptUnchanged = "unchanged"`, `BridgeScriptKept = "kept"`; `type BridgeScriptResult struct { Outcome BridgeScriptOutcome; Notice string }` (Notice only for kept); `const BridgeKeptNotice`, `const BridgeSymlinkNotice` (Step 3); `type BridgeKeptError struct { Notice string }` with pointer-receiver `Error() string` and `BridgeKeptNotice() string`, asserted by `var _ interface{ BridgeKeptNotice() string } = (*BridgeKeptError)(nil)`; `func InstallBridgeScriptOutcome() (BridgeScriptResult, error)`; `func installBridgeScriptAt(dir string, embedded []byte) (BridgeScriptResult, error)`; `func InstallBridgeScript() error` (today's signature; main.go:73 injects it); `func RemoveBridgeScript() (removed bool, err error)`. Task 5 reads the marker interface; Tasks 9 and 11 call `InstallBridgeScriptOutcome` (Task 11 also `installBridgeScriptAt` and both name constants); Task 10 calls `RemoveBridgeScript`. Internal: `bridgeStamp(content []byte) string`, `readBridgeStamp(dir string) string`, `removeBridgeScriptAt(dir string, embedded []byte) (bool, error)`.

**Callers until Tasks 5, 9 and 11.** setup (`cmd/agent-deck/conductor_cmd.go:738`), migrate-dir (`internal/session/conductor_migrate_dir.go:372`) and update (main.go:73) compile unchanged and never overwrite a kept bridge. With a kept bridge: setup exits 1 with `Error installing bridge.py: bridge.py has local changes; left unchanged`; migrate-dir logs `conductor_migrate_dir_bridge_reinstall_failed`; update still writes `bridge.py.backup` (internal/update/update.go:1115-1124) and prints `Warning: Failed to update bridge.py: ...` (main.go:4432, :4529). No existing test runs these with a kept bridge; the `TestMigrateConductorDir_*` tests have no bridge fixture and list no directory, so their targets just gain an unchecked `.bridge.py.sha256`.

**Test constraints.** No `t.Parallel()` in the new file: two tests swap `exchangeGeneratedFiles` and two call `setupSessionXDGPathEnv` (`t.Setenv`). Nothing reaches launchctl or systemctl (no Task 2 fake, G3) and no fixture needs an unwritable path (no root skip, G4).

- [ ] **Step 1: Write the failing install tests** in `internal/session/conductor_bridge_script_test.go` (package `session`). Helpers:
```go
const bridgeV1, bridgeV2 = "# bridge v1\n", "# bridge v2\n" // bridgeV2 plays the embedded bridge
func bridgeStampOf(content string) string { return fmtHash(content) + "\n" }
// writeBridgeFixtures writes each non-empty entry (name -> content) into dir with writeTestFile.
func writeBridgeFixtures(t *testing.T, dir string, files map[string]string)
```
  - `TestBridgeScript_InstallOwnership`: per row, fixtures in `t.TempDir()`, then `installBridgeScriptAt(dir, []byte(bridgeV2))` returns a nil error and exactly the result; bridge.py, the stamp and `.bridge.py.previous` hold the "after" values (— = must not exist) and `os.ReadDir(dir)` holds only those files (no temp file, no `.bridge.py.previous-<n>`). Row 1 also wants bridge.py's `Mode().Perm()` to equal that of a file made by `os.WriteFile(<another t.TempDir()>/ref, nil, 0o755)` (Perm 0o755 less the runner's umask, Task 3's check). S(x) = `bridgeStampOf(x)`; edited = `"# my edits\n"`. Row 6 is hera's case: an unedited pre-stamp bridge looks like a hand edit.

    | row (subtest name) | bridge.py | stamp | result | bridge.py after | stamp after | .previous after |
    |---|---|---|---|---|---|---|
    | missing: installed | — | — | `{BridgeScriptInstalled, ""}` | V2 | S(V2) | — |
    | equal, no stamp: stamped | V2 | — | `{BridgeScriptUnchanged, ""}` | V2 | S(V2) | — |
    | equal, stale stamp: restamped | V2 | S(V1) | `{BridgeScriptUnchanged, ""}` | V2 | S(V2) | — |
    | equal and stamped: no exchange | V2 | S(V2) | `{BridgeScriptUnchanged, ""}` | V2 | S(V2) | — |
    | stamped older: updated | V1 | S(V1) | `{BridgeScriptUpdated, ""}` | V2 | S(V2) | V1 |
    | pre-fix unstamped older bridge: kept | V1 | — | `{BridgeScriptKept, BridgeKeptNotice}` | V1 | — | — |
    | stamp present but not matching: kept | edited | S(V1) | `{BridgeScriptKept, BridgeKeptNotice}` | edited | S(V1) | — |
    | stamp without its newline: kept | V1 | `fmtHash(V1)` | `{BridgeScriptKept, BridgeKeptNotice}` | V1 | `fmtHash(V1)` | — |
  - `TestBridgeScript_SecondUpgradeKeepsOnePrevious`: one dir, successive `installBridgeScriptAt(dir, embedded)` each with a nil error → outcome, then `.bridge.py.previous`: V1 → installed, —; V2 → updated, V1; `"# bridge v3\n"` → updated, V2; `"# bridge v3\n"` again → unchanged, V2 (a rerun keeps the real previous). Then `filepath.Glob(dir/.bridge.py.previous-*)` is empty.
  - `TestBridgeScript_SymlinkKeptDirectoryRefused`: bridge.py a symlink to a V1 file in another `t.TempDir()` → `{BridgeScriptKept, BridgeSymlinkNotice}`, nil; still a symlink, target still V1, `assertNotExist` on the stamp. bridge.py a directory → err contains `refusing to replace unsafe generated-file target`.
  - `TestBridgeScript_UnsupportedExchangeKeepsBridge` (G7: no fallback; this pins the remedy): fixtures V1 + S(V1); `exchangeGeneratedFiles` returns the row's error (restored by `t.Cleanup`). Subtests `EINVAL` (`syscall.EINVAL`, renameat2 on NFS) and `ENOTSUP` (`syscall.ENOTSUP`, renamex_np without RENAME_SWAP): `errors.Is(err, row)` and err contains `move bridge.py aside`. Subtest `other failure` (`errors.New("injected exchange failure")`): `errors.Is` holds, no `move bridge.py aside`. Every row: bridge.py V1, stamp S(V1), `os.ReadDir` exactly 2 entries.
  - `TestBridgeScript_ConcurrentEditWritesNoStamp`: fixtures V1 + S(V1); the swapped `exchangeGeneratedFiles` (pattern of `TestMigrationPreservesBothConcurrentEdits`, restored by `t.Cleanup`) records `from`, writes `"# edit A\n"` to `to`, then returns `exchangeGeneratedFile(from, to)`. Want the zero result and an error containing the recorded path and not `move bridge.py aside`; bridge.py V2; the recorded copy `"# edit A\n"`; the stamp still S(V1) (no stamp written); `.bridge.py.previous` absent.
  - `TestBridgeScript_KeptErrorAndWrappers`: `(&BridgeKeptError{Notice: n}).Error()` is `bridge.py has local changes; left unchanged` for `BridgeKeptNotice` and `bridge.py is a symlink; left unchanged` for `BridgeSymlinkNotice`; `BridgeKeptNotice()` returns n. Then `setupSessionXDGPathEnv(t)` (TestMain's HOME is package-wide, so a bridge there would leak): `InstallBridgeScriptOutcome()` → installed, `ConductorDir()`/bridge.py == `conductorBridgePy`; `InstallBridgeScript()` → nil; after `writeTestFile(bridge, "# my bridge\n")`, `errors.As(fmt.Errorf("failed to install bridge.py: %w", InstallBridgeScript()), &kept)` (UpdateBridgePy's wrap) holds with `kept.BridgeKeptNotice() == BridgeKeptNotice`, and bridge.py is still `"# my bridge\n"`.

- [ ] **Step 2: Run them; confirm they fail.** GOTEST `-run '^TestBridgeScript_' ./internal/session/`. Expected: `FAIL	github.com/asheshgoplani/agent-deck/internal/session [build failed]`, every error an `undefined:` for one of this task's names (e.g. `undefined: BridgeScriptResult`; the compiler stops after ten).

- [ ] **Step 3: Implement the install API in `internal/session/conductor_bridge_script.go`** and delete the old `InstallBridgeScript` from conductor.go. Declare every Produces name except `RemoveBridgeScript` (Step 7; declaring it now would keep Step 6 from failing). Fixed text:
```go
const BridgeKeptNotice = "[skip] bridge.py has local changes; left unchanged. To take the shipped bridge, move it aside and re-run 'agent-deck conductor setup <name>'."
const BridgeSymlinkNotice = "[skip] bridge.py is a symlink; left unchanged."
```
  - `(*BridgeKeptError).Error()`: `bridge.py is a symlink; left unchanged` when `Notice == BridgeSymlinkNotice`, else `bridge.py has local changes; left unchanged`; `BridgeKeptNotice()` returns `Notice`; add the `var _ interface{ BridgeKeptNotice() string } = (*BridgeKeptError)(nil)` assertion.
  - `bridgeStamp(content []byte) string`: 64 lower-case hex digits of sha256 (crypto/sha256, encoding/hex), then `"\n"`. `readBridgeStamp(dir string) string`: the stamp's bytes only when `os.Lstat` shows a regular file; missing, unreadable, symlink or special reads "" (a FIFO is never opened). Compare exactly, no trimming; an unreadable stamp can only turn an update into kept.
  - `installBridgeScriptAt`: read the stamp once, call `replaceGeneratedFile(generatedFileSpec{Path: dir/bridge.py, Current: embedded, Perm: 0o755, Replaceable: func(existing []byte) bool { return stamp == bridgeStamp(existing) }, DisplacedTo: dir/bridgePreviousName})`, and map the outcome (`BridgeScript` prefix omitted):

    | outcome | write the stamp? | result |
    |---|---|---|
    | Created | yes | `{Installed, ""}` |
    | Replaced | yes | `{Updated, ""}` |
    | Unchanged | only when `stamp != bridgeStamp(embedded)` | `{Unchanged, ""}` |
    | Kept | never | `{Kept, BridgeKeptNotice}` |
    | Symlink | never | `{Kept, BridgeSymlinkNotice}` |
  - Stamp with `atomicWriteFile(dir/bridgeStampName, []byte(bridgeStamp(embedded)), 0o644)`, always after the bridge write; a failure returns the zero result and `fmt.Errorf("writing %s: %w", bridgeStampName, err)`. A `replaceGeneratedFile` error returns the zero result and, when `errors.Is(err, errors.ErrUnsupported) || errors.Is(err, syscall.EINVAL)` (ENOTSUP, EOPNOTSUPP, ENOSYS; EINVAL from renameat2 on NFS), `fmt.Errorf("replacing bridge.py: %w; to take the shipped bridge here, move bridge.py aside and re-run", err)`; any other error `fmt.Errorf("writing bridge.py: %w", err)`.
  - `InstallBridgeScriptOutcome()`: the moved body (a `ConductorDir` error as is; an `os.MkdirAll(dir, 0o755)` failure as `failed to create conductor dir: %w`), then `installBridgeScriptAt(dir, []byte(conductorBridgePy))`. `InstallBridgeScript()`: its error; `&BridgeKeptError{Notice: res.Notice}` when `res.Outcome == BridgeScriptKept`; else nil.
  - Take no lock: MigrateConductorDir already holds the non-reentrant base lock around its call (conductor_migrate_dir.go:281-285, :372).

- [ ] **Step 4: Run; confirm they pass.** GOTEST `-run '^(TestBridgeScript_|TestMigrateConductorDir_)' ./internal/session/`. Expected: `ok  	github.com/asheshgoplani/agent-deck/internal/session`; the migrate-dir tests reach `InstallBridgeScript()` through MigrateConductorDir.

- [ ] **Step 5: Write the failing removal test** `TestBridgeScript_RemoveOnlyOwnedBridge` in the same file. Each row's fixtures (— = not created): the stamp, `.bridge.py.previous` = V1, `bridge.py.backup` = `"# backup\n"`, and bridge.py (the link row: a symlink to a file in another `t.TempDir()` holding those bytes). `removeBridgeScriptAt(dir, []byte(bridgeV2))` returns a nil error and the row's `removed`. Removed: bridge.py, stamp and `.bridge.py.previous` all absent. Not removed: all three unchanged, and the link row's bridge.py still a symlink. Every row: `bridge.py.backup` still `"# backup\n"`.

    | row (subtest name) | bridge.py | stamp | removed |
    |---|---|---|---|
    | equal to embedded, stale stamp | V2 | S(V1) | true |
    | stamped older | V1 | S(V1) | true |
    | customized | `"# my bridge\n"` | S(V1) | false |
    | pre-fix unstamped older bridge | V1 | — | false |
    | symlink to the embedded bytes | symlink → V2 | S(V2) | false |
    | missing | — | S(V1) | false |

  Subtest `RemoveBridgeScript resolves ConductorDir`: `setupSessionXDGPathEnv(t)`, `InstallBridgeScriptOutcome()`, then `RemoveBridgeScript()` → `(true, nil)`, and `assertNotExist` on `ConductorDir()`'s bridge.py and stamp.

- [ ] **Step 6: Run it; confirm it fails.** Step 2's command. Expected: `[build failed]` with `undefined: removeBridgeScriptAt` and `undefined: RemoveBridgeScript`.

- [ ] **Step 7: Implement `removeBridgeScriptAt(dir string, embedded []byte) (bool, error)` and `RemoveBridgeScript() (removed bool, err error)`** in `conductor_bridge_script.go`:
  - `os.Lstat(dir/bridge.py)`: not exist → `(false, nil)`, nothing removed (an orphan stamp or `.bridge.py.previous` stays); other error → `false, fmt.Errorf("checking bridge.py: %w", err)`; not a regular file (symlink, directory, FIFO) → `(false, nil)`, untouched and never read through (design 5.1: a symlink is external).
  - Read it (`false, fmt.Errorf("reading bridge.py: %w", err)` on failure). Owned iff `bytes.Equal(content, embedded) || readBridgeStamp(dir) == bridgeStamp(content)`; otherwise `(false, nil)`, so a customized or pre-fix unstamped bridge is kept.
  - Owned: remove bridge.py (`false, fmt.Errorf("removing bridge.py: %w", err)`), then the stamp and `.bridge.py.previous`, ignoring not-exist (any other failure → `true, fmt.Errorf("removing %s: %w", name, err)`); then `(true, nil)`. Never touch `bridge.py.backup` or a `.bridge.py.previous-<n>` copy.
  - `RemoveBridgeScript`: `ConductorDir()` (error as is; no MkdirAll), then `removeBridgeScriptAt(dir, []byte(conductorBridgePy))`.

- [ ] **Step 8: Run; confirm they pass.** Step 4's command. Expected: `ok  	github.com/asheshgoplani/agent-deck/internal/session`.

- [ ] **Step 9: Document the rule** (exact copy; rewrap at 80 columns).
  - `internal/session/conductor_bridge_embed.go:7-8` ("self-contained: InstallBridgeScript / update.UpdateBridgePy write these exact bytes to <data>/conductor/bridge.py at setup/update time.") becomes the `//` comment: "self-contained: setup, update and migrate-dir write these exact bytes to <data>/conductor/bridge.py only while that file is missing, byte-identical, or unchanged since agent-deck stamped it (.bridge.py.sha256); a customized or symlinked bridge.py is kept (see conductor_bridge_script.go)."
  - `conductor/README.md:14-17` (from "It is embedded" to "materialize the user-facing runtime copy at:"; the path block after it stays) becomes:
  > It is embedded into the agent-deck binary (`//go:embed`, see `internal/session/conductor_bridge_embed.go`). `agent-deck conductor setup`, `agent-deck update` and `agent-deck conductor migrate-dir` write it only while the runtime copy is missing, byte-identical to the embedded bridge, or unchanged since agent-deck stamped it: the sha256 of each copy written goes to `.bridge.py.sha256` beside it, and the copy it replaces is kept as `.bridge.py.previous`. A customized or symlinked `bridge.py` is kept, as is an unedited copy from a release that predates the stamp; move it aside and re-run `agent-deck conductor setup <name>` to take the shipped bridge. The runtime copy lives at:

- [ ] **Step 10: Commit.** GOFMT `internal/session/conductor_bridge_script.go internal/session/conductor_bridge_script_test.go internal/session/conductor.go internal/session/conductor_bridge_embed.go` (expect no output); `git add` those four files and `conductor/README.md`; COMMIT with subject `fix(session): replace bridge.py only while it matches agent-deck's stamp` and this body (exact; rewrap at 72 columns): "InstallBridgeScript wrote the embedded bridge over any bridge.py, including a hand-edited one. Record the sha256 of each bridge agent-deck writes in .bridge.py.sha256 and replace bridge.py only while it still matches that stamp, keeping the replaced copy as .bridge.py.previous; a missing bridge is installed and an identical one only stamped. A customized or symlinked bridge is left unchanged and reported as kept: InstallBridgeScript keeps its signature and returns *BridgeKeptError for it. Without atomic exchange (an NFS home) a stamped bridge is not replaced, and the error says to move bridge.py aside. RemoveBridgeScript deletes only a bridge that agent-deck owns."

### Task 5: `agent-deck update` honours a kept bridge

**Files:**
- Modify: `internal/update/update.go:1074-1133` (`UpdateBridgePy`). Line numbers are at base `6333ffde`; no earlier task touches internal/update.
- Test: `internal/update/update_test.go` (imports `:3-15`; `TestUpdateBridgePy_UsesInjectedInstallerAndBacksUpExistingFile` `:234-262`; `TestUpdateBridgePy_HonorsConductorDirOverride` `:324-373`; new code after `:403`, before `TestNormalizeReleaseTag` `:405`).
- Unchanged: `cmd/agent-deck/main.go`, `cmd/agent-deck/update_cli.go`, all docs. No user doc mentions update's `bridge.py.backup`; the two comments naming `update.UpdateBridgePy` (`internal/session/conductor_bridge_embed.go:7-8`, `conductor/README.md:14-17`) are Task 4's.

**Interfaces:**
- Consumes (Task 4; G5: 5 needs 4, reached only at run time): `type BridgeKeptError struct { Notice string }` with method `BridgeKeptNotice() string`; `func InstallBridgeScript() error` returns it unwrapped, as `&BridgeKeptError{Notice: BridgeKeptNotice}` or `&BridgeKeptError{Notice: BridgeSymlinkNotice}`. Task 4 pins the method with `var _ interface{ BridgeKeptNotice() string } = (*BridgeKeptError)(nil)` in `internal/session/conductor_bridge_script.go`; `main.go:73` injects the installer through `update.SetBridgeScriptInstaller` (unchanged).
  internal/session imports internal/update, so update matches the marker by its method and never imports session. This task's tests define their own marker and build without Task 4.
- Consumes (existing): `func SetBridgeScriptInstaller(installer func() error)` (update.go:82), `func SetConductorDirResolver(resolver func() (string, error))` (:88), test helper `func isolateUpdatePaths(t *testing.T) string` (cache_xdg_test.go:12; returns `<tmp>/home`, whose legacy conductor dir `<home>/.agent-deck/conductor` the default resolver picks when it exists).
- Produces: `func UpdateBridgePy() error`, signature unchanged.
  - It never reads, writes or removes `bridge.py.backup`.
  - When the installer's error satisfies `interface{ BridgeKeptNotice() string }` (errors.As, checked BEFORE the `failed to install bridge.py: %w` wrap), it prints the notice with `fmt.Println` and returns nil. A kept bridge thus prints `Updating bridge.py...`, then the notice, and no `✓ bridge.py updated!` (which still prints for installed, updated and unchanged).
  - Every other installer error, Task 4's unsupported-exchange remedy error included (G7), keeps the wrap (the kept test's last assertion pins it).
  - Callers `main.go:4431`, `:4528` and `runUnattendedUpdate` (`update_cli.go:355`, wired at `:417`) act only on a non-nil error, so none changes; on the unattended path the notice reaches stdout, not update.log.
- Produces (test-only, update_test.go): `func captureStdout(t *testing.T, fn func()) string`; `type keptBridgeError struct{ notice string }`.

- [ ] **Step 1: Write the failing "no backup" tests** in `internal/update/update_test.go`. No test in this task calls t.Parallel (they swap package seams and os.Stdout).
  - Rename `TestUpdateBridgePy_UsesInjectedInstallerAndBacksUpExistingFile` (:234; no other file names it, `.claude/release-tests.yaml` included) to `TestUpdateBridgePy_UsesInjectedInstallerAndWritesNoBackup`. Replace :254-257 (the `.backup` must equal `legacyContent`) with `assert.NoFileExists(t, bridgePath+".backup", "update must not write bridge.py.backup")`.
  - In `TestUpdateBridgePy_HonorsConductorDirOverride`, replace :356-359 with `// The refresh lands under the OVERRIDE dir, with no .backup beside it.` and `assert.NoFileExists(t, overrideBridge+".backup", "update must not write bridge.py.backup")`. In its doc comment (:332-333), "asserts the backup + refresh land there, not under the default conductor root." becomes "asserts the refresh lands there, not under the default conductor root, and that no bridge.py.backup is written."
  - After :403 add `TestUpdateBridgePy_LeavesExistingBackupUntouched`, doc: "a bridge.py.backup left by a pre-fix update may hold the user's own bridge, so a refresh must not rewrite it." In the legacy conductor dir write `bridge.py` = `"# shipped bridge\n"` (0o755) and `bridge.py.backup` = `"# custom bridge saved by a pre-fix update\nprint('keep me')\n"` (0o644). The installer writes `"# newer shipped bridge\n"` (0o755) to bridge.py; `t.Cleanup(func() { SetBridgeScriptInstaller(nil) })`. Assert `require.NoError(t, UpdateBridgePy())`, then with `assert.Equal`, first that the `.backup` still holds its bytes (`"update must never touch an existing bridge.py.backup"`), then that bridge.py equals `"# newer shipped bridge\n"` (`"the installer must have run"`).
  - Leave `TestUpdateBridgePy_NoOpLeavesExistingBackupUntouched` (:285-322) unchanged.

- [ ] **Step 2: Run them; expect three failures.** GOTEST `-run '^TestUpdateBridgePy_' ./internal/update/`. Expected FAIL, because update.go:1114-1124 copies bridge.py over `bridge.py.backup` before calling the installer:
  - `..._UsesInjectedInstallerAndWritesNoBackup`: `file ".../home/.agent-deck/conductor/bridge.py.backup" exists`.
  - `..._HonorsConductorDirOverride`: `file ".../conductor homes/conductor/bridge.py.backup" exists`.
  - `..._LeavesExistingBackupUntouched`, at its first `assert.Equal`; testify prints on separate lines `Not equal: `, `expected: "# custom bridge saved by a pre-fix update\nprint('keep me')\n"` and `actual  : "# shipped bridge\n"`.
  - The other four (`NoConductorDir`, `NonCLICallerDoesNotHardFail`, `NoOpLeavesExistingBackupUntouched`, `OverrideGuardSkipsWhenOverrideMissing`) pass.

- [ ] **Step 3: Remove the backup from `UpdateBridgePy`** in `internal/update/update.go`.
  - Delete the "Backup existing bridge.py if present" block (:1114-1124), and with it the errors `failed to read existing bridge.py` and `failed to backup bridge.py` (no test, caller or golden names them).
  - Delete `bridgePath := filepath.Join(conductorDir, "bridge.py")` (:1091); its only uses were in that block, so keeping it fails with `declared and not used: bridgePath`. `filepath` stays imported (:187, :217, :856, :863, :886, :1062).
  - :1077-1078: "the existence guard, the .backup, and the refresh all target" becomes "the existence guard and the refresh both target".
  - Doc comment (:1074-1075): add that UpdateBridgePy never reads, writes or removes bridge.py.backup, because a .backup left by an older update may hold the user's own bridge.
  - Leave the no-op guard comment (:1099-1107) as it is: it stays true, and the doc of `TestUpdateBridgePy_NoOpLeavesExistingBackupUntouched` (update_test.go:286-288) quotes it.

- [ ] **Step 4: Run Step 2's command.** Expected: `ok  	github.com/asheshgoplani/agent-deck/internal/update`; all seven `TestUpdateBridgePy_*` pass.

- [ ] **Step 5: Write the failing kept-bridge test.** Add `"bytes"`, `"errors"` and `"fmt"`, sorted, to the standard-library imports (:4-11). After `TestUpdateBridgePy_LeavesExistingBackupUntouched` add:
  - `captureStdout`, copied verbatim, comments included, from `cmd/agent-deck/cursor_hooks_cmd_test.go:47-77` (package main, so not importable). It needs only bytes, os and testing.
  - The marker and the test:
    ```go
    // keptBridgeError stands in for session.BridgeKeptError, which this package
    // cannot import (internal/session imports internal/update).
    type keptBridgeError struct{ notice string }

    func (e *keptBridgeError) Error() string { return "bridge.py has local changes; left unchanged" }

    func (e *keptBridgeError) BridgeKeptNotice() string { return e.notice }

    func TestUpdateBridgePy_KeptBridgePrintsNoticeAndSucceeds(t *testing.T) {
    	condDir := filepath.Join(isolateUpdatePaths(t), ".agent-deck", "conductor")
    	require.NoError(t, os.MkdirAll(condDir, 0o755))
    	bridgePath := filepath.Join(condDir, "bridge.py")
    	custom := "# customized bridge\nprint('qna hook')\n"
    	require.NoError(t, os.WriteFile(bridgePath, []byte(custom), 0o755))
    	const notice = "[skip] bridge.py has local changes; left unchanged. To take the shipped bridge, move it aside and re-run 'agent-deck conductor setup <name>'."
    	// Wrapped once: a bare type assertion misses it, errors.As finds it.
    	SetBridgeScriptInstaller(func() error { return fmt.Errorf("wrapped: %w", &keptBridgeError{notice: notice}) })
    	t.Cleanup(func() { SetBridgeScriptInstaller(nil) })

    	var err error
    	out := captureStdout(t, func() { err = UpdateBridgePy() })
    	require.NoError(t, err, "a kept bridge is not an update failure")
    	assert.Contains(t, out, notice+"\n")
    	assert.NotContains(t, out, "✓ bridge.py updated!")
    	assert.NoFileExists(t, bridgePath+".backup")
    	got, readErr := os.ReadFile(bridgePath)
    	require.NoError(t, readErr)
    	assert.Equal(t, custom, string(got))

    	// Only a kept bridge becomes a notice; any other installer error still fails.
    	SetBridgeScriptInstaller(func() error { return errors.New("disk full") })
    	assert.EqualError(t, UpdateBridgePy(), "failed to install bridge.py: disk full")
    }
    ```

- [ ] **Step 6: Run it; expect it to fail.** GOTEST `-run '^TestUpdateBridgePy_KeptBridgePrintsNoticeAndSucceeds$' ./internal/update/`. Expected: FAIL at `require.NoError` with `Received unexpected error:` then `failed to install bridge.py: wrapped: bridge.py has local changes; left unchanged`, because update.go wraps every installer error with %w.

- [ ] **Step 7: Recognize a kept bridge in `UpdateBridgePy`.** Inside `if err := bridgeScriptInstaller(); err != nil {`, ahead of the wrap, declare `var kept interface{ BridgeKeptNotice() string }` under a one-line comment (matched by method because internal/update cannot import internal/session); when `errors.As(err, &kept)`, call `fmt.Println(kept.BridgeKeptNotice())` and `return nil`. This is the idiom of `internal/session/restart_result.go:99-100` and `internal/update/plan.go:50-51`; `errors` is already imported (update.go:10). Add to the doc comment that when the installer keeps the bridge, UpdateBridgePy prints the installer's notice and returns no error.

- [ ] **Step 8: Run Step 6's command, then the package.** GOTEST `./internal/update/` for the package (its launchd tests all inject a fake Runner, so no real launchctl is reached). Expected both times: `ok  	github.com/asheshgoplani/agent-deck/internal/update`.

- [ ] **Step 9: Commit.** GOFMT `internal/update/update.go internal/update/update_test.go` (no output). `git add internal/update/update.go internal/update/update_test.go`, then COMMIT with this message, followed by the two attribution lines:
  ```text
  fix(update): drop bridge.py.backup and report a kept bridge as a notice

  UpdateBridgePy copied bridge.py over bridge.py.backup before every
  refresh, so a second update replaced a saved customized bridge with the
  shipped one. session.InstallBridgeScript now keeps the bridge it
  replaces as .bridge.py.previous. It reports a bridge with local changes,
  or a symlink, through an error with a BridgeKeptNotice() method.
  UpdateBridgePy now prints that notice and returns nil, so update no
  longer warns "failed to install bridge.py". It never touches an
  existing bridge.py.backup, which may hold the user's own bridge.
  ```

### Task 6: Launch agents: ownership, safe replace and remove

**Files:**
- Create: `internal/session/service_files.go` (no build tag; imports `golang.org/x/sys/unix` as `image_upload.go:14` does) and `internal/session/service_files_test.go` (package `session`; no `t.Parallel`, the tests swap seams).
- Modify: `internal/session/conductor.go`. Tasks 2-4 edit it first, so find each function by name (base 0953876e lines, the same at 6333ffde): `installBridgeDaemonLaunchd` (:2779-2803), `installTransitionNotifierLaunchd` (:2885-2923), `uninstallBridgeDaemonLaunchd` (:2974-2984), `uninstallTransitionNotifierLaunchd` (:3015-3025), `installHeartbeatDaemonLaunchd` (:3145-3167), `uninstallHeartbeatDaemonLaunchd` (:3274-3288); delete `RemoveHeartbeatPlist` (:1416-1426).
- Test (existing; they stay green and unchanged): the four `TestInstallTransitionNotifierLaunchd_*` (`internal/session/conductor_bg_guard_test.go`); `TestLaunchdHeartbeatInstallWritesFreshPlist` (:172), `TestLaunchdHeartbeatUninstallRefusesFailedUnload` (:194) and `TestUninstallHeartbeatDaemon_UnknownAgentDisablesAndPreservesMeta` (:353, as Task 2 converted it) in `internal/session/conductor_heartbeat_tick_test.go`; `TestRuntimeLifecycle_ConductorTeardown*` (`cmd/agent-deck/conductor_runtime_lifecycle_test.go:209`, `:405`), whose nix gate needs a missing heartbeat plist to make no launchctl call.

**Interfaces:**
- Consumes (Task 2): `func installServiceManagerFake(t *testing.T) *serviceManagerFake` with `Calls() []string`, `MutatingCalls() []string`, `FailOn(tool, verb string, err error)` (fails every matching call) and `FailNext(tool, verb string, err error)` (fails only the next one); failed calls are still recorded. The fake swaps the seams `runLaunchctl = func(args ...string) error` (every load and unload of the six functions) and `launchctlOutput = func(args ...string) ([]byte, error)` (the notifier guard's `managername` and `list`). Existing: `withGuardSeams(t *testing.T, managerName string, running bool) *[]string` (conductor_bg_guard_test.go:15), `testutil.SkipIfRoot(t testing.TB, why string)` (internal/testutil/root.go:14).
- Produces (contract): `type ExternalServiceError struct { Label, Path, Reason string }`, `Error()` = `<Label>: managed externally (<Reason>)`, Reason `"symlink"` or `"read-only"`; `var serviceFileWritable = func(path string) bool { return unix.Access(path, unix.W_OK) == nil }`; `var renameServiceFile = os.Rename`; `func serviceFileExternal(label, path string) (*ExternalServiceError, error)`; `func replaceLaunchAgent(label, plistPath string, content []byte) error`; `func removeLaunchAgent(label, plistPath string, strictUnload bool) error`. The six launchd functions return the `*ExternalServiceError` itself as their error; exported signatures do not change.
- Produces for later tasks, which reuse them and never redeclare them: `func writeServiceTemp(target string, content []byte, perm os.FileMode) (string, error)` (service_files.go; Task 7); in service_files_test.go `writeServiceFixture(t *testing.T, path, content string, perm os.FileMode)` (Tasks 7 and 11), `wantFileBytes(t *testing.T, path, want string)`, `wantNoTemps(t *testing.T, dir string)`, `wantCalls(t *testing.T, got []string, want ...string)` and `wantExternal(t *testing.T, err error, label, path, reason string)` (Task 7). Local to this task but package-level, so no later task may redeclare them: `testAgentLabel`, `testPlist`, `wantPerm`, `observeLaunchctl`, `serviceEntry(path string) string`, `var externalServiceKinds []struct{ name, reason string; setup func(t *testing.T, p string) }`.

- [ ] **Step 1: Write the failing ownership tests** in service_files_test.go. Paths live under `t.TempDir()`; HOME stays the TestMain one. Add `const testAgentLabel = "com.agentdeck.test-agent"` and `writeServiceFixture` (MkdirAll the parent 0o755, WriteFile, Chmod to perm).
  - `TestExternalServiceError_Error`: `(&ExternalServiceError{Label: LaunchdPlistName, Path: "/p.plist", Reason: "read-only"}).Error()` is `"com.agentdeck.conductor-bridge: managed externally (read-only)"`.
  - `TestServiceFileExternal`: in one dir, `writable.plist` 0644, `readonly.plist` 0444, `link.plist` → `writable.plist`, `dangling.plist` → a missing name, `dir.plist` a directory. A row may stub `serviceFileWritable` (restored in t.Cleanup); it calls `ext, err := serviceFileExternal(testAgentLabel, path)` and checks `(err != nil) == wantErr`, then `ext == nil` when want is `""`, else `*ext == ExternalServiceError{Label: testAgentLabel, Path: path, Reason: want}`:

| row | path | serviceFileWritable | want | wantErr |
|---|---|---|---|---|
| missing | `absent.plist` | real | `""` | false |
| symlink | `link.plist` | real | `"symlink"` | false |
| dangling symlink | `dangling.plist` | real | `"symlink"` | false |
| 0444 file | `readonly.plist` | real | `"read-only"` | false |
| writable file | `writable.plist` | real | `""` | false |
| root-owned 0644 | `writable.plist` | `return false` | `"read-only"` | false |
| root run on 0444 | `readonly.plist` | `return true` | `"read-only"` (the mode bits decide) | false |
| directory | `dir.plist` | real | `""` | true |

- [ ] **Step 2: Run it to verify it fails.** GOTEST `-run '^(TestExternalServiceError_Error|TestServiceFileExternal)$' ./internal/session/`. Expected: `FAIL github.com/asheshgoplani/agent-deck/internal/session [build failed]` with `undefined: ExternalServiceError`, `undefined: serviceFileWritable`, `undefined: serviceFileExternal`.
- [ ] **Step 3: Implement `ExternalServiceError`, `serviceFileWritable` and `serviceFileExternal` in service_files.go.** `os.Lstat`, then: not-exist → `(nil, nil)`; other failure → `"checking %s at %s: %w"` (label, path, err); symlink → `"symlink"`; regular file with `Mode().Perm()&0o222 == 0` (tested first, so a root run still sees 0444 as read-only) or `!serviceFileWritable(path)` → `"read-only"`; other regular file → `(nil, nil)`; anything else → `"refusing to manage %s: %s is not a regular file (%s)"` (label, path, `info.Mode().Type()`).
- [ ] **Step 4: Run it to verify it passes.** Step 2's command. Expected: `ok  github.com/asheshgoplani/agent-deck/internal/session`.
- [ ] **Step 5: Write the failing replace tests and their helpers.**
  - `testPlist(t *testing.T, old string, perm os.FileMode) string`: `filepath.Join(t.TempDir(), testAgentLabel+".plist")`, written by writeServiceFixture unless `old == ""`. `wantFileBytes`; `wantPerm(t *testing.T, path string, want os.FileMode)`; `wantNoTemps`: `filepath.Glob(filepath.Join(dir, ".*.tmp-*"))` is empty; `wantCalls`: `slices.Equal`, so no `want` asserts no calls. `wantExternal`: `errors.As(err, &ext)` and `*ext == ExternalServiceError{Label: label, Path: path, Reason: reason}`, else `t.Fatalf("err = %v, want *ExternalServiceError{%s %s %s}", err, label, path, reason)`.
  - `serviceEntry(path)`: the Lstat and Stat modes, Readlink target and ReadFile bytes in one string (zero values on error), so any write (also through a link), chmod, removal or replaced link changes it. `observeLaunchctl(t *testing.T, plist string) *[]string`: wraps the installed fake's `runLaunchctl` (restored in t.Cleanup); before delegating it appends `fmt.Sprintf("%s %s temps=%d", args[0], <plist bytes now, "" when missing>, <count of .*.tmp-* beside it>)`.
  - `externalServiceKinds`, one row per external kind; each `setup` first creates P's parent at 0o755: `0444` (`"managed by nix"` at 0o444) → `"read-only"`; `symlink` (to `<another t.TempDir()>/target` holding `"managed by nix"` at 0o644) → `"symlink"`; `dangling symlink` (to `<dir of P>/gone`) → `"symlink"`; `root-owned 0644` (`"managed by nix"` at 0o644, `serviceFileWritable` returning false, restored in t.Cleanup) → `"read-only"`.
  - Every test and subtest starts with `fake := installServiceManagerFake(t)`; the content written is always `"new"`.
  - `TestReplaceLaunchAgent_ExternalIsUntouched`, a subtest per kind at `p := filepath.Join(t.TempDir(), testAgentLabel+".plist")`: `wantExternal(t, replaceLaunchAgent(testAgentLabel, p, []byte("new")), testAgentLabel, p, kind.reason)`; `wantCalls(t, fake.Calls())`; `serviceEntry(p)` unchanged; `wantNoTemps(t, filepath.Dir(p))`.
  - `TestReplaceLaunchAgent_OwnedPlist`, subtests `{old: "old", perm: 0o640}` (neither CreateTemp's 0o600 nor 0o644, so a missing Chmod shows), `{old: "", perm: 0o644}` and `{old: "new", perm: 0o644}` (same bytes, still reloaded): `p := testPlist(t, tc.old, tc.perm)`; `seen := observeLaunchctl(t, p)`; replace returns nil; `wantCalls(t, fake.MutatingCalls(), "launchctl unload "+p, "launchctl load "+p)`; `wantCalls(t, *seen, "unload "+tc.old+" temps=1", "load new temps=0")` (temp written before the unload, renamed before the load); `wantFileBytes(t, p, "new")`; `wantPerm(t, p, tc.perm)`; `wantNoTemps`.
  - `TestReplaceLaunchAgent_TempWriteFailureUnloadsNothing`: `testutil.SkipIfRoot(t, "root can create files in a 0555 directory")`; `p := testPlist(t, "old", 0o644)`, then `os.Chmod(filepath.Dir(p), 0o555)` (0o755 again in t.Cleanup); the error contains `writing com.agentdeck.test-agent plist`; `wantCalls(t, fake.Calls())`; `wantFileBytes(t, p, "old")`.
  - `TestReplaceLaunchAgent_Failures`, old plists at 0o644. Rename "1st"/"2nd": `renameServiceFile` (restored in t.Cleanup) returns `errors.New("rename refused")` on that call, `os.Rename` otherwise. Load "first": `fake.FailNext("launchctl", "load", errors.New("load refused"))`; "every": `fake.FailOn` with the same arguments. Each row checks `wantCalls(t, fake.MutatingCalls(), ...)`, the plist after, that the error contains the text (P is the plist path), then `wantNoTemps`:

| row | old | rename | load | MutatingCalls (`launchctl <verb> P`) | plist after | error contains |
|---|---|---|---|---|---|---|
| rename refused | `"old"` | 1st | ok | unload, load | `"old"` | `replacing com.agentdeck.test-agent plist: rename refused; reloaded the previous plist` |
| rename refused, old will not load | `"old"` | 1st | every | unload, load | `"old"` | `the previous plist did not load: run 'launchctl load P'` |
| rename refused, first install | none | 1st | ok | unload | absent | `replacing com.agentdeck.test-agent plist: rename refused` |
| new plist rejected | `"old"` | ok | first | unload, load, load | `"old"` | `loading com.agentdeck.test-agent: load refused; restored and reloaded the previous plist` |
| old plist rejected too | `"old"` | ok | every | unload, load, load | `"old"` | `restored the previous plist, but it did not load: run 'launchctl load P'` |
| restore fails | `"old"` | 2nd | every | unload, load | `"new"` | `restoring the previous plist failed` |
| first install rejected | none | ok | every | unload, load | `"new"` | `plist written but failed to load com.agentdeck.test-agent` |

- [ ] **Step 6: Run them to verify they fail.** GOTEST `-run '^TestReplaceLaunchAgent_' ./internal/session/`. Expected: `[build failed]` with `undefined: replaceLaunchAgent` and `undefined: renameServiceFile`.
- [ ] **Step 7: Implement `renameServiceFile`, `writeServiceTemp` and `replaceLaunchAgent` in service_files.go.**
  - `writeServiceTemp`: `os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".tmp-*")` (never ending in `.plist`, `.service` or `.timer`; update's hygiene pass globs `*.plist`, internal/update/launchd.go:188), then `Chmod(perm)` on the open file, Write, Sync, Close; any failure removes the temp. Returns the temp's path.
  - `replaceLaunchAgent`, in order: (1) `serviceFileExternal`: return its error, and `ext` only inside `if ext != nil` (a nil `*ExternalServiceError` returned as an `error` is non-nil); (2) keep an existing plist's bytes and permission bits (a new plist gets 0o644, as `os.WriteFile` gives today); (3) `writeServiceTemp`; (4) `_ = runLaunchctl("unload", plistPath)`, even with no plist (the guard tests pin it); (5) `renameServiceFile(tmp, plistPath)`; on failure remove the temp and, when an old plist exists, load it again; (6) `runLaunchctl("load", plistPath)`; on failure with old bytes, restore them with `writeServiceTemp` (old mode) and `renameServiceFile`, removing that temp if the restore fails, and load again only once restored. Error copy, label first:
```go
"reading %s plist: %w"                                 // nothing unloaded
"writing %s plist: %w"                                 // temp write failed; nothing unloaded
"replacing %s plist: %w; reloaded the previous plist"
"replacing %s plist: %w; the previous plist did not load: run 'launchctl load %s'"
"replacing %s plist: %w"                               // first install: nothing to reload
"loading %s: %w; restored and reloaded the previous plist"
"loading %s: %w; restored the previous plist, but it did not load: run 'launchctl load %s'"
"loading %s: %w; restoring the previous plist failed: %v"
"plist written but failed to load %s: %w"              // first install: the new plist stays, as today
```
- [ ] **Step 8: Run them to verify they pass.** Step 6's command. Expected: `ok  github.com/asheshgoplani/agent-deck/internal/session`; as root, TempWriteFailure skips.
- [ ] **Step 9: Write the failing `TestRemoveLaunchAgent`**, a table. Each row starts with `fake := installServiceManagerFake(t)`, builds P under `t.TempDir()`, may call `fake.FailOn("launchctl", "unload", errors.New("unload refused"))`, then checks the error, `wantCalls(t, fake.Calls(), ...)` and the entry at P:

| row | fixture at P | strict | unload fails | error | Calls | P after |
|---|---|---|---|---|---|---|
| missing | none | true | no | nil | none | absent |
| `external/<kind>`, each of `externalServiceKinds` | `kind.setup(t, p)` | false | no | `wantExternal(t, err, testAgentLabel, p, kind.reason)` | none | `serviceEntry(p)` unchanged |
| owned | `"old"` 0644 | true | no | nil | `launchctl unload P` | removed |
| owned, lenient | `"old"` 0644 | false | yes | nil | `launchctl unload P` | removed |
| owned, strict | `"old"` 0644 | true | yes | contains `unloading com.agentdeck.test-agent` | `launchctl unload P` | kept |

- [ ] **Step 10: Run it to verify it fails.** GOTEST `-run '^TestRemoveLaunchAgent$' ./internal/session/`. Expected: `[build failed]` with `undefined: removeLaunchAgent`.
- [ ] **Step 11: Implement `removeLaunchAgent` in service_files.go**, in order: (1) `os.Lstat` not-exist → nil before anything else; (2) `serviceFileExternal`: return its error, and `ext` only inside `if ext != nil`; (3) `runLaunchctl("unload", plistPath)`; only with `strictUnload`, return its failure as `"unloading %s: %w"` and keep the plist; (4) `os.Remove`, its failure as `"removing %s plist: %w"`.
- [ ] **Step 12: Run it to verify it passes.** Step 10's command. Expected: `ok  github.com/asheshgoplani/agent-deck/internal/session`.
- [ ] **Step 13: Write the failing wiring tests.**
  - `TestLaunchAgentWiring_ExternalPlistIsLeftAlone`: a subtest `<install|uninstall> <agent>/<kind>` for each call below and each `externalServiceKinds` kind. It sets `t.Setenv("HOME", t.TempDir())` (as withGuardSeams does, so fixtures at the real plist paths never land in the TestMain HOME the package shares) and `t.Setenv("PATH", t.TempDir())` (no python3 or agent-deck: an external plist must not need generating), then `fake := installServiceManagerFake(t)`, resolves `p`, runs `kind.setup(t, p)`, takes `before := serviceEntry(p)`, makes the call and checks `wantExternal(t, err, label, p, kind.reason)`, `wantCalls(t, fake.Calls())` (not even managername or list), `serviceEntry(p) == before` and `wantNoTemps(t, filepath.Dir(p))`.

| agent | label | path | install call | uninstall call |
|---|---|---|---|---|
| bridge | `LaunchdPlistName` | `LaunchdPlistPath()` | `installBridgeDaemonLaunchd()` | `uninstallBridgeDaemonLaunchd()` |
| notifier | `TransitionNotifierLaunchdPlistName` | `TransitionNotifierLaunchdPlistPath()` | `installTransitionNotifierLaunchd()` | `uninstallTransitionNotifierLaunchd()` |
| heartbeat | `HeartbeatPlistLabel("ops")` | `HeartbeatPlistPath("ops")` | `installHeartbeatDaemonLaunchd("ops", 15)` | `uninstallHeartbeatDaemonLaunchd("ops")` |

  - `TestInstallTransitionNotifierLaunchd_ExternalPrecedesBackgroundGuard`: `withGuardSeams(t, "Background", true)` (temp HOME; the guard alone would skip and return nil), then `fake := installServiceManagerFake(t)`; `writeServiceFixture(t, p, "managed by nix", 0o444)` at `p, _ := TransitionNotifierLaunchdPlistPath()`; `_, err := installTransitionNotifierLaunchd()`; `wantExternal(t, err, TransitionNotifierLaunchdPlistName, p, "read-only")`; `wantCalls(t, fake.Calls())`; `wantFileBytes(t, p, "managed by nix")`.
- [ ] **Step 14: Run them to verify they fail.** GOTEST `-run '^(TestLaunchAgentWiring_ExternalPlistIsLeftAlone|TestInstallTransitionNotifierLaunchd_ExternalPrecedesBackgroundGuard)$' ./internal/session/`. Expected: FAIL, every case stopping in wantExternal (`want *ExternalServiceError`). The uninstalls (they unload and delete, or take a dangling link for absent) and the guard test (the Background skip) report `err = <nil>`. The bridge and notifier installs report `err = <nil>` where they wrote through a symlink or into the stubbed file and `open <P>: permission denied` on 0444; the heartbeat install reports `failed to generate heartbeat plist: agent-deck not found in PATH`. A host with agent-deck at FindAgentDeck's fallbacks /opt/homebrew/bin or /usr/local/bin (conductor.go:1479-1482) gets the write's result there instead; one with no fallback python3 for findPython3 fails the bridge install with `failed to generate plist: python3 not found in PATH`.
- [ ] **Step 15: Rewire the six functions in conductor.go and delete `RemoveHeartbeatPlist`.**
  - Installs: resolve the plist path first; call `serviceFileExternal(label, path)` before `GenerateLaunchdPlist`, `GenerateTransitionNotifierLaunchdPlist` or `GenerateHeartbeatPlist` (they need python3 or agent-deck), returning its error and, only inside `if ext != nil`, `ext`; keep MkdirAll; replace the unload, WriteFile and load lines with `replaceLaunchAgent(label, path, []byte(content))`. Labels: `LaunchdPlistName`, `TransitionNotifierLaunchdPlistName`, `HeartbeatPlistLabel(name)`. The two `(string, error)` installers return `""` with the external error and every error raised before `replaceLaunchAgent`, and `(path, err)` with `replaceLaunchAgent`'s error, matching today's `plist written but failed to load` return and Task 7's systemd `(unitPath, err)`; callers read the path only on success (cmd/agent-deck/conductor_cmd.go:757-767, :773-781, :1955).
  - Notifier: the check sits between `TransitionNotifierLaunchdPlistPath()` and the `inBackgroundLaunchdDomain() && transitionNotifierIsRunning()` guard, which does not change. Heartbeat: return the MkdirAll error as `failed to create LaunchAgents dir: %w`, as bridge and notifier do, instead of discarding it with `_ =`.
  - Uninstalls: resolve the path, drop the `os.Stat` check and return `removeLaunchAgent(LaunchdPlistName, path, false)`, `removeLaunchAgent(TransitionNotifierLaunchdPlistName, path, false)` and `removeLaunchAgent(HeartbeatPlistLabel(name), path, true)` (strict: `TestLaunchdHeartbeatUninstallRefusesFailedUnload` pins a kept plist). With Lstat in place of Stat, a dangling-symlink plist (today: absent) and a live symlink (today: unloaded and unlinked) report external, a behaviour change for Tasks 9 and 10.
  - Delete `RemoveHeartbeatPlist`: it loses its only caller (afterwards `git grep -n RemoveHeartbeatPlist` finds nothing), and its Stat-then-Remove would delete an external plist.
- [ ] **Step 16: Run the task's tests to verify they pass.** GOTEST `-run '^(TestExternalServiceError_Error|TestServiceFileExternal|TestReplaceLaunchAgent_|TestRemoveLaunchAgent|TestLaunchAgentWiring_|TestInstallTransitionNotifierLaunchd_|TestLaunchdHeartbeat|TestUninstallHeartbeatDaemon_)' ./internal/session/`, then GOTEST `-run '^TestRuntimeLifecycle_ConductorTeardown' ./cmd/agent-deck/`. Expected: `ok` for both packages. The four guard tests pass unchanged (the two that proceed still record `unload` and `load` with no plist present); both `TestLaunchdHeartbeat*` pass with their PATH fakes; `TestUninstallHeartbeatDaemon_StopFailureKeepsEnabled` skips off Linux; the teardown tests pass because a missing heartbeat plist makes no launchctl call (the cmd TestMain's failing launchctl stub would turn one into exit 1).
- [ ] **Step 17: Commit.** GOFMT `internal/session/service_files.go internal/session/service_files_test.go internal/session/conductor.go` (no output); `git add` those three files; COMMIT with subject `fix(session): leave externally managed launch agents alone` and body "A plist that is a symlink or that this user cannot write belongs to another manager (home-manager writes 0444 files). Installing or removing the bridge, notifier or heartbeat agent now returns *ExternalServiceError for it, with no launchctl call and no write. An owned plist is written to a temp file before anything is unloaded and keeps its mode; a failed rename reloads the old plist, and a failed load restores and reloads the old bytes. RemoveHeartbeatPlist lost its only caller and is removed."

### Task 7: systemd units: ownership, safe replace and remove, and the platform probes

**Files:**
- Modify: `internal/session/service_files.go` (Task 6's file): the systemd helpers, the sentinel and both probes; add the `errors` and `internal/platform` imports if absent.
- Modify: `internal/session/conductor.go`, each function found by name (base 0953876e lines; Tasks 2, 3, 4 and 6 edit the file first): imports :3-19, `systemdUserAvailable` :2744-2761, `installBridgeDaemonSystemd` :2805-2840, `installTransitionNotifierSystemd` :2925-2959, `uninstallBridgeDaemonSystemd` :2986-3000, `uninstallTransitionNotifierSystemd` :3027-3041, `installHeartbeatDaemonSystemd` :3169-3208, `uninstallHeartbeatDaemonSystemd` :3290-3314.
- Modify: `internal/session/conductor_heartbeat_tick_test.go`: `TestUninstallHeartbeatDaemon_UnknownAgentDisablesAndPreservesMeta` (:353-391 at base, as Task 2 left it) and its imports.
- Create: `internal/session/service_files_systemd_test.go`.
- Test (rerun unchanged): `internal/session/conductor_systemd_logs_test.go`, `TestUninstallHeartbeatDaemon_StopFailureKeepsEnabled` (`internal/session/conductor_heartbeat_tick_test.go`, Linux only) and Task 6's tests.

**Interfaces:**
- Consumes:
  - Task 2: `var runSystemctl = func(args ...string) error`; `installServiceManagerFake(t *testing.T) *serviceManagerFake` with `Calls()`, `MutatingCalls()` and `FailOn(tool, verb string, err error)` (fails every matching call, still recorded).
  - Task 6: `type ExternalServiceError struct { Label, Path, Reason string }`, `func serviceFileExternal(label, path string) (*ExternalServiceError, error)`, `var serviceFileWritable = func(path string) bool`, `var renameServiceFile = os.Rename`, `func writeServiceTemp(target string, content []byte, perm os.FileMode) (string, error)`; test helpers `writeServiceFixture(t *testing.T, path, content string, perm os.FileMode)`, `wantFileBytes(t *testing.T, path, want string)`, `wantNoTemps(t *testing.T, dir string)`, `wantCalls(t *testing.T, got []string, want ...string)`, `wantExternal(t *testing.T, err error, label, path, reason string)`. Not reused, and no name redeclared: Task 6's local `serviceEntry` and `externalServiceKinds` (its root-owned kind stubs the seam for every path, so a pair's owned unit would turn external too).
  - Existing: `useTempConductorHome(t)` (conductor_test.go:870).
- Produces:
  - `type systemdUnitFile struct { Name, Path string; Content []byte }`
  - `func replaceSystemdUnits(label string, units []systemdUnitFile, enableUnit string) error`
  - `func removeSystemdUnits(label string, units []systemdUnitFile, disableUnit string, strictDisable bool) error`
  - `var systemdUserAvailableCheck = systemdUserAvailable` (conductor.go)
  - `func BridgeDaemonExternal() *ExternalServiceError`, `func HeartbeatDaemonExternal(name string) *ExternalServiceError`: nil when agent-deck owns the file, it is missing, or it cannot be checked.
  - On systemd, `InstallBridgeDaemon`, `InstallTransitionNotifierDaemon`, `InstallHeartbeatDaemon` and their three `Uninstall*` twins return the `*ExternalServiceError`, labelled with the unit each installer enables: `agent-deck-conductor-bridge.service`, `agent-deck-transition-notifier.service`, `agent-deck-conductor-heartbeat-<name>.timer` (Task 10 handles the heartbeat meta).
  - Local: `var errSystemdUserUnavailable = errors.New("systemd user session not available")`; `func systemdUnitsExternal(label string, units []systemdUnitFile) (*ExternalServiceError, error)`: the first unit, in slice order, whose `serviceFileExternal(label, u.Path)` errors or is external.

- [ ] **Step 1: Write the failing helper and probe tests** in `internal/session/service_files_systemd_test.go` (package session; its imports include errors, maps, os, path/filepath, slices, testing and internal/platform; no `t.Parallel`; no root skips, G4). Helpers:
  - `stubSystemdUserAvailable(t *testing.T, f func() bool)` swaps `systemdUserAvailableCheck`, restored in t.Cleanup. `hbUnits(dir string) []systemdUnitFile` returns `hb.service` then `hb.timer` in dir, Content `"new <Name>"`.
  - `snapshotUnitDir(t *testing.T, dir string) map[string]string`: every `os.ReadDir(dir)` entry (temps included) by name, with its Lstat fingerprint: `"<perm> <bytes>"` for a regular file, `"-> <target> <target's bytes, or missing>"` for a symlink, the file mode for anything else.
  - `externalUnitKinds []struct{ name, reason string; setup func(t *testing.T, p string) }`, each setup creating p's directory: `0444` (`writeServiceFixture(t, p, "kept", 0o444)`, `read-only`); `symlink` (to `<t.TempDir()>/store.unit` holding `"kept"` 0644, `symlink`); `dangling symlink` (to `<t.TempDir()>/gone.unit`, `symlink`); `unwritable via the seam` (`"kept"` 0644, `serviceFileWritable` wrapped to answer false for p only, deferring to the seam it replaced, restored in t.Cleanup, `read-only`).

  Each test, row and kind subtest starts with `fake := installServiceManagerFake(t)`; the hb tests use `units := hbUnits(dir)` with a fresh `dir := t.TempDir()`, whose units hold `"old <Name>"` at 0644 unless a row says otherwise; "stub fails" means `stubSystemdUserAvailable(t, func() bool { t.Error("availability checked"); return true })`.
  - `TestReplaceSystemdUnits_OneExternalUnitWritesNeither`, a subtest per kind set up at `hb.timer`, stub fails: `wantExternal(t, replaceSystemdUnits("hb.timer", units, "hb.timer"), "hb.timer", units[1].Path, kind.reason)`; `wantCalls(t, fake.Calls())`; `snapshotUnitDir(t, dir)` equal before and after.
  - `TestReplaceSystemdUnits_OwnedPairRenamedThenOnlyEnabled`, rows `old` and `current` (units already hold `"new <Name>"`): the stub records both files' bytes and returns true; err nil; recorded `["new hb.service", "new hb.timer"]`; each unit 0644 and `!os.SameFile` with its pre-call `os.Stat` (renamed, even when current); `wantNoTemps(t, dir)`; `wantCalls(t, fake.Calls(), "systemctl --user enable --now hb.timer")`. Row `unavailable` (stub returns false): `err == errSystemdUserUnavailable`, both units `"new <Name>"`, `wantCalls(t, fake.Calls())`.
  - `TestReplaceSystemdUnits_Failures`, stub fails, each row ending in `wantNoTemps(t, dir)` and `wantCalls(t, fake.Calls())`:
    - `temp write fails`: hb.service only, `units[1].Path` = `<dir>/missing/hb.timer`; err contains `writing hb.timer`; `wantFileBytes` hb.service `old hb.service`.
    - `rename fails`: `renameServiceFile` refuses its 2nd call with `rename refused` (restored in t.Cleanup); err contains `installing hb.timer: rename refused`; hb.service `new hb.service`, hb.timer `old hb.timer`.
  - `TestRemoveSystemdUnits_Sequence`: `removeSystemdUnits("hb.timer", units, "hb.timer", strict)`; a disable failure is `fake.FailOn("systemctl", "disable", errors.New("exit status 1"))`; D = `systemctl --user disable --now hb.timer`, R = `systemctl --user daemon-reload`; left = `slices.Sorted(maps.Keys(snapshotUnitDir(t, dir)))`.

| row | fixture | strict | disable fails | err | `fake.Calls()` | left |
|---|---|---|---|---|---|---|
| owned | both | true | no | nil | D, R | none |
| strict failure | both | true | yes | contains `disabling hb.timer` | D | hb.service, hb.timer |
| lenient failure | both | false | yes | nil | D, R | none |
| service only | hb.service | true | no | nil | R | none |
| missing | none | true | no | nil | none | none |
| external/<kind> | hb.service, `kind.setup` at hb.timer | false | no | `wantExternal(t, err, "hb.timer", units[1].Path, kind.reason)` | none | snapshot unchanged |

  - `TestDaemonExternalProbes_HostPlatform`, a subtest per kind with `useTempConductorHome(t)`; rows (label, path, probe) by `platform.Detect()`, any other platform `t.Skipf`:
    - macOS: (`com.agentdeck.conductor-bridge`, `LaunchdPlistPath()`, `BridgeDaemonExternal()`) and (`com.agentdeck.conductor-heartbeat.ops`, `HeartbeatPlistPath("ops")`, `HeartbeatDaemonExternal("ops")`).
    - Linux and WSL2: (`agent-deck-conductor-bridge.service`, `SystemdBridgeServicePath()`, `BridgeDaemonExternal()`), then `agent-deck-conductor-heartbeat-ops.timer` with `HeartbeatDaemonExternal("ops")` at `SystemdHeartbeatServicePath("ops")` and again at `SystemdHeartbeatTimerPath("ops")`.
    - Each row: the probe is nil while path is missing and after `writeServiceFixture(t, path, "owned", 0o644)`; after `os.Remove(path)` and `kind.setup(t, path)`, `ext != nil && *ext == ExternalServiceError{label, path, kind.reason}`; then `os.Remove(path)`. Last, `wantCalls(t, fake.Calls())`.

- [ ] **Step 2: Run them to verify they fail.** GOTEST `-run '^(TestReplaceSystemdUnits_|TestRemoveSystemdUnits_|TestDaemonExternalProbes_)' ./internal/session/`. Expected: `FAIL	github.com/asheshgoplani/agent-deck/internal/session [build failed]`, after `undefined:` errors for some of `systemdUnitFile`, `systemdUserAvailableCheck`, `replaceSystemdUnits`, `removeSystemdUnits`, `errSystemdUserUnavailable`, `BridgeDaemonExternal`, `HeartbeatDaemonExternal` (the compiler stops after ten).

- [ ] **Step 3: Implement `replaceSystemdUnits`** in `service_files.go`, with `systemdUnitFile`, `errSystemdUserUnavailable` (doc: the units are in place and nothing was enabled) and `systemdUnitsExternal`; add `systemdUserAvailableCheck` below `systemdUserAvailable` in conductor.go (doc: install paths call it, so tests decide availability without XDG_RUNTIME_DIR or loginctl; the two hints keep calling `systemdUserAvailable`). In order:
  1. `systemdUnitsExternal(label, units)`: return its error, and `ext` only inside `if ext != nil`.
  2. `writeServiceTemp(u.Path, u.Content, 0o644)` for every unit; a failure returns `"writing %s: %w"` (u.Name).
  3. `renameServiceFile(tmp, u.Path)` in slice order; a failure returns `"installing %s: %w"` (u.Name). A deferred cleanup removes every temp not renamed; nothing was stopped, so nothing is restored.
  4. `if !systemdUserAvailableCheck() { return errSystemdUserUnavailable }`, unwrapped.
  5. `runSystemctl("--user", "enable", "--now", enableUnit)`; a failure returns `"unit written but enable failed: %w"` (today's bridge and notifier text; the heartbeat's `failed to enable heartbeat timer` goes, and no test pins it). No stop, no daemon-reload.

- [ ] **Step 4: Implement `removeSystemdUnits`** in `service_files.go`, in order:
  1. The same external pass.
  2. No unit present by `os.Lstat`: return nil with no systemctl call. (Today the bridge and notifier always run `disable --now`, conductor.go:2987 and :3028, and the heartbeat always runs daemon-reload, :3312.)
  3. Only when the unit named `disableUnit` is present, `runSystemctl("--user", "disable", "--now", disableUnit)`. With `strictDisable` a failure returns `"disabling %s: %w"` and keeps every file (today's heartbeat); without it the failure is ignored (today's bridge and notifier).
  4. `os.Remove` each present unit in slice order; a failure returns `"removing %s: %w"` (u.Name).
  5. `_ = runSystemctl("--user", "daemon-reload")`.

- [ ] **Step 5: Implement the probes** in `service_files.go`, switching on `platform.Detect()` as `InstallBridgeDaemon` does. macOS: `serviceFileExternal(LaunchdPlistName, LaunchdPlistPath())` and `serviceFileExternal(HeartbeatPlistLabel(name), HeartbeatPlistPath(name))`. Linux and WSL2: `serviceFileExternal(systemdBridgeServiceName, SystemdBridgeServicePath())`; the heartbeat checks `SystemdHeartbeatServicePath(name)` (it hard-codes heartbeat.sh's path, which migrate-dir moves), then `SystemdHeartbeatTimerPath(name)`, both labelled `SystemdHeartbeatTimerName(name)`, and the first external one wins. A path error or a `serviceFileExternal` error counts as not external for that file (the install that follows reports it); any other platform gives nil.

- [ ] **Step 6: Run them to verify they pass.** Step 2's GOTEST. Expected: `ok  	github.com/asheshgoplani/agent-deck/internal/session`; on darwin the probe test takes the macOS rows.

- [ ] **Step 7: Write the failing wiring tests** in the same file, labels written as the literals in Produces:
  - `TestSystemdDaemons_UnavailableKeepsTodaysErrorWithUnitsInPlace`: `useTempConductorHome(t)`; fake; stub returns false; `writeServiceFixture(t, filepath.Join(ConductorVenvDir(), "bin", "python3"), "#!/bin/sh\nexit 0\n", 0o755)`. Each install returns `""` (heartbeat: only the error) and an err with `errors.Is(err, errSystemdUserUnavailable)` and exactly this text (condDir = `ConductorDir()`, opsDir = `ConductorNameDir("ops")`):
    - bridge: `systemd user session not available (common in containers/VMs without lingering); run manually: python3 <condDir>/bridge.py`
    - notifier: `systemd user session not available; run manually: agent-deck notify-daemon`
    - `installHeartbeatDaemonSystemd("ops", 15)`: `systemd user session not available; run heartbeat manually via cron or: bash <opsDir>/heartbeat.sh`
    - Then `slices.Sorted(maps.Keys(snapshotUnitDir(t, <SystemdUserDir()>)))` is exactly `agent-deck-conductor-bridge.service`, `agent-deck-conductor-heartbeat-ops.service`, `agent-deck-conductor-heartbeat-ops.timer`, `agent-deck-transition-notifier.service` (no temp), and `wantCalls(t, fake.Calls())`.
  - `TestSystemdDaemons_ExternalUnitsLeftAlone`, a subtest per kind: `useTempConductorHome(t)`; `t.Setenv("PATH", t.TempDir())` and no venv python3 (an external unit needs no generating); fake; stub fails. `kind.setup` runs at the bridge unit, the notifier unit and the heartbeat timer; the heartbeat service is `"kept"` 0644 (one external unit keeps the pair).
    - The three `install*Systemd`, then the three `uninstall*Systemd` (each in the order bridge, notifier, heartbeat; name `ops`, interval 15) each give `wantExternal(t, err, label, <its external file; the heartbeat's is the timer>, kind.reason)`.
    - Then `wantCalls(t, fake.Calls())`; `snapshotUnitDir` of the unit dir equals its pre-call value; `ConductorDir()` and `filepath.Dir(logDataPath("transition-notifier.log"))` do not exist (no generate, no MkdirAll).
  - In `TestUninstallHeartbeatDaemon_UnknownAgentDisablesAndPreservesMeta`, replace Task 2's block (from `var wantCalls []string` through the `if` holding its `t.Fatalf`) with `wantCalls(t, fake.MutatingCalls())` (no unit file, no plist: no mutating call on any platform), and delete the `slices` and `internal/platform` imports Task 2 added for it (the file used neither at base).

- [ ] **Step 8: Run them to verify they fail.** GOTEST `-run '^(TestSystemdDaemons_|TestUninstallHeartbeatDaemon_)' ./internal/session/`. Expected `FAIL`:
  - `UnavailableKeepsTodaysErrorWithUnitsInPlace` at `bridge`: today's installers never consult the stub or wrap the sentinel.
  - Every `ExternalUnitsLeftAlone` subtest at `install bridge`, with `want *ExternalServiceError{agent-deck-conductor-bridge.service …}`. On hera 0444 gets `failed to write systemd unit: … permission denied`; the other kinds are written (through any link) and get today's `systemd user session not available …`.
  - On Linux, `UnknownAgentDisablesAndPreservesMeta` reports `systemctl --user daemon-reload`; on darwin it passes.

- [ ] **Step 9: Rewire `conductor.go`.**
  - Each installer first resolves its unit path(s) (today's path errors), builds its `[]systemdUnitFile` (Name, Path) and calls `systemdUnitsExternal` before any `Generate*` or `MkdirAll`, returning the error, or `ext` inside `if ext != nil` (with `""` for the bridge and notifier). It then generates, keeps today's `MkdirAll`s (unit dir; bridge and notifier log dirs at 0o700), fills Content and calls `replaceSystemdUnits`:
    - bridge (n = `systemdBridgeServiceName`, `replaceSystemdUnits(n, units, n)`): if `errors.Is(err, errSystemdUserUnavailable)`, `return "", fmt.Errorf("%w (common in containers/VMs without lingering); run manually: python3 %s/bridge.py", err, logDir)`. Notifier (n = `systemdTransitionNotifierServiceName`): `return "", fmt.Errorf("%w; run manually: agent-deck notify-daemon", err)`.
    - Both: any other error returns `(unitPath, err)`, success `(unitPath, nil)`; no caller reads the path on error (conductor_cmd.go:757, :773, :1955).
    - heartbeat (`timer := SystemdHeartbeatTimerName(name)`; units `{SystemdHeartbeatServiceName(name), svcPath}` then `{timer, timerPath}`; `replaceSystemdUnits(timer, units, timer)`): unavailable returns `fmt.Errorf("%w; run heartbeat manually via cron or: bash %s/heartbeat.sh", err, condDir)` with `condDir, _ := ConductorNameDir(name)`; any other error unchanged.
  - Each uninstall resolves its path(s) (today's errors) and then only returns `removeSystemdUnits(n, []systemdUnitFile{{Name: n, Path: unitPath}}, n, false)` (bridge, notifier) or `removeSystemdUnits(timer, <the heartbeat's two units, as above>, timer, true)` (heartbeat). `UninstallHeartbeatDaemon` is unchanged; add `"errors"` to the imports if no earlier task has.

- [ ] **Step 10: Run the new and existing tests to verify they pass.** GOTEST `-run '^(TestReplaceSystemdUnits_|TestRemoveSystemdUnits_|TestDaemonExternalProbes_|TestSystemdDaemons_|TestUninstallHeartbeatDaemon_|TestSystemdInstall|TestExternalServiceError_|TestServiceFileExternal|TestReplaceLaunchAgent_|TestRemoveLaunchAgent|TestLaunchAgentWiring_|TestInstallTransitionNotifierLaunchd_|TestLaunchdHeartbeat)' ./internal/session/`, then Task 6's GOTEST `-run '^TestRuntimeLifecycle_ConductorTeardown' ./cmd/agent-deck/`. Expected: `ok` for both packages. `TestSystemdInstallPreservesActivationError` still gets a path and `enable failed`. On darwin `_StopFailureKeepsEnabled` skips (the `strict failure` row covers its rule on any host) and the probe test's systemd rows do not run; Task 14's Linux container runs both.

- [ ] **Step 11: Commit.** GOFMT `internal/session/service_files.go internal/session/conductor.go internal/session/service_files_systemd_test.go internal/session/conductor_heartbeat_tick_test.go`; `git add` the same four files; COMMIT with subject `fix(session): leave externally managed systemd units alone` and body: "A unit file that is a symlink or that this user cannot write belongs to another manager, and the heartbeat service and timer count as one pair. Installing or removing the bridge, notifier or heartbeat units then returns *ExternalServiceError, with no systemctl call and nothing generated, written or removed. Owned units are renamed into place from temp files before the availability check and the unchanged 'systemctl --user enable --now', adding no stop. Removal makes no call when no unit file exists and disables only when the enabled unit's file is present; a failed disable keeps the heartbeat's files and is ignored for the bridge and notifier, as before. BridgeDaemonExternal and HeartbeatDaemonExternal let setup and migrate-dir ask first. TestUninstallHeartbeatDaemon_UnknownAgentDisablesAndPreservesMeta now expects no service-manager call on any platform."

### Task 8: heartbeat.sh: keep a custom script

Corrects design §2, which lists the director's custom `heartbeat.sh` as already safe. Only `MigrateConductorHeartbeatScripts` (run by `runAutoMigration`, `cmd/agent-deck/conductor_cmd.go:135`) skips a script that lacks either managed marker. `InstallHeartbeatScript` overwrites unconditionally, on every `conductor setup <name>` with heartbeat on (step 6, `conductor_cmd.go:718`) and for every conductor in `conductor migrate-dir --apply` (`internal/session/conductor_migrate_dir.go:367`). Hera's `director/heartbeat.sh` has neither marker. Needs no task; Tasks 9 and 11 need it (G5). Setup's call stays as is; Task 9 changes it (until then setup prints no skip line for a kept script and goes on to install the daemon, as today). Siblings: nothing else writes heartbeat.sh (not `conductor/setup.sh`, `teardown.sh` or the bridge), and no user doc changes: the `[conductor].dir` comment (`conductor.go:131-140`) and `config-reference.md:695` describe managed scripts and stay true.

**Files:** (lines at 6333ffde; Tasks 2-7 edit conductor.go, and Task 2 conductor_test.go, first, so find each site by its symbol)
- Modify: `internal/session/conductor.go:1322-1341` (`InstallHeartbeatScript` and its doc comment) and `:2207-2211` (the `managedScript` local in `MigrateConductorHeartbeatScripts`)
- Modify: `internal/session/conductor_migrate_dir.go:230-231` (step 6 of `MigrateConductorDir`'s doc comment; the call at `:367` stays unchanged)
- Modify: `internal/session/conductor_migrate_dir_test.go:57` (the `heartbeat.sh` fixture of `TestMigrateConductorDir_MovesHomesAndPreservesUserState`)
- Test: `internal/session/conductor_test.go` (after `TestRenderConductorHeartbeatScript_ReplacesHeartbeatPrefix`, which ends at `:700`) and `internal/session/conductor_migrate_dir_test.go` (after `TestMigrateConductorDir_MovesHomesAndPreservesUserState`, which ends at `:112`). Both files already import everything the tests use. No test here reaches launchctl or systemctl (`MigrateConductorDir` does no daemon work, `conductor_migrate_dir.go:233`), so none calls `installServiceManagerFake` (G3), swaps a seam, calls `t.Parallel` or needs a root skip (G4).

**Interfaces:**
- Consumes (existing code): `renderConductorHeartbeatScript(name, profile string) string` (`conductor.go:1343`), `ConductorNameDir(name string) (string, error)` (`:706`), `MigrateConductorHeartbeatScripts() ([]string, error)` (`:2180`), `MigrateConductorDir(opts ConductorDirMigrateOptions) (*ConductorDirMigrateResult, error)` (`conductor_migrate_dir.go:239`); test helpers `setupSessionXDGPathEnv(t) (home, xdgConfigHome, xdgDataHome string)` (`xdg_paths_test.go:11`) and `writeConductorHome(t, base, name string, files map[string]string) string` (`conductor_migrate_dir_test.go:13`).
- Produces:
  - `func isManagedHeartbeatScript(content string) bool`
  - `func InstallHeartbeatScriptOutcome(name, profile string) (kept bool, err error)`. Task 9 calls it at setup step 6 and prints `  [skip] heartbeat.sh has local changes; left unchanged` when kept.
  - `func InstallHeartbeatScript(name, profile string) error`, now its wrapper (kept → nil). Task 11 gets the keep rule through `MigrateConductorDir`'s unchanged call at `conductor_migrate_dir.go:367`, which this task tests; Task 11 must not re-implement it.

- [ ] **Step 1: Write the fixtures and the migration guard test** in `conductor_test.go`, after `TestRenderConductorHeartbeatScript_ReplacesHeartbeatPrefix`. The fixtures are test-only helpers, not contract names:

```go
// customHeartbeatScript has neither managed marker, so it is the user's own script.
const customHeartbeatScript = "#!/bin/bash\n# director's own heartbeat\nexec /usr/local/bin/director-heartbeat\n"

// olderHeartbeatScript is a managed heartbeat.sh from an earlier template
// generation, rendered against an old conductor root.
func olderHeartbeatScript(name string) string {
	return "#!/bin/bash\n# Heartbeat for conductor: " + name + " (profile: default)\n" +
		"SESSION=\"conductor-" + name + "\"\nCONDUCTOR_ROOT=\"/old/path/conductor\"\n"
}
```

No test calls `MigrateConductorHeartbeatScripts` today (`git grep`: only `conductor_cmd.go:135`), so `TestMigrateConductorHeartbeatScripts_RefreshesOnlyManaged` pins its behaviour across the extraction; it passes before and after by design. After `_, _, xdgDataHome := setupSessionXDGPathEnv(t)`, per row it calls `writeConductorHome(t, filepath.Join(xdgDataHome, "agent-deck", "conductor"), name, files)` with `meta.json` = `{"name":"<name>","profile":"default"}` plus the row's `heartbeat.sh`, then runs `MigrateConductorHeartbeatScripts()` and asserts a nil error, `slices.Equal(migrated, []string{"missing", "stale"})` (ListConductors reads directories sorted) and every script's exact bytes (`t.Fatalf("%s/heartbeat.sh = %q (err %v), want %q", ...)`):

| conductor | heartbeat.sh before | after |
|---|---|---|
| current | `renderConductorHeartbeatScript("current", "default")` | unchanged |
| custom | `customHeartbeatScript` | unchanged |
| missing | none | `renderConductorHeartbeatScript("missing", "default")` |
| stale | `olderHeartbeatScript("stale")` | `renderConductorHeartbeatScript("stale", "default")` |

- [ ] **Step 2: Convert the `:57` fixture and write the failing migrate-dir test** in `conductor_migrate_dir_test.go`. Line 57 becomes `"heartbeat.sh": olderHeartbeatScript("alpha"),`: today's `"OLD_ROOT=/old/path/conductor"` has no markers and would now be kept, but that test asserts a re-render at `:97`. After that test, add `TestMigrateConductorDir_KeepsCustomHeartbeatScript`: after `_, _, xdgDataHome := setupSessionXDGPathEnv(t)`, `writeConductorHome` gives home `alpha` under `filepath.Join(xdgDataHome, "agent-deck", "conductor")` with `meta.json` = `{"name":"alpha","profile":"default"}` and `heartbeat.sh` = `customHeartbeatScript`; `MigrateConductorDir(ConductorDirMigrateOptions{Target: filepath.Join(t.TempDir(), "vault-conductors"), Apply: true})` returns nil; `os.ReadFile` of `<target>/alpha/heartbeat.sh` succeeds and equals `customHeartbeatScript`, else `t.Fatalf("migrate-dir rewrote a custom heartbeat.sh:\n%s", got)`.

- [ ] **Step 3: Run the three tests; expect only the migrate-dir test to fail**
GOTEST `-run '^(TestMigrateConductorDir_KeepsCustomHeartbeatScript|TestMigrateConductorDir_MovesHomesAndPreservesUserState|TestMigrateConductorHeartbeatScripts_RefreshesOnlyManaged)$' ./internal/session/`. Expected: one `--- FAIL:` line, for `TestMigrateConductorDir_KeepsCustomHeartbeatScript`, printing `migrate-dir rewrote a custom heartbeat.sh:` and the rendered template (`# Heartbeat for conductor: alpha (profile: default)`); the other two pass (no `--- FAIL` line).

- [ ] **Step 4: Write the failing unit tests** in `conductor_test.go`, after the guard test. `TestIsManagedHeartbeatScript`, one `t.Errorf` per row:

| row | content | want |
|---|---|---|
| current render | `renderConductorHeartbeatScript("alpha", "work")` | true |
| older generation | `olderHeartbeatScript("alpha")` | true |
| custom | `customHeartbeatScript` | false |
| header marker only | `"# Heartbeat for conductor: alpha\nexec my-heartbeat\n"` | false |
| session marker only | `"SESSION=\"conductor-alpha\"\nexec my-heartbeat\n"` | false |
| empty | `""` | false |

`TestInstallHeartbeatScriptOutcome`, one `t.Run` per row: `setupSessionXDGPathEnv(t)`; `os.MkdirAll` of `ConductorNameDir("alpha")` (0o755); write `existing` there as `heartbeat.sh` (0o755) unless it is ""; `kept, err := InstallHeartbeatScriptOutcome("alpha", "work")`; want a nil error, `kept == wantKept`, and the file equal to `existing` when kept, else to `renderConductorHeartbeatScript("alpha", "work")`:

| row | existing | wantKept |
|---|---|---|
| missing is written | "" (no file) | false |
| older managed script is rewritten | `olderHeartbeatScript("alpha")` | false |
| custom script is kept | `customHeartbeatScript` | true |

- [ ] **Step 5: Run them; expect a build failure**
GOTEST `-run '^(TestIsManagedHeartbeatScript|TestInstallHeartbeatScriptOutcome)$' ./internal/session/`. Expected: `undefined: isManagedHeartbeatScript` and `undefined: InstallHeartbeatScriptOutcome`, then `FAIL	github.com/asheshgoplani/agent-deck/internal/session [build failed]`.

- [ ] **Step 6: Extract `func isManagedHeartbeatScript(content string) bool`** beside `InstallHeartbeatScript` in `conductor.go`: the two `strings.Contains` checks from `:2207-2208`, unchanged (both `# Heartbeat for conductor:` and `SESSION="conductor-` present). Doc: a heartbeat.sh carrying both markers of `conductorHeartbeatScript` is agent-deck's. In `MigrateConductorHeartbeatScripts`, replace `:2207-2211` with `if !isManagedHeartbeatScript(existingStr) { continue }`; keep `existingStr`, which the `TrimSpace` comparison at `:2213` still uses.

- [ ] **Step 7: Implement `func InstallHeartbeatScriptOutcome(name, profile string) (kept bool, err error)`** where `InstallHeartbeatScript` is now, followed by the wrapper `func InstallHeartbeatScript(name, profile string) error` (kept → nil; doc: the form for callers that do not report a kept script). The outcome's doc is the comment at `:1322-1333` with its subject renamed, plus: a script without the managed markers belongs to the user, so it is left untouched and reported as kept. It `os.ReadFile`s the script, then:
  - read succeeds and `!isManagedHeartbeatScript` → `(true, nil)`, nothing written;
  - any error but `os.IsNotExist` → `(false, fmt.Errorf("reading existing heartbeat.sh for %q: %w", name, err))`, nothing written (an unreadable script cannot be classified);
  - missing or managed → today's `os.WriteFile(scriptPath, []byte(renderConductorHeartbeatScript(name, profile)), 0o755)`, its error returned unwrapped. It stays `os.WriteFile` (the contract's "as today"; Task 8 does not depend on Task 3's writer). Read and write both follow symlinks, so a symlinked script is classified by its target and a managed one is written through, as today; an edited copy that keeps both markers counts as managed and is rewritten.

- [ ] **Step 8: Update step 6 of `MigrateConductorDir`'s doc comment** (`conductor_migrate_dir.go:230-231`) to:
```go
//  6. Reconcile path-baked artifacts: re-render each conductor's managed
//     heartbeat.sh (a custom one is kept) and reinstall base bridge.py.
```

- [ ] **Step 9: Run the affected tests; expect PASS**
GOTEST `-run '^(TestIsManagedHeartbeatScript|TestInstallHeartbeatScriptOutcome|TestMigrateConductorHeartbeatScripts_|TestMigrateConductorDir_|TestRenderConductorHeartbeatScript_|TestConductorHeartbeatScript_)' ./internal/session/`. Expected: `ok  	github.com/asheshgoplani/agent-deck/internal/session`, covering all 18 `TestMigrateConductorDir_` tests (the 17 existing ones and the new one). `KeepsCustomHeartbeatScript` passing also proves the wrapper returns nil for a kept script; otherwise `MigrateConductorDir` would fail with `re-render heartbeat.sh for "alpha"`. No `cmd/agent-deck` test plants its own `heartbeat.sh`, and setup still writes a missing or managed one, so no other package needs a run.

- [ ] **Step 10: Commit**
GOFMT `internal/session/conductor.go internal/session/conductor_migrate_dir.go internal/session/conductor_test.go internal/session/conductor_migrate_dir_test.go`; `git add` those four files; COMMIT with this subject and body (then the two attribution lines):
```
fix(session): keep a custom heartbeat.sh on setup reruns and migrate-dir

InstallHeartbeatScript rewrote heartbeat.sh unconditionally, so rerunning
'conductor setup <name>' with heartbeat on, and every 'conductor migrate-dir
--apply', replaced a user's own script. Only MigrateConductorHeartbeatScripts
checked for the managed markers.

isManagedHeartbeatScript now holds that check for both writers.
InstallHeartbeatScriptOutcome writes a missing or managed script, leaves a
script without the markers untouched and reports it as kept, and does not
overwrite one it cannot read. InstallHeartbeatScript wraps it.
```
Expected: GOFMT prints nothing, the lefthook pre-commit `go vet ./...` passes, and the commit is created.

### Task 9: `conductor setup` wiring

**Files:**
- Modify: `cmd/agent-deck/conductor_cmd.go:710-804` (`handleConductorSetup`: step 6 heartbeat :710-726, step 7 bridge :728-769, step 8 notifier :771-781, the `--json` map :783-804; 6333ffde numbers, which Tasks 1-8 leave as they are)
- Create: `cmd/agent-deck/conductor_service_fixtures_test.go` (CLI service fixtures; Tasks 10 and 11 reuse them)
- Create: `cmd/agent-deck/conductor_setup_external_test.go`
- Test: `cmd/agent-deck/conductor_setup_external_test.go`
- Modify (docs): `skills/agent-deck/references/cli-reference.md:1115-1118`, `skills/agent-deck/references/config-reference.md:695,697`, `README.md:699`, `docs/conductor/telegram.md:91`, `docs/conductor/README.md:208`, `docs/conductor/mattermost.md:39`

**Interfaces:** (needs Tasks 4, 6, 7, 8)
- Consumes:
  - Task 4: `func InstallBridgeScriptOutcome() (BridgeScriptResult, error)`; `type BridgeScriptResult struct { Outcome BridgeScriptOutcome; Notice string }` (Notice only for kept); `type BridgeScriptOutcome string` with `BridgeScriptInstalled`, `BridgeScriptUpdated`, `BridgeScriptUnchanged`, `BridgeScriptKept` = `"installed"`, `"updated"`, `"unchanged"`, `"kept"`; `BridgeKeptNotice`, `BridgeSymlinkNotice`; stamp `<conductor dir>/.bridge.py.sha256` (64 lower-case hex digits, then `"\n"`), never written for kept; a replaced bridge becomes `.bridge.py.previous`.
  - Task 6: `type ExternalServiceError struct { Label, Path, Reason string }`, `(*ExternalServiceError).Error()` = `<Label>: managed externally (<Reason>)`, Reason `"symlink"` or `"read-only"` (`mode&0o222 == 0` even for root). `InstallBridgeDaemon`, `InstallTransitionNotifierDaemon` (external check before its Background-domain guard) and `InstallHeartbeatDaemon` return it as their error.
  - Task 7: `func BridgeDaemonExternal() *ExternalServiceError`. A systemd label is the unit its installer enables (bridge, notifier: the `.service`; heartbeat: the `.timer`); every external check precedes Generate* and MkdirAll.
  - Task 8: `func InstallHeartbeatScriptOutcome(name, profile string) (kept bool, err error)`.
- Produces:
  - In `conductor_service_fixtures_test.go` (package main): `var serviceQueryVerbs map[string][]string`; `func writeReadOnlyServiceFixtures(t *testing.T, home string, mode os.FileMode, labels ...string) []string`; `func installRecordingServiceFakes(t *testing.T, listLoaded bool) (pathDir, logPath string)`; `func serviceCallLines(t *testing.T, logPath string) []string`; `func mutatingServiceCalls(t *testing.T, logPath string) []string`.
  - The contract's pinned setup lines, and the `--json` keys `"bridge_script"` (present when step 7 ran) and `"external"` (always present; `[]` when empty, never null).

- [ ] **Step 1: Write the shared fixtures** in `cmd/agent-deck/conductor_service_fixtures_test.go`. No root skip (G4).
  - `serviceQueryVerbs` is Task 2's allow-list: `"launchctl"`: list, print, print-disabled, managername; `"systemctl"`: is-active, is-enabled, is-failed, is-system-running, show, cat, status, list-units, list-unit-files, list-timers.
  - `writeReadOnlyServiceFixtures`: a label ending in `.service` or `.timer` maps to `<home>/.config/systemd/user/<label>`, any other label to `<home>/Library/LaunchAgents/<label>.plist`. For each: MkdirAll the parent 0o755, write `"fixture\n"`, Chmod to mode. Returns the paths in label order. Callers pass both platforms' labels; the binary reads its host's.
  - `installRecordingServiceFakes`: 0o755 `/bin/sh` scripts `launchctl` and `systemctl` in pathDir = `t.TempDir()`; logPath = `filepath.Join(t.TempDir(), "service.log")`. Each appends `<tool> $*` as one line to logPath. When its verb (the first argument not starting with `-`) is `list` or `is-active`, it prints `$*` and exits 0 if listLoaded, else exits 1; any other verb exits 0.
  - Callers pass `"PATH=" + pathDir + string(os.PathListSeparator) + os.Getenv("PATH")` as extraEnv. `agentDeckTestEnv` appends extraEnv last (channels_cmd_test.go:244) and os/exec keeps the last duplicate key, so the fakes shadow TestMain's failing stubs (as issue2806_remove_stopped_test.go:141 does). Real launchctl and systemctl stay unreachable (G3).
  - `serviceCallLines` returns the log's non-empty lines (no log: nil). `mutatingServiceCalls` keeps the lines whose verb is not in `serviceQueryVerbs[tool]` (fail closed).

- [ ] **Step 2: Write the setup fixture** in `cmd/agent-deck/conductor_setup_external_test.go`: package main; imports crypto/sha256, encoding/json, fmt, os, path/filepath, runtime, slices, strings, testing, internal/session; no t.Parallel.
  - `setupCLIDiscordConfig = "[conductor.discord]\nbot_token = \"dummy\"\n"` (a non-empty bot_token is all `discordConfigured` checks, conductor_cmd.go:366). `setupCLIBridgeUnit = "agent-deck-conductor-bridge.service"`, `setupCLINotifierUnit = "agent-deck-transition-notifier.service"` (session's unexported names, conductor.go:2559-2560). `setupCLIServiceLabels() (bridge, notifier string)`: `session.LaunchdPlistName`, `session.TransitionNotifierLaunchdPlistName` on darwin, else the two units.
  - `type setupCLIFixture struct { home, condDir, pythonLog, serviceLog string; env []string; want map[string]string }`. `newSetupCLIFixture(t *testing.T, config string) *setupCLIFixture`:
    - `t.Skip` unless darwin or linux; `home := t.TempDir()`; `writeAddUserConfig(t, home, config)` (add_test.go:790); `condDir = home/.local/share/agent-deck/conductor` (ConductorDir under agentDeckTestEnv's XDG_DATA_HOME).
    - `<condDir>/venv/bin/python3`, a 0o755 `/bin/sh` script, runs `printf '%s\n' "$*" >> '<pythonLog>'`, then `exit 1`; pythonLog is in another `t.TempDir()`. findPython3 prefers it, so no host pip runs.
    - `pathDir, serviceLog` come from `installRecordingServiceFakes(t, false)`; `env = []string{"PATH=" + pathDir + string(os.PathListSeparator) + os.Getenv("PATH")}`; `want = map[string]string{}`.
  - Methods: `keep(t, paths ...string)` records each file's current bytes in want; `writeUserFile(t, path, content string)` writes 0o755 and keeps it; `assertUnchanged(t)` re-reads every kept file and calls `t.Errorf` on a difference or read error; `run(t, args ...string) (stdout, stderr string)` calls `runAgentDeckEnv(t, fx.home, "", fx.env, args...)`, and on a non-zero exit `t.Fatalf("agent-deck %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, stdout, stderr)`.

- [ ] **Step 3: Write the three failing tests.** "Has the line" means `slices.Contains(strings.Split(stdout, "\n"), line)`, failing as `stdout lacks the line %q`. In `--json` runs, stdout unmarshals into one `map[string]json.RawMessage` (else `t.Fatalf`).
  - `TestConductorSetupCLI_ExternalBridgeAndAgentsUntouched` (hera's case), subtests `text` and `json`, each with its own Discord fixture; `fx.writeUserFile(t, <condDir>/bridge.py, "# bridge.py with the director's Discord Q&A hook\n")`; `fx.keep` the paths of `writeReadOnlyServiceFixtures(t, fx.home, 0o444, session.LaunchdPlistName, session.TransitionNotifierLaunchdPlistName, setupCLIBridgeUnit, setupCLINotifierUnit)`; `fx.run(t, "conductor", "setup", "steward-probe", "--no-heartbeat")`, plus `"--json"` in `json`. Both: `assertUnchanged`; pythonLog empty (`setup ran the venv python (pip) for an external bridge agent`); stderr contains neither `Warning: failed` nor `Tip: `.
    - `text`: `serviceCallLines` is empty (no call at all: every external check, the notifier's included, comes before any launchctl or systemctl call). Stdout has the lines `[skip] bridge daemon managed externally; Python deps left to its manager`, `session.BridgeKeptNotice`, `"[skip] " + bridgeLabel + ": managed externally (read-only)"` and the same for notifierLabel, and contains none of `[ok] bridge.py`, `[ok] Bridge daemon loaded`, `Bridge daemon NOT installed`, `[ok] Transition notifier daemon installed`.
    - `json`: `mutatingServiceCalls` is empty (only the `notifier_daemon_running` query runs). `bridge_script` is `"kept"`; `external` unmarshals to `[bridgeLabel, notifierLabel]` (`slices.Equal`); there is no `daemon` and no `notifier_daemon` key.
  - `TestConductorSetupCLI_FreshBridgeInstalledAndStamped`: one Discord fixture; the rows run in order, each as subtests `text` then `json` of `conductor setup fresh --no-heartbeat` (`json` adds `--json`), with the prep before each subtest. Text: stdout has the row's line. JSON: `t.Errorf("bridge_script = %s, want %q", ...)` on a mismatch, and raw `external` must be `[]` (`external = %q, want [] (never null)`). "Stamped": the stamp equals `fmt.Sprintf("%x\n", sha256.Sum256(<bridge.py bytes>))`.

    | row | prep | text line | `bridge_script` | after each run |
    |---|---|---|---|---|
    | installed | remove bridge.py and the stamp if present | `[ok] bridge.py installed` | `"installed"` | stamped; save bridge.py as `shipped` |
    | unchanged | none | `[ok] bridge.py unchanged` | `"unchanged"` | bridge.py == shipped, stamped |
    | updated | bridge.py = `older` (`"# a bridge an earlier agent-deck wrote\n"`), stamp = its sha256 line | `[ok] bridge.py updated` | `"updated"` | bridge.py == shipped, stamped; `filepath.Glob(<condDir>/.bridge.py.previous*)` is exactly `.bridge.py.previous`, holding `older` |
    | symlink | remove bridge.py, then symlink it to `<home>/my-bridge.py` (written `"# mine\n"`); save the stamp's bytes | `session.BridgeSymlinkNotice` | `"kept"` | same link, target bytes unchanged; stamp bytes as saved |

  - `TestConductorSetupCLI_RerunKeepsCustomHeartbeatScript`: fixture with config `""`, name `hb-custom`. `runConductorSetupCLI(t, fx.home, name)` (TestMain's failing stubs; it adds `--json --no-heartbeat`); `fx.writeUserFile(t, <cliConductorDir(t, fx.home, name)>/heartbeat.sh, "#!/bin/sh\n# the director's own heartbeat\nexit 0\n")`; `fx.keep` the paths of `writeReadOnlyServiceFixtures(t, fx.home, 0o444, session.HeartbeatPlistLabel(name), session.SystemdHeartbeatServiceName(name), session.SystemdHeartbeatTimerName(name))`; then `fx.run(t, "conductor", "setup", name)` (heartbeat on, interval 15). Expect `assertUnchanged`; the lines `  [skip] heartbeat.sh has local changes; left unchanged` and `"  [skip] " + hbLabel + ": managed externally (read-only)"` (hbLabel: `HeartbeatPlistLabel(name)` on darwin, else `SystemdHeartbeatTimerName(name)`); stderr without `failed to install heartbeat`; no `mutatingServiceCalls` line containing `conductor-heartbeat`.

- [ ] **Step 4: Run the tests; expect FAIL.** GOTEST `-run '^TestConductorSetupCLI_(ExternalBridgeAndAgentsUntouched|FreshBridgeInstalledAndStamped|RerunKeepsCustomHeartbeatScript)$' ./cmd/agent-deck/`. They compile on the tree Task 8 leaves.
  - `ExternalBridgeAndAgentsUntouched/text` and `/json`: `agent-deck conductor setup steward-probe --no-heartbeat: exit 1` (json: `... --no-heartbeat --json: exit 1`). Stderr: `⚠  Python dependencies install failed.`, then `Error installing bridge.py: bridge.py has local changes; left unchanged`: today's step 7 runs installPythonDeps (:736) and treats Task 4's `*BridgeKeptError` as fatal (:738-741).
  - `FreshBridgeInstalledAndStamped`: `installed/text` passes. The `json` subtests of installed, unchanged and updated: `bridge_script = , want "<outcome>"` and `external = "", want [] (never null)`. `unchanged/text`, `updated/text`: `stdout lacks the line` (today always prints `[ok] bridge.py installed`, :743; Task 4 already passes the file checks). `symlink/text`, `/json`: `agent-deck conductor setup fresh --no-heartbeat: exit 1` (json adds ` --json`), stderr `Error installing bridge.py: bridge.py is a symlink; left unchanged`.
  - `RerunKeepsCustomHeartbeatScript`: both `stdout lacks the line` errors, and a stderr error quoting `Warning: failed to install heartbeat daemon: <hbLabel>: managed externally (read-only)` (:720-721). The files are already unchanged (Tasks 6 and 8).

- [ ] **Step 5: Wire `handleConductorSetup(profile string, args []string)`** in `cmd/agent-deck/conductor_cmd.go` (`errors` is imported, :7). Siblings: migrate-dir's reloads (:1947, :1955) are Task 11's; teardown's uninstalls are Task 10's.
  1. Before `// Step 6` (:710): `external := []string{}` (non-nil, so `--json` prints `[]`). `skipExternal := func(indent string, err error) bool`: when `errors.As` finds a `*session.ExternalServiceError`, append `ext.Label`, print `indent + "[skip] " + ext.Error()` unless `*jsonOutput`, return true; otherwise return false.
  2. Step 6 (:718-724): `kept, err := session.InstallHeartbeatScriptOutcome(name, resolvedProfile)`. An error keeps today's `Warning: failed to install heartbeat script: %v` and installs no daemon. On nil, print `  [skip] heartbeat.sh has local changes; left unchanged` when kept (unless JSON), then run today's `InstallHeartbeatDaemon` if/else with its `Warning: failed to install heartbeat daemon: %v` under `if !skipExternal("  ", err)`.
  3. Step 7 (:728-769): `var plistPath, bridgeScript string`; in the channel branch (:730), after `Installing bridge...`, in this order:
     1. `bridgeExternal := session.BridgeDaemonExternal()`. Non-nil: print `[skip] bridge daemon managed externally; Python deps left to its manager` (unless JSON) and never call `installPythonDeps`. Nil: `depsInstalled = installPythonDeps()` (declared `var depsInstalled bool`).
     2. `res, err := session.InstallBridgeScriptOutcome()` replaces `InstallBridgeScript()`. An error stays fatal: today's `Error installing bridge.py: %v`, then `exitCLI(1)`. That includes Task 4's unsupported-exchange error (NFS): exit 1 is intended, with no fallback (G7). Set `bridgeScript = string(res.Outcome)`; unless JSON, `fmt.Println(res.Notice)` when `res.Outcome == session.BridgeScriptKept`, else `fmt.Printf("[ok] bridge.py %s\n", res.Outcome)`.
     3. Daemon: `bridgeExternal != nil` → `skipExternal("", bridgeExternal)` and no `InstallBridgeDaemon`; else `!depsInstalled` → today's two `[skip] Bridge daemon NOT installed` lines; else today's `InstallBridgeDaemon()` if/else, its `Warning: failed to install bridge daemon` and `Run manually:` lines under `if !skipExternal("", err)` (a plist that became external after the probe).
  4. Step 8 (:771-781): today's if/else, its `Warning: failed to install transition notifier daemon` and `Tip:` lines under `if !skipExternal("", err)`. `notifierDaemonPath` and `[ok] Transition notifier daemon installed` stay on the nil branch; the Background guard still returns `(path, nil)`, reported as today.
  5. JSON (:783-804): `"external": external` in the map literal; `if bridgeScript != "" { data["bridge_script"] = bridgeScript }` beside the `daemon` check; `daemon` and `notifier_daemon` unchanged.

- [ ] **Step 6: Run the setup tests; expect PASS.** GOTEST `-run '^(TestConductorSetup|TestInstallPythonDeps|TestServiceManagersStubbedForCLI)' ./cmd/agent-deck/`. Expected: `ok  	github.com/asheshgoplani/agent-deck/cmd/agent-deck`, covering the three new tests, `TestConductorSetupCLI_BareRerunKeepsAgent_Issue2434` (no channel: `external: []`, no `bridge_script`), the in-process `TestConductorSetup_*` tests and `TestInstallPythonDepsPinsEveryPackage`.

- [ ] **Step 7: Update the docs.** Lines verified at 6333ffde; llms.txt, llms-full.txt and site/index.html hold no copy.
  - `cli-reference.md:1115` (heartbeat timers) gains: " Setup keeps a `heartbeat.sh` that is not agent-deck's generated script, printing `[skip] heartbeat.sh has local changes; left unchanged`."
  - `cli-reference.md:1117` becomes: "- With a chat channel configured (Telegram, Slack, Discord or Mattermost), setup installs `bridge.py` and the bridge daemon. It writes `bridge.py` only when the file is missing, equals the shipped bridge, or is unchanged since agent-deck wrote it (agent-deck records its sha256 in `.bridge.py.sha256` and keeps the copy it replaces as `.bridge.py.previous`). Any other `bridge.py`, including one written before the stamp existed, or a symlink, is left as is, with a `[skip] bridge.py ...` line."
  - After `cli-reference.md:1118` add: "- A bridge, transition-notifier or heartbeat plist or unit that is read-only or a symlink is managed externally: setup never unloads, rewrites or reloads it, and prints `[skip] <label>: managed externally (read-only)` (or `(symlink)`). With an external bridge agent, setup also leaves the Python dependencies to that agent's manager." and "- `setup --json` adds `bridge_script` (`installed`, `updated`, `unchanged` or `kept`; with a chat channel) and `external` (the labels of the agents left alone; `[]` when none)."
  - `README.md:699` gains: " A notifier plist or unit that is read-only or a symlink is managed externally: setup leaves it alone and prints `[skip] <label>: managed externally (...)`."
  - `docs/conductor/telegram.md:91`: "installed by `setup`." becomes "installed by `setup` (a `bridge.py` you changed, or a symlink, is kept as is)."
  - `docs/conductor/README.md:208` (heartbeat agent) gains: " A heartbeat plist or unit that is read-only or a symlink is managed externally: setup leaves it alone. Setup also keeps a `heartbeat.sh` you wrote."
  - `docs/conductor/mattermost.md:39`: "for the bridge;" becomes "for the bridge (unless the bridge agent is managed externally, with a read-only or symlinked plist or unit: its manager then provides it);".
  - `config-reference.md:695` and `:697` (re-run setup to regenerate the heartbeat or bridge daemon) each gain: "A plist or unit that is read-only or a symlink is managed externally: setup leaves it alone, so change it through its manager."

- [ ] **Step 8: Commit.** GOFMT `cmd/agent-deck/conductor_cmd.go cmd/agent-deck/conductor_service_fixtures_test.go cmd/agent-deck/conductor_setup_external_test.go`. `git add` those three and `skills/agent-deck/references/cli-reference.md skills/agent-deck/references/config-reference.md README.md docs/conductor/telegram.md docs/conductor/README.md docs/conductor/mattermost.md`. COMMIT with the message in `~/Products/agent-deck-conductor-setup-fix/plan-work/commit-T09.txt`:
  - Subject: `fix(conductor): leave a kept bridge and external agents alone in setup`
  - Body: "Setup asks whether the bridge agent is managed externally before it runs pip, reports bridge.py as installed, updated, unchanged or kept, says when it keeps a custom heartbeat.sh, and prints a [skip] line, not a warning, for a read-only or symlinked plist or unit. --json adds bridge_script and external. The CLI and config references, README and conductor docs say so."

### Task 10: `conductor teardown` wiring

**Files:**
- Modify: `internal/session/conductor.go`, `UninstallHeartbeatDaemon` (base 0953876e :3210-3235; Tasks 2-4 and 6-8 edit the file first, so find it by name).
- Modify: `cmd/agent-deck/conductor_cmd.go`, in `handleConductorTeardown`: the `--all` daemon stop (base :971-991), `disableHeartbeat` (:1004-1012) and the shared-file cleanup (:1135-1147); new `reportTeardownStop` after `exitIncompleteTeardown` (:1192-1198). Task 9 shifts these lines, so find each block by its quoted code.
- Create: `cmd/agent-deck/conductor_teardown_external_test.go` (package main).
- Test: `internal/session/conductor_heartbeat_tick_test.go` (append after `TestLaunchdHeartbeatUninstallRefusesFailedUnload`; add `"errors"` to its imports if absent).

**Interfaces:** (needs Tasks 4, 6, 7, 9)
- Consumes:
  - Task 4: `func RemoveBridgeScript() (removed bool, err error)`. Only when bridge.py equals the embedded bytes or matches its stamp (64 lower-case hex digits of sha256, then "\n") does it remove bridge.py, `.bridge.py.sha256` and `.bridge.py.previous`, returning `(true, nil)`. It never touches bridge.py.backup. It returns `(false, nil)` when bridge.py is missing, not a regular file (a symlink) or customized.
  - Task 6: `type ExternalServiceError struct { Label, Path, Reason string }`, with `Error()` = `<Label>: managed externally (<Reason>)` and Reason "symlink" or "read-only" (`mode&0o222 == 0` counts even for root). `UninstallBridgeDaemon() error` and `UninstallTransitionNotifierDaemon() error` return it unwrapped. On launchd they call `removeLaunchAgent(label, plistPath, false)`, so a failed unload is ignored. A non-regular path returns `refusing to manage <label>: <path> is not a regular file (<type>)`. Session test only (service_files_test.go): `writeServiceFixture(t *testing.T, path, content string, perm os.FileMode)`, `wantFileBytes(t *testing.T, path, want string)`, `wantCalls(t *testing.T, got []string, want ...string)`.
  - Task 7: `removeSystemdUnits(label string, units []systemdUnitFile, disableUnit string, strictDisable bool) error`. Its external pass runs first and returns Task 6's error unchanged; after it, no unit file means no call. An external unit's Label is the unit the Install function enables. The bridge and notifier are lenient: `disable --now <unit>`, remove, `daemon-reload`.
  - Task 9 (`cmd/agent-deck/conductor_service_fixtures_test.go`):
    - `writeReadOnlyServiceFixtures(t *testing.T, home string, mode os.FileMode, labels ...string) []string`: a label ending in `.service` or `.timer` maps to `<home>/.config/systemd/user/<label>`, any other to `<home>/Library/LaunchAgents/<label>.plist`; it writes each file, chmods it to `mode` and returns the paths in label order.
    - `installRecordingServiceFakes(t *testing.T, listLoaded bool) (pathDir, logPath string)`: `launchctl` and `systemctl` fakes that append `<tool> $*` to logPath. With listLoaded, `list` and `is-active` print `$*` and exit 0 (else exit 1): IsBridgeDaemonRunning and IsTransitionNotifierDaemonRunning need that (launchd also needs the output) before the `--all` stop runs. Every other verb exits 0. Tests pass `"PATH=" + pathDir + string(os.PathListSeparator) + os.Getenv("PATH")` as `runAgentDeckEnv`'s extraEnv: agentDeckTestEnv appends it last and os/exec keeps the last PATH, so the fakes shadow TestMain's failing stubs.
    - `mutatingServiceCalls(t *testing.T, logPath string) []string`.
  - Task 2 (session test only): `installServiceManagerFake(t *testing.T) *serviceManagerFake` and `MutatingCalls() []string`.
- Produces:
  - `UninstallHeartbeatDaemon(name string) error`, signature unchanged. On an `*ExternalServiceError` it still sets HeartbeatEnabled=false and saves meta, then returns that error.
  - `func reportTeardownStop(err error, daemon, okLine string)`, used only in conductor_cmd.go.
  - The pinned lines, each printed only when `!*jsonOutput` (no JSON key is added). From the `--all` stop: `[skip] ` + `Error()` on stdout, or `Warning: failed to stop <bridge|transition notifier> daemon: <err>` on stderr. From `disableHeartbeat`: `  [skip] <Error()>; heartbeat turned off in meta.json`. From `--all --remove`: `[skip] bridge.py has local changes; kept`, `[skip] bridge.py is a symlink; kept`, or `Warning: failed to remove bridge.py: <err>` on stderr.

Structural pins: the body of `handleConductorTeardown` keeps `applyConductorRuntimeAction(` before `session.TeardownConductor(` and keeps `session.CleanupRemovedSession(`. It gains no `RemoveSessionAndVerify(` (`conductor_runtime_lifecycle_test.go:23`, `session_closed_paths_test.go:61`). `extractFuncBody` counts braces even inside string literals, so no new string in that body may contain `{` or `}`. No test in this task calls `t.Parallel()` or needs a root skip (G4).

- [ ] **Step 1: Write the failing session test** `TestUninstallHeartbeatDaemon_ExternalTurnsOffMetaAndReturnsError`.
  - `label` is `HeartbeatPlistLabel("ops")` on darwin and `SystemdHeartbeatTimerName("ops")` on linux; any other GOOS calls `t.Skipf("no heartbeat service manager on %s", runtime.GOOS)`.
  - Setup: `setupHeartbeatTickTest(t, "ops")` (its own temp HOME, so the 0444 files never reach the package's TestMain HOME), `fake := installServiceManagerFake(t)`, `SaveConductorMeta(&ConductorMeta{Name: "ops", Profile: "default", HeartbeatEnabled: true})`, then `writeServiceFixture(t, p, "external\n", 0o444)` for each p of `HeartbeatPlistPath("ops")`, `SystemdHeartbeatServicePath("ops")` and `SystemdHeartbeatTimerPath("ops")`, so either platform branch sees an external agent.
  - After `err := UninstallHeartbeatDaemon("ops")`, assert in order: (1) `errors.As(err, &ext) && ext.Label == label && ext.Reason == "read-only"`, else `t.Fatalf("UninstallHeartbeatDaemon = %v, want %s: managed externally (read-only)", err, label)`; (2) `LoadConductorMeta("ops")` gives `HeartbeatEnabled == false`, else `t.Fatalf("an external heartbeat must still be turned off in meta.json: meta=%+v err=%v", meta, err)`; (3) `wantFileBytes(t, p, "external\n")` for each fixture; (4) `wantCalls(t, fake.MutatingCalls())`.

- [ ] **Step 2: Run it and confirm it fails.** GOTEST `-run '^TestUninstallHeartbeatDaemon_External' ./internal/session/`. Expected: FAIL with `an external heartbeat must still be turned off in meta.json`. After Tasks 6/7 the platform step returns the error, and today's `if err != nil { return err }` returns before the meta is saved.

- [ ] **Step 3: Implement the external branch of `UninstallHeartbeatDaemon(name string) error`** in `internal/session/conductor.go`. Keep the platform switch and today's meta update: the #2354 raw branch (with its comment) when `meta.Warning != ""`, else `HeartbeatEnabled = false` and `SaveConductorMeta`. Only the early return changes. An `*ExternalServiceError` found by `errors.As` is held; the meta update runs; the function returns the meta update's error if there is one, else the held error. Any other platform error still returns before the meta is touched. With nothing held, return the meta update's result, never the held variable: a nil `*ExternalServiceError` stored in an `error` is non-nil. Add `"errors"` to the imports if absent. Doc comment, second sentence: "A timer managed externally is left in place: the heartbeat is still turned off in meta.json and its *ExternalServiceError is returned for the caller to report."

- [ ] **Step 4: Run it and confirm it passes.** GOTEST `-run '^(TestUninstallHeartbeatDaemon_|TestLaunchdHeartbeat)' ./internal/session/`. Expected: `ok  	github.com/asheshgoplani/agent-deck/internal/session`. The run includes UnknownAgentDisablesAndPreservesMeta (the raw path, unchanged). On Linux only, it also includes StopFailureKeepsEnabled: a non-external stop failure still returns before the meta is touched.

- [ ] **Step 5: Write the failing CLI tests** in `cmd/agent-deck/conductor_teardown_external_test.go` (imports crypto/sha256, encoding/json, fmt, os, path/filepath, runtime, slices, strings, testing, internal/session). The conductors have only a meta.json, as in `TestConductorTeardownReportsHeartbeatOff`: with no session row there is no runtime action, tmux or service-unit path. Local helpers and rules:
  - `externalTeardownHome(t *testing.T, heartbeat bool) string`: `sandboxConductorHome(t)`, then `session.SaveConductorMeta(&session.ConductorMeta{Name: "ops", Profile: "default", HeartbeatEnabled: heartbeat})`; returns `os.Getenv("HOME")`.
  - `type teardownAgent struct { label, file string; stop []string }` and `teardownAgents(t *testing.T, name string) (bridge, notifier, heartbeat teardownAgent)`, filled from the table below. Tests call it after `externalTeardownHome`, so the session path functions resolve under the test's HOME. It calls `t.Fatal` on a path error and `t.Skipf` unless GOOS is darwin or linux.
  - `teardownFileState(t *testing.T, path string) string`: `"missing"` when Lstat reports not-exist, else `fmt.Sprintf("%v %q", info.Mode(), content)`; any other Lstat or read error is `t.Fatal`.
  - Agent `a`'s fixture is `writeReadOnlyServiceFixtures(t, home, mode, a.label)`; the test records `teardownFileState(t, a.file)` at once, and `t.Fatalf`s if it is `"missing"` (the helper's path and the binary's must agree). A file is "kept" when its state after the run is the same.

| agent | darwin label / file | linux label / file | stop: the exact mutating calls of an owned agent |
|---|---|---|---|
| bridge | `session.LaunchdPlistName` / `LaunchdPlistPath()` | `"agent-deck-conductor-bridge.service"` / `SystemdBridgeServicePath()` | darwin `"launchctl unload " + file`; linux `"systemctl --user disable --now " + label`, then `"systemctl --user daemon-reload"` |
| notifier | `session.TransitionNotifierLaunchdPlistName` / `TransitionNotifierLaunchdPlistPath()` | `"agent-deck-transition-notifier.service"` / `SystemdTransitionNotifierServicePath()` | same form |
| heartbeat | `session.HeartbeatPlistLabel(name)` / `HeartbeatPlistPath(name)` | `session.SystemdHeartbeatTimerName(name)` / `SystemdHeartbeatTimerPath(name)` | not used |

  **`TestConductorTeardownExternal_AllSkipsExternalAgents`**, one subtest per row: `home := externalTeardownHome(t, false)`, `bridge, notifier, _ := teardownAgents(t, "ops")`, `pathDir, logPath := installRecordingServiceFakes(t, true)`, both fixtures at the row's modes, then `conductor teardown --all` through `runAgentDeckEnv` with the PATH entry. It asserts: exit 0, else `t.Fatalf("teardown --all: exit=%d stdout=%q stderr=%q", ...)`; each listed line is a whole stdout line, else `missing line %q in:\n%s`; `strings.Count(stdout, "stopped and removed")` equals the count, else `%d agents reported stopped and removed, want %d`; `mutatingServiceCalls` equals the row's calls (`slices.Equal`; "none" is empty); a 0o444 file is kept and a 0o644 file is `"missing"`. On Linux `stop` ends with `daemon-reload`, from Task 7's lenient removal; the conductor's own heartbeat uninstall makes no call, because no unit file exists.

| subtest | bridge | notifier | stdout lines | count | mutating calls |
|---|---|---|---|---|---|
| both-external | 0o444 | 0o444 | `"[skip] " + bridge.label + ": managed externally (read-only)"`, `"[skip] " + notifier.label + ": managed externally (read-only)"` | 0 | none |
| mixed-ownership | 0o444 | 0o644 | the bridge `[skip]` line, `[ok] Transition notifier daemon stopped and removed` | 1 | `notifier.stop` |
| reverse-mix | 0o644 | 0o444 | `[ok] Daemon stopped and removed`, the notifier `[skip]` line | 1 | `bridge.stop` |

  - **`TestConductorTeardownExternal_AllWarnsWhenStopFails`**: AllSkipsExternalAgents's setup, but `bridge.file` and `notifier.file` are each a non-empty directory (`os.MkdirAll(filepath.Join(file, "keep"), 0o755)`), so both stops return Task 6's non-regular error on both platforms. It asserts exit 0, count 0 and no `mutatingServiceCalls`, and that stderr contains `"Warning: failed to stop bridge daemon: refusing to manage " + bridge.label` and `"Warning: failed to stop transition notifier daemon: refusing to manage " + notifier.label`, else `stderr lacks %q:\n%s`.
  - **`TestConductorTeardownExternal_HeartbeatSkipTurnsOffMeta`**: `home := externalTeardownHome(t, true)`, `_, _, heartbeat := teardownAgents(t, "ops")`, `pathDir, logPath := installRecordingServiceFakes(t, false)`, then `heartbeat`'s fixture at 0o444 (on Linux the timer alone: Task 7 counts the pair external when either unit is). `conductor teardown ops` exits 0 with the stdout line `"  [skip] " + heartbeat.label + ": managed externally (read-only); heartbeat turned off in meta.json"`, else `t.Fatalf("teardown ops: exit=%d, want line %q\nstdout=%q stderr=%q", ...)`. Then `session.LoadConductorMeta("ops")` has `HeartbeatEnabled == false`, else `t.Fatalf("heartbeat must be off in meta.json: meta=%+v err=%v", ...)`. A second run, `conductor teardown ops --json`, exits 0 and its stdout unmarshals with `"success": true`, so the skip stays off JSON stdout; else `t.Fatalf("teardown ops --json: exit=%d stdout=%q stderr=%q", ...)`. The fixture is kept, and `mutatingServiceCalls` is empty.
  - **`TestConductorTeardownExternal_RemoveKeepsCustomizedBridge`**: no PATH fakes, because TestMain's failing stubs make IsBridgeDaemonRunning false and so the `--all` stop never runs. Each subtest calls `externalTeardownHome(t, false)`, writes the files at 0o644 into `session.ConductorDir()`, and requires `runAgentDeck(t, home, "conductor", "teardown", "--all", "--remove")` to exit 0. Here `B = "#!/usr/bin/env python3\n# hand-edited bridge\n"`. For each of the two lines `[skip] bridge.py has local changes; kept` and `[skip] bridge.py is a symlink; kept`, `printed` (a whole stdout line) must equal whether the row names it, else `%q printed=%v, want %v`; the other messages are `%s must be removed: err=%v` and `%s must be left unchanged: %q err=%v` (a link is compared by its `os.Readlink` target).

| subtest | files | line printed | after |
|---|---|---|---|
| customized | `bridge.py` = B; `bridge.py.backup` = `"# the user's own bridge\n"` | `[skip] bridge.py has local changes; kept` | all unchanged |
| stamped | customized's, plus `.bridge.py.sha256` = `fmt.Sprintf("%x\n", sha256.Sum256([]byte(B)))` and `.bridge.py.previous` = `"# previous bridge\n"` | neither | bridge.py, the stamp and .bridge.py.previous removed; backup unchanged |
| symlink | customized's backup; `bridge.py` a symlink to `<home>/my-bridge.py` holding B; the stamp of B | `[skip] bridge.py is a symlink; kept` | the link, its target, the stamp and the backup unchanged |
| absent | `bridge.py.backup` only | neither | backup unchanged |

- [ ] **Step 6: Run them and confirm they fail.** GOTEST `-run '^TestConductorTeardownExternal_' ./cmd/agent-deck/`. Expected: FAIL, as follows:
  - `AllSkipsExternalAgents/*`: `missing line` for each row's `[skip]` lines (darwin: `[skip] com.agentdeck.conductor-bridge: managed externally (read-only)` and the `com.agentdeck.transition-notifier` one; Linux: the unit names), and `2 agents reported stopped and removed, want 0` (both-external) or `want 1` (the two mixes). Tasks 6/7 already leave the external files alone, but the CLI discards the error and prints both `[ok]` lines.
  - `AllWarnsWhenStopFails`: `stderr lacks` for both Warning lines, and `2 agents reported stopped and removed, want 0`.
  - `HeartbeatSkipTurnsOffMeta`: `exit=1`, with stderr `Error disabling heartbeat for ops: <label>: managed externally (read-only)`. Step 3 already turns the meta off, but `disableHeartbeat` still exits 1 on any error.
  - `RemoveKeepsCustomizedBridge`: /customized reports `"[skip] bridge.py has local changes; kept" printed=false, want true`, then `bridge.py must be left unchanged` (today's cleanup removes bridge.py with a silent `os.Remove`). /symlink reports `"[skip] bridge.py is a symlink; kept" printed=false, want true` and `bridge.py must be left unchanged` (today's `os.Remove` unlinks the link). /stamped reports `.bridge.py.sha256 must be removed` and `.bridge.py.previous must be removed` (today's cleanup removes only bridge.py). /absent passes; it pins that a missing bridge.py prints neither line.

- [ ] **Step 7: Wire `handleConductorTeardown` in `cmd/agent-deck/conductor_cmd.go`.** `errors` is already imported. Only these blocks change:
  1. After `exitIncompleteTeardown`, add `func reportTeardownStop(err error, daemon, okLine string)`. An `*session.ExternalServiceError` found by `errors.As` prints `"[skip] " + ext.Error()` to stdout. Any other non-nil error prints `Warning: failed to stop <daemon> daemon: <err>` to stderr. Nil prints okLine.
  2. In the `--all` stop, `stopErr := session.UninstallBridgeDaemon()` replaces `_ = session.UninstallBridgeDaemon()`, and the print inside `!*jsonOutput` becomes `reportTeardownStop(stopErr, "bridge", "[ok] Daemon stopped and removed")`. The notifier block changes the same way, with `"transition notifier"` and `"[ok] Transition notifier daemon stopped and removed"`. The `Stopping ... daemon...` lines stay.
  3. In `disableHeartbeat`, an `*session.ExternalServiceError` found by `errors.As` prints `fmt.Printf("  [skip] %s; heartbeat turned off in meta.json\n", ext.Error())` unless `*jsonOutput`, then falls through to `conductor.HeartbeatEnabled = false`. Any other error keeps today's stderr `Error disabling heartbeat for %s: %v` line and `exitCLI(1)`.
  4. In the shared cleanup, `session.RemoveBridgeScript()` replaces `_ = os.Remove(filepath.Join(condDir, "bridge.py"))`, and the other removals stay. Unless `*jsonOutput`, an error prints `Warning: failed to remove bridge.py: <err>` to stderr. `(false, nil)` prints `[skip] bridge.py is a symlink; kept` when `os.Lstat(filepath.Join(condDir, "bridge.py"))` reports a symlink, `[skip] bridge.py has local changes; kept` when it reports any other entry, and nothing when it fails, because `(false, nil)` also means missing. Name the result `bridgeRemoved`, because the outer `removed` slice is read again for the JSON.

- [ ] **Step 8: Run the new tests and their neighbours and confirm they pass.** GOTEST `-run '^(TestConductorTeardown|TestRuntimeLifecycle_Conductor|TestSessionDeletePathsUseSharedCleanup|TestServiceManagersStubbedForCLI)' ./cmd/agent-deck/`. Expected: `ok  	github.com/asheshgoplani/agent-deck/cmd/agent-deck`. The run includes both structural pins and TestConductorTeardownReportsHeartbeatOff.

- [ ] **Step 9: Commit.** GOFMT `internal/session/conductor.go internal/session/conductor_heartbeat_tick_test.go cmd/agent-deck/conductor_cmd.go cmd/agent-deck/conductor_teardown_external_test.go` (expect no output); `git add` those four files; COMMIT with this message in `~/Products/agent-deck-conductor-setup-fix/plan-work/commit-T10.txt`, followed by the two attribution lines (G1):

```text
fix(conductor): leave external agents and a customized bridge alone in teardown

teardown --all reports a bridge or notifier agent whose plist or unit is
read-only or a symlink as [skip] instead of "stopped and removed", and
warns instead of claiming success when stopping or removing an agent
fails. Turning off an external heartbeat still records
heartbeat_enabled=false and no longer fails the teardown. --all --remove
deletes bridge.py, its stamp and .bridge.py.previous only when bridge.py
is the shipped bridge or still matches its stamp, says so when it keeps
them, and never touches bridge.py.backup.
```

### Task 11: `conductor migrate-dir`: refuse to strand an external agent, carry the stamp

**Files:**
- Modify: `internal/session/conductor_migrate_dir.go` (lines as of 6333ffde; Task 8 only rewrites doc step 6 in place, so they hold): `ConductorDirMigrateResult` `:184-202` (the `Refused` doc `:191-195`, `BridgeReinstalled` `:201`), the `MigrateConductorDir` doc steps 1 and 6 (`:221-225`, `:230-231`), the blocker sites `:272` (dry-run) and `:301-305` (apply), the bridge reinstall `:371-376`, and a new helper after `planBlockers` (`:639-659`).
- Modify: `cmd/agent-deck/conductor_cmd.go`: `bridgeDaemonInstalled` (`:1850-1862` at 6333ffde), `handleConductorMigrateDir` (reconcile and JSON map, `:1923-1977`) and `printMigrateDirSummary` (`:1997-2091`). Tasks 9 and 10 shift these lines; anchor on the function names.
- Test: `internal/session/conductor_migrate_dir_test.go` (append at the end, after Task 8's additions; add the import `bytes`).
- Create: `cmd/agent-deck/conductor_migrate_dir_test.go` (`package main`; imports `encoding/json`, `os`, `path/filepath`, `strings`, `testing`, `github.com/asheshgoplani/agent-deck/internal/session`).

**Interfaces:**
- Consumes (needs Tasks 4, 7, 8 and 9):
  - Task 4: `func InstallBridgeScriptOutcome() (BridgeScriptResult, error)` (takes no lock), `func installBridgeScriptAt(dir string, embedded []byte) (BridgeScriptResult, error)`, `type BridgeScriptResult struct { Outcome BridgeScriptOutcome; Notice string }`, `BridgeScriptUpdated`, `BridgeScriptUnchanged`, `BridgeScriptKept`, `bridgeStampName`, `bridgePreviousName`, `BridgeKeptNotice`.
  - Task 6: `type ExternalServiceError struct { Label, Path, Reason string }`, `Error()` = `<Label>: managed externally (<Reason>)`; a 0444 file is read-only for every uid, so no root skip. Test helper `writeServiceFixture(t *testing.T, path, content string, perm os.FileMode)` (`service_files_test.go`).
  - Task 7: `func BridgeDaemonExternal() *ExternalServiceError`, `func HeartbeatDaemonExternal(name string) *ExternalServiceError` (they read files only). With Task 6, `InstallBridgeDaemon` and `InstallHeartbeatDaemon` return the error unwrapped, checked before generating any plist or unit, so no venv python3 is needed.
  - Task 8: `InstallHeartbeatScript(name, profile string) error` keeps a custom script; Task 8 converted the `:57` fixture of `TestMigrateConductorDir_MovesHomesAndPreservesUserState` and added `TestMigrateConductorDir_KeepsCustomHeartbeatScript`.
  - Task 9 (`cmd/agent-deck/conductor_service_fixtures_test.go`): `writeReadOnlyServiceFixtures(t *testing.T, home string, mode os.FileMode, labels ...string) []string` (relied on: a launchd label is written at `home/Library/LaunchAgents/<label>.plist`, a `.service` or `.timer` name at `home/.config/systemd/user/<name>`), `installRecordingServiceFakes(t *testing.T, listLoaded bool) (pathDir, logPath string)`, `mutatingServiceCalls(t *testing.T, logPath string) []string`.
- Produces:
  - `ConductorDirMigrateResult` gains `BridgeScript BridgeScriptOutcome` (what the reinstall at the new base did; "" when it did not run or failed) and `BridgeNotice string` (the kept notice). `BridgeReinstalled` stays, true for installed, updated and unchanged.
  - `func externalServiceBlockers(source string, plan []migratePlanEntry) []string`.
  - migrate-dir output: JSON key `"bridge_script"`; the reload's `[skip] <ExternalServiceError.Error()>` line (stdout, or stderr under `--json`); the summary's `"  " + BridgeNotice` line.

- [ ] **Step 1: Write the failing blocker test** `TestMigrateConductorDir_ExternalAgentBlocksMove` at the end of `internal/session/conductor_migrate_dir_test.go`, one subtest per row below.
  - Fixture: `home, xdgConfigHome, xdgDataHome := setupSessionXDGPathEnv(t)`; `defaultBase := filepath.Join(xdgDataHome, "agent-deck", "conductor")`; `writeConductorDirConfig(t, xdgConfigHome, defaultBase)`, then keep the bytes of `<xdgConfigHome>/agent-deck/config.toml`; `writeConductorHome(t, defaultBase, "alpha", ...)` with `meta.json` = `{"name":"alpha","profile":"default"}`; `defaultBase/bridge.py` = `"# hand-edited bridge\n"`. No `installServiceManagerFake`: `MigrateConductorDir` reaches no service-manager seam.
  - Each unit: `writeServiceFixture(t, filepath.Join(home, filepath.FromSlash(rel)), "managed elsewhere\n", 0o444)`. Both platforms' files are written; the probe reads the host's, so the expected text takes `ext.Label` from the probe.

| subtest | HOME-relative 0444 units | `ext` (`t.Fatal("probe reports no external unit for the 0444 fixture")` when nil) |
|---|---|---|
| bridge | `"Library/LaunchAgents/"+LaunchdPlistName+".plist"`, `".config/systemd/user/"+systemdBridgeServiceName` | `BridgeDaemonExternal()` |
| heartbeat | `"Library/LaunchAgents/"+HeartbeatPlistLabel("alpha")+".plist"`, `".config/systemd/user/"+SystemdHeartbeatServiceName("alpha")`, `".config/systemd/user/"+SystemdHeartbeatTimerName("alpha")` | `HeartbeatDaemonExternal("alpha")` |

  - For `apply` in `false, true` (dry-run reports it, `--apply` refuses), `res, err := MigrateConductorDir(ConductorDirMigrateOptions{Target: target, Apply: apply})` with `target := filepath.Join(t.TempDir(), "vault-conductors")` gives `err == nil`, `res.Refused`, `!res.ConfigWritten` and `res.Blockers` exactly `[]string{ext.Label + " is managed externally and points at " + defaultBase + "; change its manager's configuration first"}`; otherwise `t.Fatalf("apply=%v: err=%v res=%+v, want the one blocker %q", ...)`.
  - Afterwards `assertFileContains` finds `# hand-edited bridge` in `defaultBase/bridge.py` and `"alpha"` in `defaultBase/alpha/meta.json`, config.toml is `bytes.Equal` to its earlier bytes, and `assertNotExist(t, target)` holds.

- [ ] **Step 2: Run it; expect failure.** GOTEST `-run 'TestMigrateConductorDir_' ./internal/session/`. Expected: both subtests FAIL with `apply=false: err=<nil> res=&{DryRun:true ... Refused:false Blockers:[] ...}`, because planning knows only skip-exists and reject-conflict. The 18 existing `TestMigrateConductorDir_*` tests (17 plus Task 8's) pass; a failure at `:97` of `_MovesHomesAndPreservesUserState` means Task 8's fixture conversion is missing.

- [ ] **Step 3: Implement `func externalServiceBlockers(source string, plan []migratePlanEntry) []string`** after `planBlockers` in `conductor_migrate_dir.go`.
  - One blocker when `BridgeDaemonExternal()` is non-nil, then one per plan entry with `e.action.IsHome` (whatever its action) whose `HeartbeatDaemonExternal(e.action.Name)` is non-nil, each `fmt.Sprintf("%s is managed externally and points at %s; change its manager's configuration first", ext.Label, source)`. Doc comment: those plists and units hard-code the current paths and agent-deck may not rewrite them; the transition notifier references no conductor path, so it never blocks.
  - At `:272` (dry-run) and `:301` (apply), when `!noop`, append `externalServiceBlockers(source, plan)...` to `planBlockers(plan)` before `Refused` is derived. A no-op moves nothing, so it strands nothing.
  - Add the external-agent case to the `Refused` doc (`:191-195`) and to step 1 of the `MigrateConductorDir` doc (`:221-225`).

- [ ] **Step 4: Run it; expect a pass.** Step 2's command. Expected: `ok  	github.com/asheshgoplani/agent-deck/internal/session`.

- [ ] **Step 5: Write the failing carry test** `TestMigrateConductorDir_ApplyCarriesBridgeStampAndCustomHeartbeat` in the same file.
  - Fixture: `_, _, xdgDataHome := setupSessionXDGPathEnv(t)` with no config, so the base is `defaultBase := <xdgDataHome>/agent-deck/conductor`. `writeConductorHome` gives alpha `meta.json` = `{"name":"alpha","profile":"default"}` and `heartbeat.sh` = `custom := "#!/bin/sh\n# my own heartbeat\n"`. In `defaultBase`, at 0o644: `bridge.py` = `older := "# a bridge an earlier agent-deck wrote\n"`, `bridgeStampName` = `fmtHash(older)+"\n"`, `bridgePreviousName` = `"# two releases ago\n"`.
  - `MigrateConductorDir(ConductorDirMigrateOptions{Target: target, Apply: true})` (target as in Step 1) returns `err == nil`, then:

| check | want |
|---|---|
| `res.BridgeScript`, `res.BridgeReinstalled`, `res.BridgeNotice` | `BridgeScriptUpdated`, `true`, `""`: the stamp moved with the bridge, so the reinstall may replace it |
| `filepath.Glob(filepath.Join(target, bridgePreviousName+"*"))` | exactly one match, and `target/.bridge.py.previous` holds `older` |
| `bridgeStampName`, `bridgePreviousName` under `defaultBase` | `assertNotExist` |
| `installBridgeScriptAt(target, []byte(conductorBridgePy))` | `err == nil` and `Outcome == BridgeScriptUnchanged` (not kept) |
| `target/alpha/heartbeat.sh` | exactly `custom` |

- [ ] **Step 6: Run it; expect failure.** Step 2's command. Expected: the build fails with `res.BridgeScript undefined (type *ConductorDirMigrateResult has no field or method BridgeScript)`. Once it builds, the `heartbeat.sh` row already holds through Task 8's keep rule; at 0953876e it would fail.

- [ ] **Step 7: Implement the result fields and the reinstall** in `conductor_migrate_dir.go`.
  - After `BridgeReinstalled` (`:201`) add `BridgeScript BridgeScriptOutcome` and `BridgeNotice string` with the Produces docs; document `BridgeReinstalled` as true for installed, updated and unchanged.
  - Replace `:371-376` with `r, err := InstallBridgeScriptOutcome()`. On error keep today's `sessionLog.Warn("conductor_migrate_dir_bridge_reinstall_failed", ...)`; replacing a stamped bridge on a filesystem without atomic exchange (an NFS home) fails here with Task 4's remedy error, so the move stays committed and `BridgeScript` stays "". Otherwise set `BridgeScript = r.Outcome`, `BridgeNotice = r.Notice`, `BridgeReinstalled = r.Outcome != BridgeScriptKept`.
  - Its comment replaces "bridge.py is fully regenerable": the bridge is replaced only when missing or still matching its stamp, which moved with it; a failure must not abort the committed move; the call runs under `acquireConductorBaseLock`, so the installer must not take that lock (Task 4's does not).
  - In doc step 6 (`:230-231`, as Task 8 worded it), "reinstall base bridge.py" becomes "reinstall base bridge.py unless it has local changes".

- [ ] **Step 8: Run it; expect a pass.** Step 2's command. Expected: `ok`.

- [ ] **Step 9: Write the failing CLI tests** in `cmd/agent-deck/conductor_migrate_dir_test.go`. No test calls `t.Parallel`. Each starts with `home := t.TempDir()`; tests 1 and 3 add `from := filepath.Join(home, "old-conductor")` and `to := filepath.Join(home, "new-conductor")`. Other fixture files are written at 0o644.
  - Tests 1 and 2 call `t.Setenv("HOME", home)` so the in-process probes read the binary's files. Both write the bridge units with `writeReadOnlyServiceFixtures(t, home, 0o444, session.LaunchdPlistName, "agent-deck-conductor-bridge.service")`; test 2 also writes alpha's heartbeat units (labels `session.HeartbeatPlistLabel("alpha")`, `session.SystemdHeartbeatServiceName("alpha")`, `session.SystemdHeartbeatTimerName("alpha")`). Then both take `br := session.BridgeDaemonExternal()`, and only test 2 takes `hb := session.HeartbeatDaemonExternal("alpha")` (`t.Fatal` when nil).
  - Tests 1 and 2 run `runAgentDeckEnv(t, home, "", []string{"PATH=" + pathDir + string(os.PathListSeparator) + os.Getenv("PATH")}, "conductor", "migrate-dir", ...)` with `pathDir, logPath := installRecordingServiceFakes(t, false)`, and end by asserting `mutatingServiceCalls(t, logPath)` is empty. Test 3 uses plain `runAgentDeck` (TestMain's failing stubs).
  - Pass the target first and `--from=` with `=`: the positional scan takes the first non-flag argument as the target, so a bare `--from /old /new` swaps them.
  - Test 2 runs its two rows in each of two subtests, each with its own home: `read-only` (the fixtures above) and `dangling` (the two bridge units are instead symlinks to `<home>/gone`, which does not exist, so `br.Reason == "symlink"`).

| test | fixtures | args after `migrate-dir` | expected |
|---|---|---|---|
| 1 `TestConductorMigrateDir_ExternalBridgeAgentRefuses` | bridge units; `from/bridge.py` = `"# hand-edited bridge\n"` | `to --from=<from>` | exit 0; stdout has `"    - " + br.Label + " is managed externally and points at " + from + "; change its manager's configuration first\n"` and `"  Note: this plan would be REFUSED as-is — resolve the blockers above first."` |
| 1, same | same | `to --from=<from> --apply` | exit 1; stdout has `REFUSED — nothing was moved and [conductor].dir was NOT changed.` |
| 2 `TestConductorMigrateDir_NoOpApplyReportsKeptBridgeAndExternalAgents` | bridge and alpha heartbeat units; `dir := filepath.Join(home, "conductor")`; `dir/alpha/meta.json` = `{"name":"alpha","profile":"default","heartbeat_enabled":true,"heartbeat_interval":15}`; `dir/bridge.py` = `"# hand-edited bridge\n"` | `dir --from=<dir> --apply` | exit 0; stderr lacks `failed to reload`; stdout has `"[skip] " + hb.Error() + "\n"`, `"[skip] " + br.Error() + "\n"` and `"  " + session.BridgeKeptNotice + "\n"` |
| 2, same | same | `dir --from=<dir> --apply --json` | exit 0; stdout is one JSON object with `bridge_script` = `"kept"`; stderr has `"[skip] " + br.Error()` |
| 3 `TestConductorMigrateDir_SkipOnlyDryRunKeepsForceNote` | `from/alpha/meta.json` and `to/alpha/meta.json` = `{"name":"alpha","profile":"default"}`; no units | `to --from=<from>` | exit 0; stdout has `"  Note: some destinations already exist; re-run with --apply --force to merge them."` and lacks `would be REFUSED` |

- [ ] **Step 10: Run them; expect failure.** GOTEST `-run 'TestConductorMigrateDir_' ./cmd/agent-deck/`. Expected:
  - `ExternalBridgeAgentRefuses` fails on the dry-run: the blocker is listed, but the REFUSED note is missing, because the note is driven only by skip and reject actions.
  - `NoOpApplyReportsKeptBridgeAndExternalAgents/read-only` fails on its first run, because stderr reports both agents as reload failures: `Warning: failed to reload heartbeat daemon for alpha: <hb.Label>: managed externally (read-only)` and `Warning: failed to reload bridge daemon: <br.Label>: managed externally (read-only)`. Past that check it would also find no `[skip]` lines, no kept notice and no JSON `bridge_script`. `/dangling` fails on the heartbeat's Warning the same way; its bridge gets no line at all, because `bridgeDaemonInstalled`'s `os.Stat` follows the dangling link and the reload is skipped.
  - `SkipOnlyDryRunKeepsForceNote` passes: it pins today's note, which Step 11 must keep.

- [ ] **Step 11: Implement the CLI changes** in `cmd/agent-deck/conductor_cmd.go` (`errors` is already imported).
  - `handleConductorMigrateDir`: before the reconcile, `skipOut := os.Stdout`, or `os.Stderr` under `--json` so stdout stays one JSON document. At the heartbeat reload and the bridge reload, declare `var ext *session.ExternalServiceError`; when `errors.As(derr, &ext)`, `fmt.Fprintln(skipOut, "[skip] "+ext.Error())` replaces the Warning, the name is not added to `reloadedHeartbeats`, and `bridgeReloaded` stays false. Add `"bridge_script": res.BridgeScript,` after `"bridge_reinstalled"`.
  - `bridgeDaemonInstalled` (conductor_cmd.go:1850-1862, used only by this reload): `os.Lstat` replaces both `os.Stat` calls, so a bridge plist or unit that is a symlink, dangling included, reaches `InstallBridgeDaemon`, which reports it as `[skip] <label>: managed externally (symlink)`.
  - `printMigrateDirSummary`: after the `if res.BridgeReinstalled { … }` block that prints `[ok] bridge.py reinstalled at new base`, outside it (a kept bridge leaves `BridgeReinstalled` false), add `if res.BridgeNotice != "" { fmt.Println("  " + res.BridgeNotice) }`. Hoist `skipped` above `if len(res.Actions) == 0` (drop it from the inner `var`), delete `anySkipped` and `anyReject`, and replace the note block (`:2083-2090`): when `res.DryRun && len(res.Blockers) > 0`, print a blank line, then today's `--apply --force` note (`:2086`) if `len(res.Blockers) == skipped` (every blocker is a skip-exists one), else today's REFUSED note (`:2088`).

- [ ] **Step 12: Run both packages; expect a pass.** GOTEST `-run 'TestMigrateConductorDir_|TestConductorMigrateDir_' ./internal/session/ ./cmd/agent-deck/`. Expected: `ok` for both packages.

- [ ] **Step 13: Commit.** Run GOFMT on, then `git add`, the four files `internal/session/conductor_migrate_dir.go`, `internal/session/conductor_migrate_dir_test.go`, `cmd/agent-deck/conductor_cmd.go`, `cmd/agent-deck/conductor_migrate_dir_test.go`. COMMIT with the subject `fix(conductor): refuse a migrate-dir that would strand an external agent` and the body: "When the bridge agent, or the heartbeat agent of a conductor being moved, is managed externally, its plist or unit hard-codes the current paths. migrate-dir now reports this as a blocker in its dry-run and refuses it under --apply. The bridge reinstall at the new base goes through the stamp, so a bridge agent-deck wrote stays upgradable and a hand-edited one is kept and reported. The daemon reload prints [skip] for an external agent, a dangling symlinked plist or unit included."

### Task 12: The Discord Q&A hook in the embedded bridge

**Files:**
- Create: `conductor/tests/test_discord_qna_hook.py`
- Modify: `internal/session/conductor_bridge.py:2619-2622` (module-level hook after `send_discord_output`, before the Telegram banner) and `:3932-3942` (`on_interaction` before `on_message`'s `@bot.event` at :3934; the reply route after the bot check at :3940-3941). These are base-commit numbers; Step 3 shifts the second range, so Step 7 finds its places by code.
- Modify: `internal/session/conductor_test.go:2648-2650` (new test after `TestBridgeTemplate_DiscordImageUploadSupport`, which ends at :2648)
- Test: `conductor/tests/test_discord_qna_hook.py`, `conductor/tests/test_python_compat.py`, `internal/session/conductor_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks (12 is independent). Existing: `CONDUCTOR_DIR` (conductor_bridge.py:140), `log` (:216, logger `"conductor-bridge"`); inside `create_discord_bot`, the `is_authorized` closure (:3692) and `bot.target_channel_id` (:3676); Go `conductorBridgePy` (conductor_bridge_embed.go:17).
- Produces (contract, verbatim): `QNA_ROUTER_TIMEOUT = 90`; `QNA_CUSTOM_ID_RE = re.compile(r"qna:([a-f0-9]{12}):([0-9]{1,2})")`; `def qna_router_path() -> Path`; `def route_question(event: dict, timeout: float = QNA_ROUTER_TIMEOUT) -> dict`; `async def qna_handle_interaction(interaction, target_channel_id: int, is_authorized) -> bool`; `async def qna_handle_reply(message, target_channel_id: int, is_authorized) -> bool`; `@bot.event async def on_interaction(interaction)` in `create_discord_bot`; Go `TestBridgeTemplate_DiscordQnaHook`. Task 14's real-router round trip calls `route_question`.

PYTEST in this task always takes `conductor/tests/test_discord_qna_hook.py conductor/tests/test_python_compat.py`. Run the bridge tests no other way: its `AGENT_DECK_CONDUCTOR_DIR` keeps the bridge's import-time FileHandler (conductor_bridge.py:204-216) off the live `bridge.log`. test_python_compat.py gives 5 passed, 1 skipped (its 3.8-only test skips on nixpkgs python3).

- [ ] **Step 1: Write the failing test** `conductor/tests/test_discord_qna_hook.py`. No discord.py and no skipif: the `qdir` fixture swaps in a fake `discord`. Imports: `asyncio`, `inspect`, `json`, `subprocess`, `from types import SimpleNamespace`, `from unittest import mock`, `pytest`, `bridge` (conftest registers it). The constants come first, because the helpers' default arguments read them:
```python
COMPONENT, NO_MENTIONS = object(), object()
QID, CHANNEL, USER = "0123456789ab", 222, 333
ANSWER = "Answered Q:0123456789ab."
HANDLED = 'print(json.dumps({"handled": True, "response": %r}))' % ANSWER
UNHANDLED = 'print(json.dumps({"handled": False}))'
NO_RESPONSE = 'print(json.dumps({"handled": True}))'
NULL_RESPONSE = 'print(json.dumps({"handled": True, "response": None}))'
RECORDER = 'pathlib.Path(sys.argv[0]).with_name("event.json").write_text(sys.stdin.read()); ' + UNHANDLED
UNCONFIRMED = "Answer outcome unconfirmed. Check the session locally; do not retry automatically."
UNAVAILABLE = "Reply routing unavailable; nothing forwarded. Please check the session locally."
```
  - Fixture `qdir(tmp_path, monkeypatch)`: `monkeypatch.setattr(bridge, "discord", SimpleNamespace(InteractionType=SimpleNamespace(component=COMPONENT), AllowedMentions=SimpleNamespace(none=lambda: NO_MENTIONS)), raising=False)`, `monkeypatch.setattr(bridge, "CONDUCTOR_DIR", tmp_path)`, mkdir `tmp_path / "director"`; returns `tmp_path`.
  - `_router(qdir, body)` writes `"import json, pathlib, sys, time\n" + body + "\n"` to `qdir / "director" / "discord_qna.py"`. `_authorized(user_id)` returns `user_id == USER`.
  - `_interaction(custom_id=f"qna:{QID}:2", type_=COMPONENT, channel_id=CHANNEL, user_id=USER, message_id=444)` returns `SimpleNamespace(type=type_, data={"custom_id": custom_id}, channel_id=channel_id, id=555, user=SimpleNamespace(id=user_id), message=None if message_id is None else SimpleNamespace(id=message_id), response=SimpleNamespace(send_message=mock.AsyncMock(), defer=mock.AsyncMock()), edit_original_response=mock.AsyncMock())`.
  - `_message(reference_id=666, channel_id=CHANNEL, author_id=USER, content="<@999> 2")` returns `SimpleNamespace(id=777, content=content, author=SimpleNamespace(id=author_id, bot=False), channel=SimpleNamespace(id=channel_id), reference=None if reference_id is None else SimpleNamespace(message_id=reference_id), reply=mock.AsyncMock())`.
  - `_button(i)` = `asyncio.run(bridge.qna_handle_interaction(i, CHANNEL, _authorized))`; `_reply(m)` = `asyncio.run(bridge.qna_handle_reply(m, CHANNEL, _authorized))`.

  The 35 cases (router bodies are the constants above or the literal shown; every `route_question` call passes `{"reference": "1"}`; every test but `test_custom_id_pattern` takes `qdir`). Tests read each new name as `bridge.<name>` at call time, never `from bridge import`, so the file collects on today's code:

| Test | Rows | Assertions |
|---|---|---|
| `test_route_question_returns_router_result` | HANDLED, then UNHANDLED | `qna_router_path() == qdir / "director" / "discord_qna.py"`; `route_question({"reference": "1"})` returns `{"handled": True, "response": ANSWER}`, then `{"handled": False}` |
| `test_route_question_nonzero_exit_raises` | `sys.exit(3)` | `pytest.raises(RuntimeError, match="Question router failed")` |
| `test_route_question_rejects_invalid_result(body)` | `print("not json")`; `print(json.dumps(["handled"]))`; `print(json.dumps({"response": "x"}))`; `print(json.dumps({"handled": "yes"}))` | `pytest.raises(ValueError)` (bad JSON, non-dict, no bool `handled`) |
| `test_route_question_timeout` | `time.sleep(60)` | `QNA_ROUTER_TIMEOUT == 90`; `inspect.signature(route_question).parameters["timeout"].default == 90`; `route_question({"reference": "1"}, timeout=1)` raises `subprocess.TimeoutExpired` |
| `test_custom_id_pattern(custom_id, choice)` | `f"qna:{QID}:7"`→`"7"`; `f"qna:{QID}:12"`→`"12"`; `"qna:0123456789AB:1"`, `f"qna:{QID}:123"`, `f"qna:{QID}:1:x"`→`None` | `(m[2] if m else None) == choice` for `m = QNA_CUSTOM_ID_RE.fullmatch(custom_id)` |
| `test_button_ignores_other_interactions(kwargs)` | `type_="application_command"`; `custom_id="ad-status:1"` | `_button(i) is False`; `send_message`, `defer`, `edit_original_response` all `assert_not_awaited()` |
| `test_button_refusals_are_ephemeral(kwargs, text)` | `channel_id=999` or `user_id=999` → `"Not authorized to answer this question."`; `custom_id=f"qna:{QID}:123"` or `message_id=None` → `"Invalid question button. No input sent."` | `True`; `send_message.assert_awaited_once_with(text, ephemeral=True)`; `defer.assert_not_awaited()` |
| `test_button_routes_and_edits_response(body, shown)` | HANDLED → ANSWER; UNHANDLED → `"Question record unavailable. No input sent."`; `sys.exit(3)`, NO_RESPONSE, NULL_RESPONSE, `None` (no router file written) → UNCONFIRMED | `True`; `defer.assert_awaited_once_with(ephemeral=True, thinking=True)`; `edit_original_response.assert_awaited_once_with(content=shown, allowed_mentions=NO_MENTIONS)` |
| `test_reply_falls_through_silently(kwargs, router)`, also takes `caplog` | HANDLED router with `reference_id=None`, `channel_id=999` or `author_id=999`; default message with no router file | `_reply(m) is False`; `reply.assert_not_awaited()`; `[r for r in caplog.records if r.name == "conductor-bridge"] == []` |
| `test_reply_routes(body, consumed, replies)` | HANDLED → `True`, `[mock.call(ANSWER, mention_author=False, allowed_mentions=NO_MENTIONS)]`; UNHANDLED → `False`, `[]`; `sys.exit(3)`, `print("not json")`, NO_RESPONSE, NULL_RESPONSE → `True`, `[mock.call(UNAVAILABLE, mention_author=False)]` | `_reply(m) is consumed`; `m.reply.await_args_list == replies` |
| `test_events_match_design_shapes` | RECORDER; `_button(_interaction())`, then `_reply(_message())` | `json.loads((qdir / "director" / "event.json").read_text())` equals `{"reference": "444", "message": "555", "author": "333", "channel": "222", "text": "2", "custom_id": f"qna:{QID}:2"}`, then `{"reference": "666", "message": "777", "author": "333", "channel": "222", "text": "<@999> 2"}` |

- [ ] **Step 2: Run it, expect failure.** PYTEST → `35 failed, 5 passed, 1 skipped`; each new test fails with `AttributeError: module 'bridge' has no attribute '<name>'` (`qna_router_path`, `route_question`, `QNA_ROUTER_TIMEOUT`, `QNA_CUSTOM_ID_RE`, `qna_handle_interaction` or `qna_handle_reply`).

- [ ] **Step 3: Implement the module-level hook** in `internal/session/conductor_bridge.py` after `send_discord_output` (ends :2619), under the module-level "Discord bot setup" banner (:2547-2549), with the Interfaces names and values:
  - A comment block states the router contract and that agent-deck does not ship the router: the bridge runs `<CONDUCTOR_DIR>/director/discord_qna.py` as `[sys.executable, path]` with one JSON event on stdin; every event value is a string; a button event has keys reference, message, author, channel, text, custom_id, a reply event the same without custom_id; the router prints `{"handled": bool, "response": str}` to stdout and a handled result must carry a str `response`; a non-zero exit is a failure; with no router file the reply route does nothing and a button fails closed.
  - `qna_router_path()` returns exactly `CONDUCTOR_DIR / "director" / "discord_qna.py"`, evaluated per call; no module-level path constant (Step 5's Go test pins the expression).
  - `route_question`: port hera-bridge.py:2466-2472 with `str(qna_router_path())` and `timeout=timeout`; after parsing, `raise ValueError("Question router returned an invalid result")` unless the result is a dict and `isinstance(result.get("handled"), bool)`. Nothing more (the contract's validation).
  - `qna_handle_interaction`: port hera-bridge.py:2474-2499, same check order and texts, `target_channel_id` for `bot.target_channel_id`, `QNA_CUSTOM_ID_RE.fullmatch(custom_id)`. False at the two early returns (not `discord.InteractionType.component`; custom id not a str starting with `qna:`), True once anything is sent. No router-file check (design 5.3).
  - `qna_handle_reply`: port hera-bridge.py:2519-2537. Return False, sending and logging nothing, at the first that holds: `message.channel.id != target_channel_id`; `not is_authorized(message.author.id)`; `message.reference` or its `message_id` unset; `not qna_router_path().is_file()`. False for an unhandled result; True after either reply.
  - Both route with `loop = asyncio.get_running_loop()`, `routed = await loop.run_in_executor(None, route_question, event)` (the bridge's idiom, :1355-1356; Python 3.8 has no `asyncio.to_thread`).
  - Handlers validate a str response inside the try: in the `try` that wraps the `run_in_executor` call, read `routed["handled"]`; only when true read `routed["response"]` and raise `ValueError` unless it is a `str`. Branch on `handled`, never on the response value: `{"handled": false}` has no `response` key (hera-discord_qna.py:361) and must not raise, and a handled reply never falls through to a conductor. Failures take hera's `except Exception` branch with `log.exception("Discord question button routing failed")` / `log.exception("Discord question reply routing failed")`.
  - The handled `await message.reply(response, mention_author=False, allowed_mentions=discord.AllowedMentions.none())` stays after the try (a send failure after the router acted must not claim "nothing forwarded"); the failure reply passes only `mention_author=False`, as hera does.
  - From `discord` use only `discord.InteractionType.component` and `discord.AllowedMentions.none()` (all the fake provides). `asyncio.to_thread` appears nowhere in the file, comments included; the strings `async def on_interaction(` and `await qna_handle_reply(` appear only with Step 7's wiring (Step 6 expects them missing).

- [ ] **Step 4: Run it, expect pass.** PYTEST → `40 passed, 1 skipped`.

- [ ] **Step 5: Write the failing wiring tests.** `create_discord_bot` reads `HAS_DISCORD`, `discord` and `app_commands` as module globals at call time (:3651, :3669-3675, :3776, :3863), so a stub builds the real bot offline (as internal/session/test_discord_typing_best_effort.py:30-80 does through `sys.modules`). Append to the pytest file:
```python
def _keep(**_): return lambda fn: fn  # bot.tree.command(...) and app_commands.describe(...) as no-op decorators
class _Client:  # enough of discord.Client; user None makes message_mentions_bot() False
    user = None
    def __init__(self, intents=None): pass
    def event(self, coro): setattr(self, coro.__name__, coro); return coro
@pytest.mark.parametrize("mode", ["all", "mentions", "mentions_all_channels"])
def test_discord_bot_routes_questions_in_every_listen_mode(qdir, monkeypatch, mode):
    vars(bridge.discord).update(Intents=SimpleNamespace(default=SimpleNamespace), Client=_Client, Object=SimpleNamespace)
    monkeypatch.setattr(bridge, "app_commands", SimpleNamespace(CommandTree=lambda bot: SimpleNamespace(command=_keep),
                                                                describe=_keep), raising=False)
    monkeypatch.setattr(bridge, "HAS_DISCORD", True)
    _router(qdir, HANDLED)
    bot, _ = bridge.create_discord_bot({"discord": {"configured": True, "bot_token": "x", "guild_id": 1,
                                                    "channel_id": CHANNEL, "user_id": USER, "listen_mode": mode}})
    interaction, message = _interaction(), _message()
    asyncio.run(bot.on_interaction(interaction))
    asyncio.run(bot.on_message(message))
    interaction.edit_original_response.assert_awaited_once_with(content=ANSWER, allowed_mentions=NO_MENTIONS)
    message.reply.assert_awaited_once_with(ANSWER, mention_author=False, allowed_mentions=NO_MENTIONS)
```
  Then add `TestBridgeTemplate_DiscordQnaHook` to `internal/session/conductor_test.go` after `TestBridgeTemplate_DiscordImageUploadSupport`, doc comment `// TestBridgeTemplate_DiscordQnaHook pins the Discord Q&A hook (design 5.3).`, shaped like its neighbours (`template := conductorBridgePy`, a patterns loop): each of `"def route_question("`, `` `r"qna:([a-f0-9]{12}):([0-9]{1,2})"` ``, `` `CONDUCTOR_DIR / "director" / "discord_qna.py"` ``, `"async def on_interaction("`, `"await qna_handle_reply("` missing → `t.Errorf("template should contain Discord Q&A hook pattern: %q", pattern)`; `strings.Contains(template, "asyncio.to_thread")` → `t.Error("template must not use asyncio.to_thread (Python 3.9+); use loop.run_in_executor")`. CI runs this Go test; python-compat.yml runs only test_python_compat.py and test_stage_report.py, and .github/ stays untouched.

- [ ] **Step 6: Run them, expect failure.** PYTEST → `3 failed, 40 passed, 1 skipped`: each `test_discord_bot_routes_questions_in_every_listen_mode[...]` fails with `AttributeError: 'ConductorBot' object has no attribute 'on_interaction'` (the stub builds the unwired bot). GOTEST `-run '^TestBridgeTemplate_DiscordQnaHook$' ./internal/session/` → `--- FAIL: TestBridgeTemplate_DiscordQnaHook` with exactly two errors, `template should contain Discord Q&A hook pattern: "async def on_interaction("` and `template should contain Discord Q&A hook pattern: "await qna_handle_reply("`; Step 3 supplied the other three patterns and the to_thread check passes.

- [ ] **Step 7: Wire the hook into `create_discord_bot`**, finding the places by code:
  - Between `dc_cmd_help` and `on_message`'s `@bot.event`: `@bot.event` / `async def on_interaction(interaction):`, whose only statement is `await qna_handle_interaction(interaction, bot.target_channel_id, is_authorized)`.
  - In `on_message`, right after `if message.author.bot:` / `return` and before the `# Channel scope, then mention` comment: a comment saying question replies go to the Q&A router first so they work in every listen mode, `qna_handle_reply` checks the channel and authorization itself, and a failed route is never forwarded to a conductor; then `if await qna_handle_reply(message, bot.target_channel_id, is_authorized):` / `return`. Nothing else in `on_message` changes.

- [ ] **Step 8: Run them, expect pass.** GOTEST `-run '^(TestBridgeTemplate_|TestEmbeddedBridge)' ./internal/session/` → `ok  github.com/asheshgoplani/agent-deck/internal/session` (the listen-mode and reply-filter patterns still match; `TestEmbeddedBridge*` parse and import the new bytes). PYTEST → `43 passed, 1 skipped`. Not run here: internal/session/test_discord_typing_best_effort.py, which Task 14 runs in its own PYTEST process; its message has `reference=None`, so `qna_handle_reply` returns False at the reference check. Its result does not change: under nixpkgs python3 (3.14.7) it already gives `1 failed, 1 passed` on the base commit (test_typing_failure_does_not_abort_reply_path: the typing task is cancelled before its first step, so the WARNING it expects is never logged).

- [ ] **Step 9: Commit.** GOFMT `internal/session/conductor_test.go`; `git status --short --ignored conductor internal/session` lists only the three files; `git add conductor/tests/test_discord_qna_hook.py internal/session/conductor_bridge.py internal/session/conductor_test.go`; COMMIT with subject `feat(bridge): route Discord question buttons and replies to the Q&A router` and body: "Port hera's Discord Q&A hook (design 5.3) into the embedded bridge. route_question runs <conductor dir>/director/discord_qna.py with the event as JSON on stdin; on_interaction answers qna: buttons, and on_message hands replies to the router before the channel and mention filters. Routing uses loop.run_in_executor (the bridge's Python 3.8 floor); the handlers validate the router's result inside the try and fail closed. agent-deck does not ship the router; without it the reply route is inert."

### Task 13: The checkout scripts conductor/setup.sh and conductor/teardown.sh

Design 5.4, as corrected by map 08, the critic and revision T13. bridge.py follows 5.1's table: an identical bridge gets the stamp only, never a `.bridge.py.previous`. teardown.sh removes bridge.py only under RemoveBridgeScript's rule (equal to the checkout's canonical file, or matching its stamp), never removes a symlinked bridge.py and never touches `bridge.py.backup`. The external rule covers the bridge plist, the gh-watcher plist and both gh-watcher systemd units. A plist agent-deck owns keeps today's steps in today's order (write, `launchctl list` probe and unload, load), so a failed write stops the script under `set -e` before any launchctl call. Everything else stays as today: the `~/.agent-deck` paths; pip, even with an external bridge plist (setup.sh uses the system python3, never a nix venv); `cp HEARTBEAT_RULES.md` (a sibling Task 14 records) and the per-profile CLAUDE.md writes; no `uname` guard on Step 5; `/usr/bin/trash` for the gh-watcher plist; teardown.sh never touching the systemd units; bash 3.2 (no associative arrays, no `${v,,}`); `set -euo pipefail`. Accepted deviation from 5.4's "same skip line" (critic 22): the kept-bridge line names `conductor/setup.sh` as its remedy, because the Go command writes a different directory.

Every new function: its first line is exactly `name() {` at column 0 and it ends with a lone `}`; no body line is exactly `}` (extractShellFunc stops there); it is defined above the top-level code that calls it; teardown.sh defines no `fail`, so its functions never call it. A caller declares `local reason` first, then writes `if reason="$(service_file_external "$p")"; then` (`local reason="$(...)"` hides the exit status). Skip lines are plain `echo`, not ok/info/warn, so their text matches the Go side's exactly.

**Files:** (line numbers are base `0953876e`'s; the new functions shift them, so Steps 19-20 name each block by its text)
- Modify: `conductor/setup.sh`: functions after `register_session`'s closing `}` (:74); blocks :91-97, :225-241, :272-283, :287-296
- Modify: `conductor/teardown.sh`: `SCRIPT_DIR` after :12; functions after `warn()` (:23); blocks :29-36, :39, :90-91
- Modify: `conductor/gh-watcher/README.md:105`; `conductor/README.md` (the bridge paragraph Task 4 rewrote)
- Create: `conductor/setup_scripts_test.go` (package `conductor`, no TestMain)
- Test: `conductor/setup_scripts_test.go`

**Interfaces:**
- Consumes: no earlier task's code. `extractShellFunc(t *testing.T, script, name string) string` (conductor/setup_register_test.go:29); `testutil.SkipIfRoot(t testing.TB, why string)` (internal/testutil/root.go:14). Task 4's on-disk format, so Go can still upgrade a bridge setup.sh wrote: `.bridge.py.sha256` holds 64 lower-case hex digits + `"\n"`, the displaced copy is `.bridge.py.previous`, and readBridgeStamp trusts only a regular-file stamp. Pinned lines: `[skip] <label>: managed externally (<read-only|symlink>)`, `[skip] bridge.py is a symlink; left unchanged.`, teardown's `[skip] bridge.py has local changes; kept`.
- Produces (shell): in both scripts with identical text, `sha256_of FILE` and `service_file_external PATH` (external: prints `symlink` or `read-only`, exit 0; otherwise no output, exit 1). setup.sh: `install_bridge_script`, `render_bridge_plist`, `render_ghw_plist`, `install_launch_agent LABEL TEMPLATE_CMD PLIST`, `install_ghw_systemd_units UNIT_DIR`. teardown.sh: `remove_bridge_script`, `remove_launch_agent LABEL PLIST`.

- [ ] **Step 1: Write the test scaffolding** in `conductor/setup_scripts_test.go`. Imports: `crypto/sha256`, `fmt`, `os`, `os/exec`, `path/filepath`, `runtime`, `strings`, `testing`, `github.com/asheshgoplani/agent-deck/internal/testutil`. Every helper calls t.Fatal on any error.
  - `const canonicalBridge, olderBridge, customBridge = "canonical v2\n", "shipped v1\n", "my Q&A hook\n"`; `func stampOf(s string) string { return fmt.Sprintf("%x\n", sha256.Sum256([]byte(s))) }`.
  - `func testBash() string`: `/bin/bash` (3.2.57 on hera) when `runtime.GOOS == "darwin"` and it exists, else `bash`. Every bash this file runs, `-n` included, is testBash().
  - `mustWrite(t *testing.T, path, content string, perm os.FileMode)`: MkdirAll the parent, write, then `os.Chmod` (umask would turn 0464 into 0444). `mustSymlink(t *testing.T, target, link string)`: MkdirAll the parent, then link. `readOr(path string) string`: contents through symlinks, "" when absent.
  - `newScriptFixture(t *testing.T) *scriptFixture`, fields `root, home, conductorDir, log string; env []string`: root = t.TempDir(); home = root/home, with `Library/LaunchAgents`; conductorDir = home/.agent-deck/conductor, created (setup.sh:89 makes it before install_bridge_script, whose mktemp needs it); root/checkout/conductor (the SCRIPT_DIR) holds a copy of `./com.agentdeck.conductor-bridge.plist`; root/checkout/internal/session/conductor_bridge.py holds canonicalBridge; log = root/launchctl.log; root/bin/launchctl (0755) is the recorder below; env = `os.Environ()` + `HOME=<home>`, `CONDUCTOR_DIR=<conductorDir>`, `SCRIPT_DIR=<root>/checkout/conductor`, `FAKE_LAUNCHCTL_LOG=<log>`, `PATH=<root>/bin:$PATH`. Each is set explicitly (G3's PATH-fake form): nothing isolates this package, and a real `launchctl unload` of a plist rendered from the template acts on hera's live `com.agentdeck.conductor-bridge`.
    ```sh
    #!/bin/sh
    echo "launchctl $*" >> "$FAKE_LAUNCHCTL_LOG"
    if [ "$1" = list ]; then printf '%s\n' "${FAKE_LAUNCHCTL_LIST:-}"; fi
    exit 0
    ```
  - `(f *scriptFixture) run(t *testing.T, script string, funcs []string, call string, env ...string) (string, error)`: writes root/drive.sh with, in order, `#!/usr/bin/env bash`, `set -euo pipefail`, `RED=''; GREEN=''; YELLOW=''; CYAN=''; NC=''`, `info() { echo "[info] $*"; }`, `ok() { echo "[ok] $*"; }`, `warn() { echo "[warn] $*"; }`, `fail() { echo "[fail] $*"; exit 1; }`, the guard `[[ "$(command -v launchctl)" == "<root>/bin/launchctl" ]] || fail "launchctl is not the fake"` (a fake without its exec bit would fall through to /bin/launchctl), `extractShellFunc(t, script, name)` for each of funcs (the function under test and the helpers it calls), then call. Runs `exec.Command(testBash(), drive)` with a copy of f.env plus env; returns CombinedOutput and the error.
  - `(f *scriptFixture) wantCalls(t *testing.T, plist string, want []string)`: reads the log with readOr (no log = no calls) and Errorf's unless its lines equal want with `<plist>` replaced by plist. A nil want means no launchctl call at all, not even `list`.
- [ ] **Step 2: Write the failing helper tests.**
  - `TestShellServiceFileExternal` (the `Shell` prefix keeps it apart from Task 6's Go test of the same rule): one fixture holding f.root/target.plist ("x\n", 0644). Each subtest makes P = t.TempDir()/agent.plist (a file row writes "x\n" at the mode shown), runs `service_file_external "$P"` from setup.sh (funcs: service_file_external; env `P=<P>`) and Fatalf's unless `strings.TrimSpace(out) == want` and the exit is 0 exactly when want != "":

    | subtest | P | want |
    |---|---|---|
    | symlink | symlink to f.root/target.plist | `symlink` |
    | dangling symlink | symlink to f.root/gone.plist | `symlink` |
    | no write bit | 0444 file (no root skip, G4) | `read-only` |
    | owner bits deny write | 0464 file, after `testutil.SkipIfRoot(t, "root passes access(2) W_OK whatever the owner bits say")` | `read-only` |
    | writable | 0644 file | "" (exit 1) |
    | missing | nothing | "" (exit 1) |
  - `TestScriptHelpers_SameInBothScripts`: for `sha256_of` and `service_file_external`, Errorf unless `extractShellFunc(t, "setup.sh", name) == extractShellFunc(t, "teardown.sh", name)`.
- [ ] **Step 3: Run them; expect failure.** GOTEST `-run 'TestShellServiceFileExternal|TestScriptHelpers_SameInBothScripts' ./conductor/` → FAIL with extractShellFunc's `setup.sh does not define service_file_external()` and `setup.sh does not define sha256_of()`.
- [ ] **Step 4: Implement `sha256_of FILE` and `service_file_external PATH`** after setup.sh's `register_session`, then paste the identical text after teardown.sh's `warn()`.
  - `sha256_of`: install.sh:49-55's idiom: `sha256sum "$1"` when `command -v sha256sum >/dev/null 2>&1` finds it, else `shasum -a 256 "$1"`, piped to `awk '{print $1}'` (lower-case hex + newline, Go's stamp format).
  - `service_file_external`, first match wins (mode bits before access(2), as Task 6's Go): (1) `[[ -L "$1" ]]` → `echo symlink`, return 0; (2) `[[ -e "$1" ]]` and `[[ -n "$(find "$1" -prune ! -perm -0200 ! -perm -0020 ! -perm -0002 -print)" ]]` (no write bit at all; POSIX find because BSD and GNU `stat` differ; holds for root) → `echo read-only`, return 0; (3) `[[ -e "$1" && ! -w "$1" ]]` → `echo read-only`, return 0; (4) return 1. The 0444 row exercises (2), the 0464 row (3).
- [ ] **Step 5: Run Step 3's command; expect** `ok  	github.com/asheshgoplani/agent-deck/conductor` (the 0464 row is skipped only as root).
- [ ] **Step 6: Write the failing bridge tests.** K = `[skip] bridge.py has local changes; left unchanged. To take the shipped bridge, move it aside and re-run conductor/setup.sh.` "Stamp link": .bridge.py.sha256 is a symlink to f.root/stamp.txt holding the stamp. "Bridge link": bridge.py is a symlink to f.root/elsewhere.py holding the bytes. The link rows pin the symlink rules. Both tests make a fresh fixture per row.
  - `TestInstallBridgeScript`: arrange bridge.py (0644) and the stamp; run `install_bridge_script` from setup.sh (funcs: sha256_of, install_bridge_script). Fatalf unless out contains wantOut and err != nil exactly for `directory`. Every other row: bridge.py, .bridge.py.sha256 and .bridge.py.previous read (readOr) as listed (— = absent); conductorDir holds no other entry (no `.bridge.py.new-*` left); after `installed` and `updated`, bridge.py has mode 0755 (checked only when os.Stat succeeds).

    | row | arrange | wantOut | bridge.py | stamp | .previous |
    |---|---|---|---|---|---|
    | missing | — | `[ok] bridge.py installed` | canonical | stampOf(canonical) | — |
    | identical, no stamp | canonical | `[ok] bridge.py unchanged` | canonical | stampOf(canonical) | — |
    | stamped older | older + stampOf(older) | `[ok] bridge.py updated` | canonical | stampOf(canonical) | older |
    | customized, no stamp | custom | K | custom | — | — |
    | customized, stale stamp | custom + stampOf(older) | K | custom | stampOf(older) | — |
    | stamp link | older + stamp link to stampOf(older) | K | older | stampOf(older) | — |
    | stamp without newline | older + stampOf(older) minus its newline | K | older | stampOf(older) minus its newline | — |
    | symlink | bridge link to custom | `[skip] bridge.py is a symlink; left unchanged.` | custom | — | — |
    | directory | bridge.py is a directory | `[fail] refusing to replace` | — | — | — |
  - `TestRemoveBridgeScript`: arrange bridge.py, the stamp, `.bridge.py.previous` = "prev\n" and `bridge.py.backup` = "backup\n"; run `remove_bridge_script` from teardown.sh (funcs: sha256_of, remove_bridge_script); Fatalf on a non-zero exit. Removed: bridge.py, the stamp and .previous are absent and the backup intact. Kept: all four read as arranged, and a bridge link still Lstat's as a symlink. A kept row prints its line; a removed row prints no `[skip]` line.

    | row | bridge.py | stamp | result | printed |
    |---|---|---|---|---|
    | customized | custom | — | kept | `[skip] bridge.py has local changes; kept` |
    | customized, stale stamp | custom | stampOf(older) | kept | the same |
    | stamp link | older | stamp link to stampOf(older) | kept | the same |
    | stamp without newline | older | stampOf(older) minus its newline | kept | the same |
    | stamped | older | stampOf(older) | removed | — |
    | identical | canonical | — | removed | — |
    | symlink | bridge link to canonical | stampOf(canonical) | kept | `[skip] bridge.py is a symlink; kept` |
- [ ] **Step 7: Run them; expect failure.** GOTEST `-run 'TestInstallBridgeScript|TestRemoveBridgeScript' ./conductor/` → FAIL with `setup.sh does not define install_bridge_script()` and `teardown.sh does not define remove_bridge_script()`.
- [ ] **Step 8: Implement `install_bridge_script`** in setup.sh after `service_file_external`. Locals (teardown uses the same three): `src="${SCRIPT_DIR}/../internal/session/conductor_bridge.py"`, `dest="${CONDUCTOR_DIR}/bridge.py"`, `stamp="${CONDUCTOR_DIR}/.bridge.py.sha256"`. In both scripts "the stamp matches" is `[[ -f "$stamp" && ! -L "$stamp" ]] && sha256_of "$dest" | cmp -s - "$stamp"`: byte-exact, trailing newline included, as Go's `readBridgeStamp(dir) == bridgeStamp(content)` (`$(...)` would strip the newline). In order:
  1. `[[ -L "$dest" ]]` → `echo "[skip] bridge.py is a symlink; left unchanged."`, return 0.
  2. `[[ -e "$dest" && ! -f "$dest" ]]` → `fail "refusing to replace ${dest}: not a regular file"`.
  3. Outcome: dest exists and `cmp -s "$src" "$dest"` → `unchanged`; else dest exists and the stamp does not match → `echo` K, return 0; else dest exists → `updated`; else `installed`.
  4. installed or updated: `tmp="$(mktemp "${CONDUCTOR_DIR}/.bridge.py.new-XXXXXX")"`, `cp "$src" "$tmp"`, `chmod 755 "$tmp"` (mktemp creates 0600); for updated only, `ln -f "$dest" "${CONDUCTOR_DIR}/.bridge.py.previous"` (the hard link keeps the displaced inode, so an editor holding it open writes into the kept copy); then `mv -f "$tmp" "$dest"` (bridge.py is never missing; shell has no atomic exchange, so unlike Go there is no recheck of the displaced copy).
  5. `rm -f "$stamp"; sha256_of "$dest" > "$stamp"` (removing first replaces a symlinked stamp instead of writing through it; a crash before this heals next run as `unchanged`), then `ok "bridge.py ${outcome}"`.
- [ ] **Step 9: Implement `remove_bridge_script`** in teardown.sh after `service_file_external`, with the same locals, then: `if [[ -L "$dest" ]]; then echo "[skip] bridge.py is a symlink; kept"; return 0; fi`; `[[ ! -e "$dest" ]]` → return 0; neither `cmp -s "$src" "$dest"` nor the stamp matches → `echo "[skip] bridge.py has local changes; kept"`, return 0 (without the checkout, cmp fails and only the stamp rule applies); else `rm -f "$dest" "$stamp" "${CONDUCTOR_DIR}/.bridge.py.previous"`. Never `bridge.py.backup`.
- [ ] **Step 10: Run Step 7's command; expect** `ok  	github.com/asheshgoplani/agent-deck/conductor`.
- [ ] **Step 11: Write the failing service-file tests**, each with a fresh fixture per row. L = `com.agentdeck.conductor-bridge`; P = home/Library/LaunchAgents/L.plist, passed as env `P=<P>`; a plist row writes "old\n" at P, or at f.root/store.plist (0644) with P a symlink to it.
  - `TestInstallLaunchAgent`: run `install_launch_agent "L" render_bridge_plist "$P"` from setup.sh (funcs: service_file_external, render_bridge_plist, install_launch_agent; env also `PLIST_NAME=L`, `PYTHON3_PATH=/opt/test/python3`, `FAKE_LAUNCHCTL_LIST=L`). Fatalf unless exit 0 and out contains wantOut; then wantCalls. An external row's file still reads "old\n"; the owned row's plist contains `<string>` + conductorDir + `/bridge.py</string>`.

    | row | P | wantOut | calls |
    |---|---|---|---|
    | read-only plist | 0444 | `[skip] L: managed externally (read-only)` | nil |
    | symlinked plist | symlink to store.plist | `[skip] L: managed externally (symlink)` | nil |
    | owned plist | 0644 | `[ok] L loaded` | `launchctl list`, `launchctl unload <plist>`, `launchctl load <plist>` |
  - `TestInstallGhwSystemdUnits`: subtests `timer symlinked=true` and `=false`; U = home/.config/systemd/user; true makes U/agent-deck-gh-watcher.timer a symlink to f.root/store.timer ("nix\n", 0644). Run `mkdir -p "$U"; install_ghw_systemd_units "$U"` from setup.sh (funcs: service_file_external, install_ghw_systemd_units; env `U=<U>`, `GHW_SRC=<abs ./gh-watcher>`, `GHW_HOME=<conductorDir>/gh-watcher`, `GHW_INTERVAL=45`); exit 0. true: out has `[skip] agent-deck-gh-watcher.timer: managed externally (symlink)` and not `[ok] gh-watcher systemd units written to <U>`; the .service is absent and the timer still reads "nix\n". false: the [ok] line and no skip; the service contains `ExecStart=/bin/bash <GHW_HOME>/scripts/tick.sh`, the timer `OnUnitActiveSec=45`.
  - `TestShellRemoveLaunchAgent`: run `remove_launch_agent "L" "$P"` from teardown.sh (funcs: service_file_external, remove_launch_agent). Fatalf unless exit 0 and out contains wantOut; then wantCalls, and P still reads "old\n" exactly when kept.

    | row | P | wantOut | calls | kept |
    |---|---|---|---|---|
    | read-only plist | 0444 | `[skip] L: managed externally (read-only)` | nil | yes |
    | owned plist | 0644 | `[ok] L stopped and plist removed` | `launchctl unload <plist>` | no |
    | missing | — | `[info] No L plist found (already removed)` | nil | no |
- [ ] **Step 12: Run them; expect failure.** GOTEST `-run 'TestInstallLaunchAgent|TestInstallGhwSystemdUnits|TestShellRemoveLaunchAgent' ./conductor/` → FAIL with `setup.sh does not define render_bridge_plist()`, `setup.sh does not define install_ghw_systemd_units()` and `teardown.sh does not define remove_launch_agent()`.
- [ ] **Step 13: Implement `render_bridge_plist`, `render_ghw_plist` and `install_launch_agent LABEL TEMPLATE_CMD PLIST`** in setup.sh after `install_bridge_script`. The renderers are base setup.sh:226-231's and :272-278's `sed` without the `> ...` redirect, still reading the globals PYTHON3_PATH, PLIST_NAME, GHW_HOME, GHW_INTERVAL, GHW_SRC, GHW_PLIST_NAME, CONDUCTOR_DIR, HOME and SCRIPT_DIR. install_launch_agent: external → `echo "[skip] ${label}: managed externally (${reason})"`, return 0, no launchctl call; else today's steps in today's order: `"${template_cmd}" > "${plist}"`; `ok "Plist installed at ${plist}"`; if `launchctl list | grep -q "${label}" 2>/dev/null`, then `info "Reloading existing daemon..."` and `launchctl unload "${plist}" 2>/dev/null || true`; `launchctl load "${plist}"`; `ok "${label} loaded"`.
- [ ] **Step 14: Implement `install_ghw_systemd_units UNIT_DIR`** in setup.sh after `install_launch_agent`: for each of `agent-deck-gh-watcher.service` and `agent-deck-gh-watcher.timer` that is external, print `[skip] <unit>: managed externally (<reason>)`; if either was, return 0 with neither written (one unit of ownership, as Task 7's replaceSystemdUnits); else base setup.sh:287-296's loop (with `local unit`) and its two output lines, with `${GHW_UNIT_DIR}` replaced by `$1`.
- [ ] **Step 15: Implement `remove_launch_agent LABEL PLIST`** in teardown.sh after `remove_bridge_script`: external → the same skip line, return 0, nothing unloaded or removed; `[[ ! -f "${plist}" ]]` → `info "No ${label} plist found (already removed)"`; else `info "Stopping ${label}..."`, `launchctl unload "${plist}" 2>/dev/null || true`, `rm -f "${plist}"`, `ok "${label} stopped and plist removed"`.
- [ ] **Step 16: Run Step 12's command; expect** `ok  	github.com/asheshgoplani/agent-deck/conductor`.
- [ ] **Step 17: Write the failing wiring test** `TestConductorScripts_CallTheGuardedSteps`. Per script: `testBash() -n <script>` succeeds (else Fatalf with its output); map each trimmed line to its first line number; then t.Errorf, so every miss is reported: a call below that is not a trimmed line of its own (`%s does not run %q as a line of its own`); a call whose `<first field>() {`, when present, comes after it (`%s runs %q at line %d but defines it at line %d`); gone text that remains (`%s still has the unguarded %q`). Last, `extractShellFunc(t, "setup.sh", "render_ghw_plist")` (a TEMPLATE_CMD argument, not a first field).

    | script | calls | gone |
    |---|---|---|
    | setup.sh | `install_bridge_script`; `install_launch_agent "${PLIST_NAME}" render_bridge_plist "${PLIST_PATH}"`; `install_launch_agent "${GHW_PLIST_NAME}" render_ghw_plist "${GHW_PLIST_PATH}"`; `install_ghw_systemd_units "${GHW_UNIT_DIR}"` | `> "${PLIST_PATH}"`; `> "${GHW_PLIST_PATH}"`; `> "${GHW_UNIT_DIR}/${unit}"`; `cp "${SCRIPT_DIR}/../internal/session/conductor_bridge.py" "${CONDUCTOR_DIR}/bridge.py"` |
    | teardown.sh | `SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"`; `remove_launch_agent "${PLIST_NAME}" "${PLIST_PATH}"`; `remove_bridge_script`; `if GHW_REASON="$(service_file_external "${GHW_PLIST_PATH}")"; then` | `rm -f "${PLIST_PATH}"`; `rm -f "${CONDUCTOR_DIR}/bridge.py"` |
- [ ] **Step 18: Run it; expect failure.** GOTEST `-run 'TestConductorScripts_CallTheGuardedSteps' ./conductor/` → FAIL although `-n` passes on both scripts: `setup.sh does not run "install_bridge_script" as a line of its own`, the same for teardown.sh's `SCRIPT_DIR=` line, and `setup.sh still has the unguarded "> \"${PLIST_PATH}\""`.
- [ ] **Step 19: Wire setup.sh** (blocks named by their text):
  - `# Copy bridge script to base conductor directory.` … `ok "bridge.py installed"` (base :91-97) → a two-line comment (the canonical source is internal/session/conductor_bridge.py, installed only when bridge.py is missing, identical, or still matches `.bridge.py.sha256`) and `install_bridge_script`.
  - `# Generate plist from template with variable substitution` … `ok "Daemon loaded and running"` (base :225-241) → `install_launch_agent "${PLIST_NAME}" render_bridge_plist "${PLIST_PATH}"`. Base :219-223 (`info "Installing launchd daemon..."` … `PYTHON3_PATH=...`) stays.
  - Darwin branch: `sed \` … `ok "gh-watcher launchd unit loaded ..."` (base :272-283) → `install_launch_agent "${GHW_PLIST_NAME}" render_ghw_plist "${GHW_PLIST_PATH}"`.
  - `else` branch: `GHW_UNIT_DIR=...` and its `mkdir -p` stay; `for unit in ...` … `echo "  Enable with: ..."` (base :287-296) → `install_ghw_systemd_units "${GHW_UNIT_DIR}"`.
- [ ] **Step 20: Wire teardown.sh:**
  - After `GHW_PLIST_PATH=` (base :12) add `SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"` (setup.sh:15's line; remove_bridge_script reads it).
  - `if [[ -f "${PLIST_PATH}" ]]; then` through its `fi` (base :29-36) → `remove_launch_agent "${PLIST_NAME}" "${PLIST_PATH}"`.
  - `if [[ -f "${GHW_PLIST_PATH}" ]]; then` (base :39) → the three lines below. The unload, `/usr/bin/trash` and ok lines and the closing `fi` stay, so the block is still silent when no plist exists. It stays inline, not remove_launch_agent: `/usr/bin/trash` is an absolute path no PATH fake intercepts (a test would use the real Trash), so only Step 17 checks it, by text.
    ```bash
    if GHW_REASON="$(service_file_external "${GHW_PLIST_PATH}")"; then
        echo "[skip] com.agentdeck.gh-watcher: managed externally (${GHW_REASON})"
    elif [[ -f "${GHW_PLIST_PATH}" ]]; then
    ```
  - `# Remove bridge.py and bridge.log from base conductor dir` and `rm -f "${CONDUCTOR_DIR}/bridge.py" "${CONDUCTOR_DIR}/bridge.log"` (base :90-91) → a one-line comment (bridge.py goes only when it is the checkout's canonical file or matches its stamp), `remove_bridge_script`, `rm -f "${CONDUCTOR_DIR}/bridge.log"`.
- [ ] **Step 21: Update the docs.** conductor/gh-watcher/README.md:105 becomes: "`conductor/teardown.sh` unloads and removes the launchd unit. Both scripts leave a plist or unit file that is a symlink or read-only alone (`[skip] <label>: managed externally`): something else, such as home-manager, owns it." In conductor/README.md, after Task 4's sentence ending "to take the shipped bridge.", add: "`conductor/setup.sh` applies the same rule to the copy it writes under `~/.agent-deck/conductor` (its kept line names `conductor/setup.sh` as the remedy), and `conductor/teardown.sh` removes only a bridge that rule would replace."
- [ ] **Step 22: Run the package.** GOTEST `./conductor/` → `ok  	github.com/asheshgoplani/agent-deck/conductor`, including the four `TestRegisterSession_*` tests (register_session's text is unchanged; the new functions follow its closing `}`).
- [ ] **Step 23: Commit.** GOFMT `conductor/setup_scripts_test.go`; `git add conductor/setup.sh conductor/teardown.sh conductor/setup_scripts_test.go conductor/gh-watcher/README.md conductor/README.md`; COMMIT with subject `fix(conductor): keep custom bridges and external service files in setup.sh and teardown.sh` and a body stating: setup.sh installs the canonical bridge only when bridge.py is missing, identical (stamp only) or still matching `.bridge.py.sha256`, writes the stamp in the Go installer's format and keeps a replaced bridge as `.bridge.py.previous`; teardown.sh removes bridge.py only under the same rule, never a symlinked one, never `bridge.py.backup`; both scripts leave a symlinked or read-only bridge or gh-watcher plist alone, and setup.sh the gh-watcher systemd units too; conductor/README.md says so.

### Task 14: Final verification and the sandbox "after" run

**Files:**
- Create: nothing in the tree. Under `$P` (`~/Products/agent-deck-conductor-setup-fix`): `bin/agent-deck`, `acceptance/evidence-after/`, `acceptance/evidence-roundtrip/`, `logs/final-*`, `pytest-tmpdir/`, and the scratch worktrees `base-0953876e/` (Steps 3-14) and `patched-head/` (Step 7 only).
- Modify: nothing in the tree, and nothing in the main checkout: obr runs from this worktree with `--no-auto-flush`, so it writes only the obr database, never the main checkout's tracked `docs/PLAN.org`.
- Test: no new tests. The task runs the series' Go suites (darwin, and Linux in Docker), the nix gate, the nix patch's tests, the bridge's pytest files and the sandbox.

**Interfaces:**
- Consumes (needs every earlier task): a clean `work/conductor-setup-fix` whose HEAD carries the committed plan document and Tasks 2-13.
  - Task 1, in `$P/acceptance/`: `evidence-before/`, and `run-sandbox.sh BINARY OUTDIR` (OUTDIR must be new). It writes `binary.txt`, `harness.log`, `setup.{stdout,stderr,exit}`, `teardown.{stdout,stderr,exit}`, `launchctl.log` (lines `answered Aqua | managername`, `refused | <args>`, `pass rc=<n> | <args>`), `snap{0,1,2}.txt`, `{setup,teardown}.diff`, `{setup,teardown}-changed-paths.txt` (`./<path>` under the sandbox HOME), `live-status.txt` (`<before|after> list <label> rc=<n>`, `<before|after> print gui/<uid>/<label> rc=<n>`, the live files' hashes) and `plist-modes.txt` (six unprefixed `<mode> <abs path>` lines from `stat -f '%Lp %N'`: the bridge plist, then the notifier plist, after snap0, snap1 and snap2 in turn; a missing plist leaves stat's error line), and prints `setup exit <n>, teardown exit <n>; evidence in <OUTDIR>`. A live agent counts as loaded when `/bin/launchctl list <label>` exits 0.
  - Task 1: `router-roundtrip.sh TREE OUTDIR` (OUTDIR must be new), which writes the router's answer line to `OUTDIR/roundtrip.out` and exits 0 only when `route_question` answered `{"handled": false}` and the sandbox director/ manifest is unchanged. Both scripts refuse when `$P/sandbox` exists and remove it on exit; both are invoked with `bash`.
  - Task 4: `.bridge.py.sha256`, `.bridge.py.previous`. Task 12: `conductor/tests/test_discord_qna_hook.py`, `def route_question(event: dict, timeout: float = QNA_ROUTER_TIMEOUT) -> dict`. The pinned lines `BridgeKeptNotice`, `"[skip] " + ExternalServiceError.Error()`, `[skip] bridge daemon managed externally; Python deps left to its manager`.
- Produces: nothing a later task uses. Output: the evidence, `agent-deck-z8i` closed with it, and the report to John. The discovered work was filed while planning, from `agent-deck-z8i`: `agent-deck-388` (update timer plist), `agent-deck-9gc` (setup.sh `HEARTBEAT_RULES.md`), `agent-deck-j5d` (setup.sh per-profile `CLAUDE.md`), `agent-deck-rke` (the typing test of Step 8) and `agent-deck-cm3` (docs channel lists).

How to run the steps:
- Run from the worktree root `/Users/johnw/src/fork/agent-deck/.worktrees/work-conductor-setup-fix`. Each code block, and each step's prose commands taken in order, runs in one fresh shell that first sets `P=$HOME/Products/agent-deck-conductor-setup-fix; A=$P/acceptance/evidence-after; B=$P/acceptance/evidence-before`.
- Run anything longer than ten minutes in the background and wait for it to exit.
- When an output differs from the expected one, stop, unless the step gives a rule for judging it (Steps 5, 6, 8, 9 and 12 do). Fix the cause test-first, COMMIT the fix, then restart at Step 1.

- [ ] **Step 1: Check that the series is complete and stayed inside its limits**
```bash
git status --short --ignored; git log --oneline 0953876e..HEAD
git diff --stat 0953876e HEAD -- cmd/agent-deck/main.go cmd/agent-deck/version_capabilities.go cmd/agent-deck/version_capabilities_test.go internal/session/remote_channel.go internal/session/remote_channel_capabilities_test.go internal/session/remote_poll_transport_test.go go.mod go.sum Makefile CHANGELOG.md .github cmd/agent-deck/testdata/goldens docs/PLAN.org
lc() { git grep -h -o -E 'func TestRuntimeLifecycle_[A-Za-z0-9_]+' "$1" -- '*.go' | sort; }; diff <(lc 0953876e) <(lc HEAD); git grep -n -w -E 'writeVersionJSON|errRemoteAgentUnsupported' HEAD -- '*.go'
```
Expected: only `git log` prints. Its last line is `6333ffde docs(conductor): design safe conductor setup on an existing installation`; above it are exactly the plan document's commit, one commit per Task 2-13 and any fix commit made under this task. The identifier checks read only `*.go`, because the committed plan quotes the forbidden names.

- [ ] **Step 2: gofmt every changed Go file, then vet for darwin and for linux**
Run GOFMT `$(git diff --name-only --diff-filter=d 0953876e -- '*.go')`, `nix shell nixpkgs#go_1_26 -c go vet ./...` and `nix shell nixpkgs#go_1_26 -c env GOOS=linux GOARCH=amd64 go vet ./...`. Expected: each exits 0 and prints nothing. The linux vet type-checks the `_linux.go` files (`internal/session/generated_exchange_linux.go`), which a darwin vet and lint skip.

- [ ] **Step 3: Lint the branch and base 0953876e with golangci-lint v2.14.0; no new findings**
```bash
test -d $P/base-0953876e || git worktree add --detach $P/base-0953876e 0953876e; test "$(git -C $P/base-0953876e rev-parse HEAD)" = "$(git rev-parse 0953876e)" && echo base-ok
nix shell nixpkgs#go_1_26 -c make lint > $P/logs/final-lint-branch.log 2>&1
(cd $P/base-0953876e && nix shell nixpkgs#go_1_26 -c make lint) > $P/logs/final-lint-base.log 2>&1
grep -E '^(0 issues\.|[0-9]+ issues:)$' $P/logs/final-lint-branch.log $P/logs/final-lint-base.log
norm() { grep -E '^[^[:space:]]+\.go:[0-9]+(:[0-9]+)?: ' "$1" | sed -E 's/^([^:]+):[0-9]+(:[0-9]+)?: /\1: /' | sort; }; comm -23 <(norm $P/logs/final-lint-branch.log) <(norm $P/logs/final-lint-base.log)
```
Expected: `base-ok`; one summary line per log, `0 issues.` or `<N> issues:` (then per-linter counts and `make: *** [Makefile:270: lint] Error 1`; the base may have findings, so exit status decides nothing); `comm` prints nothing. File output has no colour, and `norm` drops line and column, so a finding that only moved is not new.

- [ ] **Step 4: Run the four packages in full (over an hour under hera's load)**
Run GOTEST-FULL `./internal/session/ ./internal/update/ ./cmd/agent-deck/ ./conductor/ > $P/logs/final-tests-branch.log 2>&1; echo "exit=$?"`, then `grep -E '^(ok|FAIL|--- FAIL|panic:)' $P/logs/final-tests-branch.log`. Expected: `exit=0` and exactly four `ok` lines: `github.com/asheshgoplani/agent-deck/internal/session`, `.../internal/update`, `.../cmd/agent-deck`, `.../conductor`. Any `FAIL` or `panic:` line goes to Step 5.

- [ ] **Step 5: Judge each failure against the base (only after a failing Step 4)**
- Rerun each `--- FAIL: <Test>` of `<pkg>` (top-level name) alone with GOTEST-FULL `-run '^<Test>$' <pkg>`, here and from `$P/base-0953876e`, logging to `$P/logs/final-rerun-<Test>-{branch,base}.log`. A package that failed with no `--- FAIL` line (`panic: test timed out`, or a test that called `os.Exit`) is rerun whole on both trees.
- Regression (stop, fix, restart): it fails alone here and passes on the base, or a test this series added fails. Pre-existing: it fails on both. Load only: it passes alone here. List the last two kinds, with their logs, for the report.

- [ ] **Step 6: Run the nix checkPhase gate in its safe form (do not edit the Makefile)**
```bash
env -i HOME="$HOME" TMPDIR=/tmp CGO_ENABLED=1 PATH="$HOME/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.26.10.darwin-arm64/bin:/etc/profiles/per-user/johnw/bin:/usr/bin:/bin:/usr/sbin:/sbin" make GOTOOLCHAIN=local test-runtime-lifecycle > $P/logs/final-gate.log 2>&1; echo "exit=$?"; tail -n 1 $P/logs/final-gate.log
```
Expected: `exit=0`, then `runtime lifecycle tests passed` (Makefile:226). The recipe (Makefile:148-226) builds its own sandbox (HOME, XDG, a failing fake tmux, cold GOCACHE, GOPROXY=off); its `mktemp -d` (:150) is the plan's second /tmp exception, removed by its trap (:151). It forces GOTOOLCHAIN=local (:158, :181), hence go1.26.10 first on PATH. `env -i` keeps TMUX, AGENTDECK_* and AGENT_DECK_CONDUCTOR_DIR out; HOME is passed only for `go env GOMODCACHE`. On failure: rerun once if it is `TestRuntimeLifecycle_PackagedCrashHelper` (session) with `build packaged runtime crash helper: signal: killed` or `TestRuntimeLifecycle_PackagedHelper` (statedb) with `build packaged runtime helper: signal: killed` (a 2-minute cold build timed out under load). Any other failure, or the same one twice: run the command in `$P/base-0953876e` and judge as in Step 5.

- [ ] **Step 7: The nix overlay patch applies, and its five tests pass on the patched HEAD**
```bash
PATCH=$HOME/src/nix/overlays/ai/agent-deck-legacy-remote-capability.patch; git apply --check -p1 $PATCH; echo "exit=$?"
git worktree add --detach $P/patched-head HEAD && git -C $P/patched-head apply -p1 $PATCH
```
Then run `(cd $P/patched-head && GOTEST -run '^TestRuntimeLifecycle_(VersionJSONAdvertisesRemoteAgent|RemoteChannel|LegacyRemote)' ./cmd/agent-deck/ ./internal/session/) > $P/logs/final-patched.log 2>&1; echo "patched=$?"`, `grep -E '^(ok|FAIL)' $P/logs/final-patched.log` and `git worktree remove --force $P/patched-head`. Expected: `exit=0`, `patched=0`, two `ok` lines (`.../cmd/agent-deck`, `.../internal/session`), neither ending in `[no tests to run]`. The run catches what `--check` cannot: a change to `runAgentDeck`, `agentDeckTestEnv`, `channelsCLIBinary` or the SSHRunner/RemoteChannel internals the patch's tests use.

- [ ] **Step 8: Run the bridge's pytest files**
1. PYTEST `conductor/tests/test_discord_qna_hook.py conductor/tests/test_python_compat.py`. Expected: `43 passed, 1 skipped` (Task 12's final count), no `failed`, no `error`. The skip is `test_bridge_imports_on_python_38` (conductor/tests/test_python_compat.py:108-113, Python 3.8 only). A second skip means some of Task 12's tests did not run without discord.py, which design 7 requires.
2. `mkdir -p $P/pytest-tmpdir`, then a separate PYTEST `internal/session/test_discord_typing_best_effort.py` with `TMPDIR=$P/pytest-tmpdir` added to its `env` assignments: at import it puts a stub `discord` in `sys.modules` and sets `AGENT_DECK_CONDUCTOR_DIR` from `tempfile.mkdtemp()`. It drives the real `on_message` that Task 12 changed. Expected: `test_typing_task_does_not_leak_after_delivery` passes (the reply still reaches the channel). Under nixpkgs python3 3.14.7 the summary is `1 failed, 1 passed`: `test_typing_failure_does_not_abort_reply_path` fails with `AssertionError: no logs of level WARNING or higher triggered on conductor-bridge`, as on 0953876e's files and on a Task 12-wired bridge (checked 2026-11-01; filed as `agent-deck-rke`). For any failure, run the same command from `$P/base-0953876e` and judge as in Step 5; a failure on both is pre-existing and goes in the report.

- [ ] **Step 9: Run CI's Linux checks and the systemd branches in Docker**
First `docker info --format '{{.ServerVersion}}'` prints a version (29.9.2 on 2026-11-01). If it does not answer, ask John to start Docker: nothing else runs the Linux and systemd branches. If he declines, the report lists them as unexecuted.
```bash
make verify-docker VERIFY_ARGS="--stages gofmt,build,vet,lint" > $P/logs/final-verify-docker.log 2>&1; grep -E '^(STAGE|VERIFY_RC)' $P/logs/final-verify-docker.log
make verify-docker VERIFY_ARGS="--stages test --pkgs './internal/update/... ./conductor/...'" > $P/logs/final-verify-docker-test.log 2>&1; grep -E '^(STAGE|VERIFY_RC)' $P/logs/final-verify-docker-test.log
T=$PWD; RUN='^(TestReplaceGeneratedFile|TestWriteGeneratedFileOrMigrate|TestBridgeScript_|TestIsMutatingServiceVerb|TestServiceManager|TestInstallServiceManagerFake_|TestBridgeDaemonHint|TestUninstallHeartbeatDaemon_|TestInstallTransitionNotifierLaunchd_|TestLaunchdHeartbeat|TestSystemdInstall|TestExternalServiceError_|TestServiceFileExternal|TestReplaceLaunchAgent_|TestRemoveLaunchAgent|TestLaunchAgentWiring_|TestReplaceSystemdUnits_|TestRemoveSystemdUnits_|TestSystemdDaemons_|TestDaemonExternalProbes_|TestIsManagedHeartbeatScript|TestInstallHeartbeatScriptOutcome|TestMigrateConductorHeartbeatScripts_|TestRenderConductorHeartbeatScript_|TestConductorHeartbeatScript_|TestMigrateConductorDir_|TestConductorSetup|TestInstallPythonDeps|TestConductorTeardown|TestRuntimeLifecycle_Conductor|TestSessionDeletePathsUseSharedCleanup|TestConductorMigrateDir_)'
docker run --rm --init -v "$T":/src:ro -e GOFLAGS=-buildvcs=false -e AGENTDECK_SKIP_UPDATE_CHECK=1 -e GOTOOLCHAIN=local -e PERF_BUDGET_MULTIPLIER=2.0 -e "RUN=$RUN" golang:1.26.10-bookworm bash -c 'set -euo pipefail
apt-get update -qq >/dev/null && apt-get install -y -qq tmux zoxide >/dev/null && useradd --create-home --uid 10001 verify
mkdir -p /work && tar -C /src --exclude=./.git --exclude=./.worktrees --exclude=./.claude/worktrees -cf - . | tar -C /work -xf - && chown -R verify:verify /work && cd /work
exec setpriv --reuid=10001 --regid=10001 --clear-groups env HOME=/home/verify bash scripts/test.sh -race -count=1 -timeout 60m -run "$RUN" ./internal/session/ ./cmd/agent-deck/' > $P/logs/final-linux-tests.log 2>&1; echo "exit=$?"
grep -E '^(ok|FAIL|--- FAIL|panic:)' $P/logs/final-linux-tests.log
```
Expected: `STAGE gofmt OK`, `STAGE build OK`, `STAGE vet OK`, `STAGE lint OK`, `VERIFY_RC=0`; `STAGE test OK`, `VERIFY_RC=0` (scripts/verify-docker.sh:97-123); `exit=0` and two `ok` lines, `.../internal/session` and `.../cmd/agent-deck`, neither `[no tests to run]`. `RUN` is the union of Tasks 2 and 6-11's own verification regexes, plus Tasks 3 and 4's tests, so the Linux exchange (`renameat2`, generated_exchange_linux.go) runs under the new writer. The `docker run` repeats verify-docker's test container (scripts/verify-docker.sh:74-87, :94-95, :118-119: read-only mount, tar copy without VCS data, tmux and zoxide, uid 10001, `--init`) with scripts/test.sh and `-timeout 60m`, because verify-docker's fixed 20m cannot hold internal/session. Judging: if the only failure is `STAGE lint FAIL`, run `make -C $P/base-0953876e verify-docker VERIFY_ARGS="--stages lint" > $P/logs/final-verify-docker-base.log 2>&1`; Step 3's `comm -23` over `norm` of the two logs (redefine `norm`) prints nothing. A failing test is judged as in Step 5, rerunning the `docker run` with `RUN='^<Test>$'` and its package, from `T=$PWD` and from `T=$P/base-0953876e`.

- [ ] **Step 10: Build the fixed binary from the committed HEAD**
`nix shell nixpkgs#go_1_26 -c go build -o $P/bin/agent-deck ./cmd/agent-deck`, then `nix shell nixpkgs#go_1_26 -c go version -m $P/bin/agent-deck | grep -E 'vcs\.(revision|modified)'`. Expected: `vcs.revision=` plus `git rev-parse HEAD`'s output, and `vcs.modified=false`. With no `-ldflags`, `main.Version` keeps its source default `1.16.34` (cmd/agent-deck/main.go:48), the installed binary's version that the before run used.

- [ ] **Step 11: Run the sandbox "after" run**
```bash
test ! -e $A || mv $A $A.$(date +%s); bash $P/acceptance/run-sandbox.sh $P/bin/agent-deck $A; echo "exit=$?"
test ! -e $P/sandbox && echo sandbox-gone; diff <(ls -A $B) <(ls -A $A)
```
Expected: `setup exit 0, teardown exit 0; evidence in /Users/johnw/Products/agent-deck-conductor-setup-fix/acceptance/evidence-after`, `exit=0`, `sandbox-gone`, and no `diff` output (the same evidence files as Task 1's run). The `mv` matters only on a restart.

- [ ] **Step 12: Check design 7's "after" results**
`cat $A/setup.stdout $A/setup.stderr` contains each of these lines (`grep -Fx`):
```
[skip] bridge.py has local changes; left unchanged. To take the shipped bridge, move it aside and re-run 'agent-deck conductor setup <name>'.
[skip] bridge daemon managed externally; Python deps left to its manager
[skip] com.agentdeck.conductor-bridge: managed externally (read-only)
[skip] com.agentdeck.transition-notifier: managed externally (read-only)
```
and none of today's lines (cmd/agent-deck/conductor_cmd.go:719-779, :1605, :1636): `grep -E 'Python dependencies (install failed|could NOT be installed)|Bridge daemon NOT installed|Warning: failed to install|\[ok\] bridge\.py|\[ok\] Bridge daemon loaded|\[ok\] Transition notifier daemon installed'` over it prints nothing. `$A/setup.exit` and `$A/teardown.exit` both hold `0`. Then:
```bash
al() { grep -v -E '^(answered Aqua \| managername$|pass rc=[0-9]+ \| (list|print)( |$))' "$1/launchctl.log"; }; al $A; echo before-run:; al $B
grep -E '^\./(\.local/share/agent-deck/conductor/(bridge\.py|\.bridge\.py\.(sha256|previous))|Library/LaunchAgents/)' $A/setup-changed-paths.txt $A/teardown-changed-paths.txt
grep -c -E '^444 /' $A/plist-modes.txt; grep -c -v -E '^444 /' $A/plist-modes.txt
grep -c -E '^(before|after) list com\.agentdeck\.(conductor-bridge|transition-notifier) rc=0$' $A/live-status.txt
diff <(sed -n 's/^before //p' $A/live-status.txt) <(sed -n 's/^after //p' $A/live-status.txt)
chg() { grep -v '/conductor/steward-probe/' "$1" | sed -E 's/\.previous-[0-9]+$/.previous-*/' | LC_ALL=C sort -u; }; LC_ALL=C comm -23 <(chg $A/setup-changed-paths.txt) <(chg $B/setup-changed-paths.txt)
grep -v '/conductor/steward-probe/' $A/teardown-changed-paths.txt | grep -v '^\./\.local/share/agent-deck/'; grep '/conductor/' $A/teardown-changed-paths.txt | grep -v '/conductor/steward-probe/'
```
Expected, in order:
1. `before-run:`, then the before run's `refused | unload /Users/johnw/Products/agent-deck-conductor-setup-fix/sandbox/home/Library/LaunchAgents/com.agentdeck.transition-notifier.plist`: the after run made no refused or mutating launchctl call (the shim refuses every verb but list, print and managername), and the same filter prints the before run's refused call.
2. Nothing: bridge.py and both plists byte-identical, and no `.bridge.py.sha256` or `.bridge.py.previous` (a kept bridge is never stamped).
3. `6`, then `0`: both plists were 0444 at every snapshot.
4. `4`, then no `diff` output: `/bin/launchctl list` found both live agents before and after, and their `print` results and the live bridge.py's and plists' hashes did not change.
5. `comm` prints nothing: setup changed only what the unfixed binary also changed (`.previous-<digits>` is the os.CreateTemp suffix of `replaceGeneratedFile`'s temp file). A printed path whose name carries a per-run value (a session id, a date), or that only this run's copy of the live directory holds, is listed with its cause; any other is a regression.
6. Nothing: apart from removing `conductor/steward-probe/`, teardown changed nothing outside `./.local/share/agent-deck/` and nothing else under `conductor/` (design 7 allows only agent-deck's own state database and logs).
The report lists every remaining entry of both after-run lists with its cause (for example, the shared `CLAUDE.md` migration).

- [ ] **Step 13: Run the real-router round trip (design 7)**
```bash
R=$P/acceptance/evidence-roundtrip; test ! -e $R || mv $R $R.$(date +%s); bash $P/acceptance/router-roundtrip.sh "$PWD" $R > $P/logs/final-roundtrip.log 2>&1; echo "exit=$?"
cat $R/roundtrip.out; test ! -e $P/sandbox && echo sandbox-gone
```
Expected: `exit=0`, `{"handled": false}` (the router's answer), and `sandbox-gone`. The script's own sandbox has an empty Discord token, so the router's `request()` would raise before any network call, and its button event's reference `"1"` matches no stored Discord message id, so the router writes nothing. Never route a handled event: the router would then edit Discord posts.

- [ ] **Step 14: Remove the base worktree (there is nothing to commit)**
`test ! -e $P/sandbox && echo sandbox-gone; git worktree remove $P/base-0953876e; git status --short --ignored`. Expected: `sandbox-gone`, then nothing. The sandbox's copy of John's config is gone (design 7), and the tree gained no `__pycache__`, `.pytest_cache` or build output. This task commits and pushes nothing.

- [ ] **Step 15: Record the evidence and close agent-deck-z8i**
Write one paragraph to `$P/logs/final-obr-comment.txt`: HEAD's short sha; Steps 1-13 as observed (gofmt and vet clean on darwin and linux; lint adds nothing over 0953876e; the four `ok` packages; `runtime lifecycle tests passed`; the patch applies and its tests pass; pytest `43 passed, 1 skipped`, and the typing test's result beside the base's; verify-docker and the Linux run OK, or declined by John; in the sandbox no refused launchctl call, bridge.py and both 0444 plists byte-identical, both live agents loaded; the round trip answered `{"handled": false}` and left director/ unchanged); the failures listed in Steps 5, 6, 8 and 9; the evidence paths `~/Products/agent-deck-conductor-setup-fix/{acceptance/evidence-before,acceptance/evidence-after,acceptance/evidence-roundtrip,logs}`. Then, from the worktree root:
```bash
M=/Users/johnw/src/fork/agent-deck/docs/PLAN.org; m0=$(/usr/bin/stat -f %m $M)
obr --no-auto-flush comments add agent-deck-z8i --file $P/logs/final-obr-comment.txt && obr --no-auto-flush close agent-deck-z8i --reason "Fixed on work/conductor-setup-fix at <short HEAD>; push, nix pin bump and hera activation remain with John and the nix and pi sessions" && obr --no-auto-flush show agent-deck-z8i
test "$(/usr/bin/stat -f %m $M)" = "$m0" && echo plan-org-untouched
```
Expected: `obr show` reports the issue closed, then `plan-org-untouched`.

- [ ] **Step 16: Report to John**
In the session: each step's result and its log; the failures from Steps 5, 6, 8 and 9 (and any Linux run John declined); the after-run lists from Step 12 with their causes; the five discovered-work ids (Produces); the plan's departures from the design (its "Where this plan departs from the design" section); what stays unproven (design 7): the embedded hook has not handled a live Discord question, and the steward plan's day-zero check is the live proof. State that nothing was pushed and that the next steps are others': pushing to `jwiegley/agent-deck` `main` is John's; the pin bump in `~/src/nix` `sources/ai.json` is the nix session's, after that push; hera's activation is the pi session's.
