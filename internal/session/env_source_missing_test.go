package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildSourceCmd_MissingFileDoesNotStopChain: with ignoreMissing (the
// default) a missing env file is skipped, and the commands joined after it
// with && still run. The old `[ -f f ] && source f` form was false for a
// missing file, so the && chain stopped there and the agent never started.
func TestBuildSourceCmd_MissingFileDoesNotStopChain(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.env")
	src := buildSourceCmd(missing, true)

	for _, cmd := range []string{
		src + ` && printf ran`,
		`export A=b && ` + src + ` && ` + src + ` && printf ran`,
	} {
		out, err := runSourceCmdInBash(t, dir, cmd)
		if err != nil || out != "ran" {
			t.Errorf("command after a missing env file did not run (err=%v, out=%q)\ncmd: %s", err, out, cmd)
		}
	}

	// A failure earlier in the chain still stops it: skipping a missing file
	// must not turn into running the rest regardless.
	cmd := `false && ` + src + ` && printf ran`
	if out, err := runSourceCmdInBash(t, dir, cmd); err == nil || strings.Contains(out, "ran") {
		t.Errorf("chain continued after an earlier failure (err=%v, out=%q)\ncmd: %s", err, out, cmd)
	}
}

// TestBuildSourceCmd_FailureModesKept: what must still stop the chain. A
// present file whose source fails stops it in both modes, and with
// ignoreMissing=false a missing file does too.
func TestBuildSourceCmd_FailureModesKept(t *testing.T) {
	dir := t.TempDir()
	failing := filepath.Join(dir, "failing.env")
	if err := os.WriteFile(failing, []byte("export LOC5_SENTINEL=sourced\nreturn 7\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	missing := filepath.Join(dir, "missing.env")

	tests := []struct {
		name          string
		path          string
		ignoreMissing bool
	}{
		{"failing source, ignore missing", failing, true},
		{"failing source, strict", failing, false},
		{"missing file, strict", missing, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := buildSourceCmd(tt.path, tt.ignoreMissing) + ` && printf ran`
			out, err := runSourceCmdInBash(t, dir, cmd)
			if err == nil || strings.Contains(out, "ran") {
				t.Errorf("chain continued (err=%v, out=%q)\ncmd: %s", err, out, cmd)
			}
		})
	}
}

// TestBuildSourceCmd_EdgePaths records two behaviours of the ignoreMissing
// form at its edges. A path that is a directory is not a file to source, so
// it is skipped like a missing one. A file whose name ends in a newline is
// not sourced: command substitution strips the trailing newline, the source
// then names a file that does not exist, and the chain stops.
func TestBuildSourceCmd_EdgePaths(t *testing.T) {
	dir := t.TempDir()

	sub := filepath.Join(dir, "a-directory")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cmd := buildSourceCmd(sub, true) + ` && printf ran`
	if out, err := runSourceCmdInBash(t, dir, cmd); err != nil || out != "ran" {
		t.Errorf("command after a directory path did not run (err=%v, out=%q)\ncmd: %s", err, out, cmd)
	}

	newline := filepath.Join(dir, "newline.env\n")
	if err := os.WriteFile(newline, []byte("export LOC5_SENTINEL=sourced\n"), 0o600); err != nil {
		t.Skipf("file name ending in a newline not supported here: %v", err)
	}
	cmd = buildSourceCmd(newline, true) + ` && printf '%s' "ran:$LOC5_SENTINEL"`
	if out, err := runSourceCmdInBash(t, dir, cmd); err == nil || strings.Contains(out, "ran:") {
		t.Errorf("a file name ending in a newline: want the chain stopped, got err=%v, out=%q\ncmd: %s", err, out, cmd)
	}
}

// TestBuildEnvSourceCommand_MissingShellEnvFileStillLaunches: the whole
// prefix a session is started with. A [shell] env_files entry that does not
// exist must leave the command after the prefix running, and an entry that
// does exist is sourced.
func TestBuildEnvSourceCommand_MissingShellEnvFileStillLaunches(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "present.env")
	if err := os.WriteFile(present, []byte("export LOC5_SENTINEL=sourced\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	cfg := &UserConfig{
		MCPs: make(map[string]MCPDef),
		Shell: ShellSettings{
			EnvFiles: []string{filepath.Join(dir, "missing.env"), present},
		},
	}
	defer resetUserConfigCache(t, cfg)()

	inst := &Instance{Title: "env-missing", Tool: "shell", ProjectPath: dir}
	prefix := inst.buildEnvSourceCommand()
	if !strings.HasSuffix(prefix, " && ") {
		t.Fatalf("buildEnvSourceCommand() = %q, want a prefix ending in ' && '", prefix)
	}

	cmd := prefix + `printf '%s' "ran:$LOC5_SENTINEL"`
	out, err := runSourceCmdInBash(t, dir, cmd)
	if err != nil || out != "ran:sourced" {
		t.Errorf("command after the env prefix did not run with the present file sourced (err=%v, out=%q)\ncmd: %s", err, out, cmd)
	}
}
