package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTestEntryIsolatesRuntimeState(t *testing.T) {
	for _, failure := range []bool{false, true} {
		name := "success"
		if failure {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			cache := filepath.Join(root, "cache")
			for _, dir := range []string{bin, cache} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			fakeGo := `#!/usr/bin/env bash
set -eu
if [[ $1 == env ]]; then
    printf '%s\n' "$HOME/cache"
    exit
fi
[[ $1 == test && $GOENV == off ]]
tmp_root=$(cd /tmp && pwd -P)
[[ $TMPDIR == "$tmp_root"/adt.?????? ]] || { echo "TMPDIR $TMPDIR is not the short sandbox root" >&2; exit 11; }
[[ $HOME == "$TMPDIR/home" && -d $HOME && $TMUX_TMPDIR == "$TMPDIR/tmux" ]]
[[ $HOME == "$(cd "$HOME" && pwd -P)" ]]
[[ $USERPROFILE == "$HOME" && $TMP == "$TMPDIR" && $TEMP == "$TMPDIR" ]]
[[ -d $TMPDIR && -d $TMUX_TMPDIR && $GOCACHE == "$GOMODCACHE" ]]
[[ ${SHELL##*/} == bash && -x $SHELL ]] || { echo "shell $SHELL" >&2; exit 8; }
for name in OPENAI_API_KEY CLAUDE_CONFIG_DIR CODEX_HOME AGENTDECK_INSTANCE_ID SSH_AUTH_SOCK TMUX TMUX_PANE XDG_CONFIG_HOME XDG_DATA_HOME XDG_CACHE_HOME XDG_STATE_HOME XDG_RUNTIME_DIR; do
    [[ ! ${!name+x} ]] || { echo "inherited $name" >&2; exit 9; }
done
[[ ${PERF_BUDGET_MULTIPLIER-} == 2.5 && ${VISUALCHECK_BINARY-} == /synthetic/agent-deck && ${VISUALCHECK_KEEP_SANDBOX-} == 1 ]] || {
    echo "test knobs dropped: PERF_BUDGET_MULTIPLIER=${PERF_BUDGET_MULTIPLIER-} VISUALCHECK_BINARY=${VISUALCHECK_BINARY-} VISUALCHECK_KEEP_SANDBOX=${VISUALCHECK_KEEP_SANDBOX-}" >&2
    exit 10
}
mkdir "$TMPDIR/visualcheck-artifacts"
printf 'sheet\n' > "$TMPDIR/visualcheck-artifacts/contact-sheet.html"
printf '%s\n' "$HOME" "$*" > "$GOCACHE/receipt"
[[ $* != 'test fail' ]] || exit 7
`
			if err := os.WriteFile(filepath.Join(bin, "go"), []byte(fakeGo), 0700); err != nil {
				t.Fatal(err)
			}
			args := []string{"test.sh"}
			if failure {
				args = append(args, "fail")
			}
			cmd := exec.Command("bash", args...)
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "HOME="+root, "TMPDIR="+root, "SHELL=/bin/zsh",
				"PERF_BUDGET_MULTIPLIER=2.5", "VISUALCHECK_BINARY=/synthetic/agent-deck", "VISUALCHECK_KEEP_SANDBOX=1")
			for _, key := range []string{"OPENAI_API_KEY", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "AGENTDECK_INSTANCE_ID", "SSH_AUTH_SOCK", "TMUX", "TMUX_PANE", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR"} {
				cmd.Env = append(cmd.Env, key+"=synthetic-test-value")
			}
			output, err := cmd.CombinedOutput()
			if failure {
				if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 7 {
					t.Fatalf("exit = %v, want 7: %s", err, output)
				}
			} else if err != nil {
				t.Fatalf("test entry: %v: %s", err, output)
			}
			receipt, err := os.ReadFile(filepath.Join(cache, "receipt"))
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(receipt)), "\n")
			if _, err := os.Stat(filepath.Dir(lines[0])); !os.IsNotExist(err) {
				t.Fatalf("sandbox not removed: %v", err)
			}
			if !failure && lines[1] != "test -race -count=1 -timeout 20m ./..." {
				t.Fatalf("default test args = %q", lines[1])
			}
			kept, err := filepath.Glob(filepath.Join(root, "agent-deck-test-artifacts.*", "visualcheck-artifacts", "contact-sheet.html"))
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case failure && (len(kept) != 1 || !strings.Contains(string(output), filepath.Dir(kept[0]))):
				t.Fatalf("failed run did not keep and name the visualcheck artifacts: %v: %s", kept, output)
			case !failure && len(kept) != 0:
				t.Fatalf("passing run kept visualcheck artifacts: %v", kept)
			}
		})
	}
}

// TestTestEntryGivesTheFullSuiteCIsPerfBudgetMultiplier pins the timing
// allowance of a run: with no arguments (lefthook's pre-push, make test) go
// test gets upstream CI's PERF_BUDGET_MULTIPLIER=2.0 unless the caller set a
// value, and a targeted run gets only what the caller set.
func TestTestEntryGivesTheFullSuiteCIsPerfBudgetMultiplier(t *testing.T) {
	const unset = "<unset>"
	targeted := []string{"-count=1", "-run", "^TestX$", "./internal/web"}
	for _, tc := range []struct {
		name   string
		args   []string
		caller string // unset: not in the runner's environment
		want   string
	}{
		{"full suite unset", nil, unset, "2.0"},
		{"full suite empty", nil, "", "2.0"},
		{"full suite caller value", nil, "1.0", "1.0"},
		{"targeted unset", targeted, unset, unset},
		{"targeted caller value", targeted, "2.5", "2.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			cache := filepath.Join(root, "cache")
			for _, dir := range []string{bin, cache} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			fakeGo := `#!/usr/bin/env bash
set -eu
if [[ $1 == env ]]; then
    printf '%s\n' "$HOME/cache"
    exit
fi
printf '%s\n' "${PERF_BUDGET_MULTIPLIER-<unset>}" "$*" > "$GOCACHE/receipt"
`
			if err := os.WriteFile(filepath.Join(bin, "go"), []byte(fakeGo), 0700); err != nil {
				t.Fatal(err)
			}
			// A full run of this suite goes through the runner too, so this
			// process may carry the default itself: start from the case's value.
			var env []string
			for _, kv := range os.Environ() {
				if !strings.HasPrefix(kv, "PERF_BUDGET_MULTIPLIER=") {
					env = append(env, kv)
				}
			}
			env = append(env, "PATH="+bin+":"+os.Getenv("PATH"), "HOME="+root, "TMPDIR="+root)
			if tc.caller != unset {
				env = append(env, "PERF_BUDGET_MULTIPLIER="+tc.caller)
			}
			cmd := exec.Command("bash", append([]string{"test.sh"}, tc.args...)...)
			cmd.Env = env
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("test entry: %v: %s", err, output)
			}
			receipt, err := os.ReadFile(filepath.Join(cache, "receipt"))
			if err != nil {
				t.Fatal(err)
			}
			wantArgs := "test -race -count=1 -timeout 20m ./..."
			if tc.args != nil {
				wantArgs = "test " + strings.Join(tc.args, " ")
			}
			lines := strings.Split(strings.TrimSuffix(string(receipt), "\n"), "\n")
			if len(lines) != 2 || lines[0] != tc.want || lines[1] != wantArgs {
				t.Fatalf("go test got PERF_BUDGET_MULTIPLIER %q and args %q, want %q and %q", lines[0], lines[1:], tc.want, wantArgs)
			}
		})
	}
}
