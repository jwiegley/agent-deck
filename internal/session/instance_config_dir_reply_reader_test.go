package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// A session's last response is read from the Claude config dir THAT SESSION
// resolves to, not from the dir the calling process resolves to.
//
// The conductor bridge runs under launchd without CLAUDE_CONFIG_DIR. Its
// `session output` of a steward whose group has its own config_dir looked for
// the transcript under the process-wide dir, where it never was, and fell
// through to pane parsing or an error, although `session send --wait` had just
// found the same transcript through GetJSONLPath's per-instance chain (#2769).
// getClaudeLastResponse and the rollover scan findLatestClaudeTranscriptOnDisk
// now walk that chain too, keeping the process-wide dir as the fallback.

const instanceConfigDirReplyReaderConfig = `
[claude]
config_dir = "~/.claude-global"

[groups.conductor.claude]
config_dir = "~/.claude-conductor"
`

func instanceConfigDirReplyReaderInstance(t *testing.T, home, sessionID string) *Instance {
	t.Helper()
	project := filepath.Join(home, "conductor", "nix-steward")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	inst := NewInstanceWithGroupAndTool("nix-steward", project, "conductor", "claude")
	inst.ClaudeSessionID = sessionID
	return inst
}

func instanceConfigDirReplyReaderProjectsDir(t *testing.T, configDir, projectPath string) string {
	t.Helper()
	resolved := projectPath
	if r, err := filepath.EvalSymlinks(projectPath); err == nil {
		resolved = r
	}
	dir := filepath.Join(configDir, "projects", ConvertToClaudeDirName(resolved))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir projects dir: %v", err)
	}
	return dir
}

func instanceConfigDirReplyReaderTranscript(sessionID, text string) string {
	return fmt.Sprintf(`{"type":"assistant","sessionId":%q,"timestamp":"2026-11-05T18:00:00Z","message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`+"\n", sessionID, text)
}

func TestGetClaudeLastResponse_ReadsTheInstanceConfigDir(t *testing.T) {
	home := withTempAgentDeckHome(t, instanceConfigDirReplyReaderConfig)
	inst := instanceConfigDirReplyReaderInstance(t, home, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	dir := instanceConfigDirReplyReaderProjectsDir(t, filepath.Join(home, ".claude-conductor"), inst.ProjectPath)
	writeTranscript(t, dir, inst.ClaudeSessionID, instanceConfigDirReplyReaderTranscript(inst.ClaudeSessionID, "digest: nothing open"))

	resp, err := inst.getClaudeLastResponse()
	if err != nil {
		t.Fatalf("getClaudeLastResponse() error = %v, want the reply under the group's config dir", err)
	}
	if resp.Content != "digest: nothing open" {
		t.Fatalf("getClaudeLastResponse() content = %q, want %q", resp.Content, "digest: nothing open")
	}
}

func TestGetClaudeLastResponse_KeepsTheProcessWideFallback(t *testing.T) {
	home := withTempAgentDeckHome(t, instanceConfigDirReplyReaderConfig)
	inst := instanceConfigDirReplyReaderInstance(t, home, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	// Written before the group's config_dir existed: only under the process-wide dir.
	dir := instanceConfigDirReplyReaderProjectsDir(t, filepath.Join(home, ".claude-global"), inst.ProjectPath)
	writeTranscript(t, dir, inst.ClaudeSessionID, instanceConfigDirReplyReaderTranscript(inst.ClaudeSessionID, "older reply"))

	resp, err := inst.getClaudeLastResponse()
	if err != nil {
		t.Fatalf("getClaudeLastResponse() error = %v, want the process-wide transcript as the fallback", err)
	}
	if resp.Content != "older reply" {
		t.Fatalf("getClaudeLastResponse() content = %q, want %q", resp.Content, "older reply")
	}
}

func TestFindLatestClaudeTranscriptOnDisk_ScansTheInstanceConfigDir(t *testing.T) {
	home := withTempAgentDeckHome(t, instanceConfigDirReplyReaderConfig)
	// The bound id's transcript rolled over (compaction): the reply is under a new id.
	inst := instanceConfigDirReplyReaderInstance(t, home, "cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	dir := instanceConfigDirReplyReaderProjectsDir(t, filepath.Join(home, ".claude-conductor"), inst.ProjectPath)
	rolled := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	writeTranscript(t, dir, rolled, instanceConfigDirReplyReaderTranscript(rolled, "reply after compaction"))

	id, resp := inst.findLatestClaudeTranscriptOnDisk()
	if id != rolled || resp == nil {
		t.Fatalf("findLatestClaudeTranscriptOnDisk() = (%q, %v), want the rollover %q under the group's config dir", id, resp, rolled)
	}
	if resp.Content != "reply after compaction" {
		t.Fatalf("findLatestClaudeTranscriptOnDisk() content = %q, want %q", resp.Content, "reply after compaction")
	}
}

func TestFindLatestClaudeTranscriptOnDisk_KeepsTheProcessWideFallback(t *testing.T) {
	home := withTempAgentDeckHome(t, instanceConfigDirReplyReaderConfig)
	inst := instanceConfigDirReplyReaderInstance(t, home, "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee")
	dir := instanceConfigDirReplyReaderProjectsDir(t, filepath.Join(home, ".claude-global"), inst.ProjectPath)
	rolled := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	writeTranscript(t, dir, rolled, instanceConfigDirReplyReaderTranscript(rolled, "rollover before the group dir"))

	id, resp := inst.findLatestClaudeTranscriptOnDisk()
	if id != rolled || resp == nil {
		t.Fatalf("findLatestClaudeTranscriptOnDisk() = (%q, %v), want the rollover %q under the process-wide dir", id, resp, rolled)
	}
}
