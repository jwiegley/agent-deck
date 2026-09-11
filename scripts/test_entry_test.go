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
[[ $HOME == /tmp/agent-deck-tests.*/home && -d $HOME ]]
[[ $USERPROFILE == "$HOME" && $TMP == "$TMPDIR" && $TEMP == "$TMPDIR" ]]
[[ -d $TMPDIR && -d $TMUX_TMPDIR && $GOCACHE == "$GOMODCACHE" ]]
for name in OPENAI_API_KEY CLAUDE_CONFIG_DIR CODEX_HOME AGENTDECK_INSTANCE_ID SSH_AUTH_SOCK TMUX TMUX_PANE XDG_CONFIG_HOME XDG_DATA_HOME XDG_CACHE_HOME XDG_STATE_HOME XDG_RUNTIME_DIR; do
    [[ ! ${!name+x} ]] || { echo "inherited $name" >&2; exit 9; }
done
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
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "HOME="+root)
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
			if !failure && lines[1] != "test -race -count=1 ./..." {
				t.Fatalf("default test args = %q", lines[1])
			}
		})
	}
}
