package ui

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"
)

// A blocked legacy registry read stands in for arbitrarily slow cleanup IO.
// Exercise the actual deletion handlers, without a replacement cleanup stub.
//
// Under runtime authority the delete and finish commands own the whole
// deletion: they claim the captured runtime, delete its row, and prune hook
// artifacts under the instance lifecycle lock before reporting the result. So
// the key handlers must only dispatch that command, the row must be gone
// before cleanup starts, the handler that applies the report must neither
// block nor schedule work of its own, and undo becomes available only once
// the reported deletion (cleanup included) has been applied.
//
// hooks-fd-macos.yml copies this file and runtime_authority_fixture_test.go
// onto pinned upstream revisions from before the cleanup fixes and greps their
// runs for this test's case names and failure messages. Keep both files
// compilable against those revisions, and update the workflow's grep strings
// when a message it names changes.
func TestHookCleanupDeletionKeepsUIResponsive(t *testing.T) {
	for _, tc := range []struct {
		name   string
		finish bool
		undo   bool
	}{
		{"delete", false, false},
		{"finish", true, false},
		{"undo", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_DATA_HOME", "")
			t.Setenv("XDG_CONFIG_HOME", "")
			detachGlobalStateDB(t) // NewHome registers its own, soon-closed database.
			h := NewHome()
			require.NotNil(t, h.storage)
			t.Cleanup(func() { _ = h.storage.Close() })
			h.width, h.height = 100, 30
			h.initialLoading = false // Fixture represents an already loaded session list.
			inst := &session.Instance{ID: "gone", Title: "gone", Tool: "shell", Status: session.StatusStopped, CreatedAt: time.Now()}
			h.instances = []*session.Instance{inst}
			h.instanceByID = map[string]*session.Instance{"gone": inst}
			h.groupTree = session.NewGroupTree(h.instances)
			h.rebuildFlatItems()
			require.NoError(t, h.storage.SaveWithGroups(h.instances, h.groupTree))
			hooks := session.GetHooksDir()
			require.NoError(t, os.MkdirAll(hooks, 0700))
			artifact := filepath.Join(hooks, "gone.json")
			require.NoError(t, os.WriteFile(artifact, []byte("{}"), 0600))
			profile, err := session.GetProfileDir("delayed")
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(profile, 0700))
			fifo := filepath.Join(profile, "sessions.json")
			require.NoError(t, syscall.Mkfifo(fifo, 0600))
			// Nonblocking writer open succeeds only when cleanup has opened the
			// read end. This handshake avoids guessing when the command is ready.
			// A command that reports on the reported channel first never
			// reached cleanup; its report is returned instead of a writer.
			openWriter := func(reported <-chan tea.Msg) (*os.File, tea.Msg) {
				deadline := time.Now().Add(5 * time.Second)
				for {
					fd, err := syscall.Open(fifo, syscall.O_WRONLY|syscall.O_NONBLOCK, 0600)
					if err == nil {
						return os.NewFile(uintptr(fd), fifo), nil
					}
					require.ErrorIs(t, err, syscall.ENXIO)
					select {
					case msg := <-reported:
						return nil, msg
					default:
					}
					if time.Now().After(deadline) {
						t.Fatal("cleanup did not open the delayed registry")
					}
					time.Sleep(time.Millisecond)
				}
			}
			// UI handlers run under the same bound whether they handle the
			// confirming key or the reported deletion. A handler stuck on the
			// registry is released so the failure is reported, not a hang.
			update := func(msg tea.Msg) tea.Cmd {
				returned := make(chan tea.Cmd, 1)
				go func() { _, cmd := h.Update(msg); returned <- cmd }()
				select {
				case cmd := <-returned:
					return cmd
				case <-time.After(time.Second):
					writer, _ := openWriter(nil)
					_, _ = writer.Write([]byte(`{"instances":[]}`))
					_ = writer.Close()
					select {
					case <-returned:
					case <-time.After(5 * time.Second):
						t.Fatal("deletion did not recover after cleanup was released")
					}
					t.Fatal("UI deletion handler blocked on hook cleanup")
					return nil
				}
			}
			const outsideAuthority = "deletion handler scheduled work outside lifecycle authority"
			// Confirming the deletion is the UI handler; it must only dispatch.
			var keys []tea.KeyMsg
			if tc.finish {
				// A root with no repository at or above it keeps the finish's
				// git steps inert, wherever the test's temporary directory is.
				root := t.TempDir()
				t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(root))
				h.worktreeFinishDialog.Show(inst.ID, inst.Title, "gone-branch", root, filepath.Join(root, "missing"), "main")
				keys = []tea.KeyMsg{
					{Type: tea.KeySpace, Runes: []rune{' '}}, // no merge
					{Type: tea.KeyEnter},
					{Type: tea.KeyRunes, Runes: []rune{'y'}},
				}
			} else {
				h.confirmDialog.ShowDeleteSession(inst.ID, inst.Title, false, false)
				keys = []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune{'y'}}}
			}
			for _, key := range keys[:len(keys)-1] {
				h.Update(key)
			}
			cmd := update(keys[len(keys)-1])
			require.NotNil(t, cmd, "deletion must dispatch its lifecycle command")
			result := make(chan tea.Msg, 1)
			go func() { result <- cmd() }()
			writer, early := openWriter(result)
			if writer == nil {
				// The command reported without reaching cleanup: either its
				// deletion failed (an authority refusal) or it left cleanup
				// to someone else. Apply the report as production would: a
				// handler that cleans up itself blocks or schedules work and
				// fails with its own message. Either failure names the
				// report's error, which tells the two causes apart.
				var reportErr error
				switch report := early.(type) {
				case sessionDeletedMsg:
					reportErr = report.killErr
				case worktreeFinishResultMsg:
					reportErr = report.err
				}
				require.Nil(t, update(early), "%s (early %T, error: %v)", outsideAuthority, early, reportErr)
				t.Fatalf("deletion reported before hook cleanup ran: %T, error: %v", early, reportErr)
			}
			defer writer.Close()
			select {
			case <-result:
				t.Fatal("cleanup did not wait for the delayed registry")
			default:
			}
			rows, _, err := h.storage.LoadLite()
			require.NoError(t, err)
			require.Empty(t, rows, "registry deletion must commit before cleanup")
			// Input and rendering still work while the command waits on the registry.
			h.Update(tea.WindowSizeMsg{Width: 110, Height: 35})
			require.Equal(t, 110, h.width)
			if tc.finish {
				// Finish shows its progress until the command reports, cleanup
				// included. Esc dismisses that progress without cancelling it.
				require.Contains(t, stripAnsi(h.View()), "Finishing Worktree...")
				h.Update(tea.KeyMsg{Type: tea.KeyEsc})
				require.False(t, h.worktreeFinishDialog.IsVisible(), "Esc must dismiss the finish while cleanup is blocked")
				require.True(t, h.worktreeFinishDialog.isExecuting, "dismissing the finish must not forget it is running")
				require.ErrorContains(t, h.err, "finishing worktree 'gone' in the background")
				select {
				case <-result:
					t.Fatal("dismissing the finish ended its command")
				default:
				}
			}
			h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
			require.True(t, h.search.IsVisible(), "search input must work while cleanup is blocked")
			frame := stripAnsi(h.View())
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				require.NoError(t, os.WriteFile("testdata/hook_cleanup_search.txt", []byte(frame), 0644))
			}
			golden, err := os.ReadFile("testdata/hook_cleanup_search.txt")
			require.NoError(t, err)
			require.Equal(t, string(golden), frame, "search frame while deletion cleanup is blocked")
			require.FileExists(t, artifact)
			if tc.undo {
				// The deletion has not been reported yet, so there is nothing a
				// restart could race: undo must not be offered.
				h.Update(tea.KeyMsg{Type: tea.KeyEsc})
				_, undoCmd := h.Update(tea.KeyMsg{Type: tea.KeyCtrlZ})
				require.Nil(t, undoCmd, "undo offered before hook cleanup completed")
			}
			_, err = writer.Write([]byte(`{"instances":[]}`))
			require.NoError(t, err)
			require.NoError(t, writer.Close())
			var deleted tea.Msg
			select {
			case deleted = <-result:
			case <-time.After(5 * time.Second):
				t.Fatal("background cleanup did not complete")
			}
			require.NoFileExists(t, artifact)
			if tc.finish {
				finished, ok := deleted.(worktreeFinishResultMsg)
				require.True(t, ok, "finish reported %T", deleted)
				require.NoError(t, finished.err)
			} else {
				reported, ok := deleted.(sessionDeletedMsg)
				require.True(t, ok, "delete reported %T", deleted)
				require.NoError(t, reported.killErr)
			}
			// The command already deleted the row and pruned the hooks; a
			// handler that deletes or cleans up again does so outside the
			// lifecycle lock, where it can hit a same-ID replacement.
			require.Nil(t, update(deleted), outsideAuthority)
			require.Nil(t, h.getInstanceByID(inst.ID), "reported deletion must leave the session list")
			if tc.finish {
				require.False(t, h.worktreeFinishDialog.isExecuting, "reported finish must release the dialog for the next finish")
			}
			if tc.undo {
				// A deliberately invalid account makes Restart return before any
				// process launch. Its error still reveals when Restart was attempted.
				inst.Account = "missing-undo-test-account"
				_, undoCmd := h.Update(tea.KeyMsg{Type: tea.KeyCtrlZ})
				require.NotNil(t, undoCmd)
				undoResult := make(chan tea.Msg, 1)
				go func() { undoResult <- undoCmd() }()
				select {
				case result := <-undoResult:
					restored, ok := result.(sessionRestoredMsg)
					require.True(t, ok)
					require.ErrorContains(t, restored.err, "missing-undo-test-account")
				case <-time.After(5 * time.Second):
					t.Fatal("undo did not resume after cleanup completed")
				}
			}
		})
	}
}
