package session

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Sentinel errors for the account-switch flow. Callers use errors.Is to tell
// the "nothing happened" aborts apart from the one partial-success case where
// the account WAS switched but the restart failed.
var (
	// ErrUnknownAccount reports that <account> has no
	// [profiles.<account>.claude].config_dir block in config.toml.
	ErrUnknownAccount = errors.New("unknown account slot")
	// ErrAccountSwitchUnsupported reports a non-claude session.
	ErrAccountSwitchUnsupported = errors.New("account switch unsupported for this tool")
	// ErrAccountSwitchRestartFailed reports that the account was switched and
	// the conversation migrated, but the session could not be restarted. The
	// returned result is still valid and MUST be persisted by the caller.
	ErrAccountSwitchRestartFailed = errors.New("restart after account switch failed")
)

// AccountSwitchOptions tunes the switch flow.
type AccountSwitchOptions struct {
	// NoRestart leaves a previously-running session stopped after the switch
	// instead of restarting it with `claude --resume`.
	NoRestart bool
	// Persist commits the account metadata before any replacement starts.
	// Production callers must provide it; nil is supported for stopped unit fixtures.
	Persist func() error
}

// AccountSwitchResult describes a completed switch. It is returned (non-nil)
// only once the account field has actually been committed on the instance, so
// a non-nil result always means the caller must persist session state — even
// when the accompanying error is ErrAccountSwitchRestartFailed.
type AccountSwitchResult struct {
	OldAccount   string
	NewAccount   string
	MigratedPath string
	// Conversation is a human-readable summary of what happened to the
	// conversation file ("conversation migrated to ...", "no conversation ...").
	Conversation string
	Restarted    bool
	// Warnings collects non-fatal problems (e.g. folder-trust pre-seeding
	// failed). Callers surface these without treating the switch as failed.
	Warnings []string
}

// ConfiguredAccountNames lists profile names that have a Claude config_dir —
// i.e. the valid <account> values for SwitchAccount and `session set account`.
func ConfiguredAccountNames(cfg *UserConfig) []string {
	if cfg == nil {
		return nil
	}
	var names []string
	for name := range cfg.Profiles {
		if cfg.GetProfileClaudeConfigDir(name) != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// userConfigPathForMessages renders the config.toml path used in error text.
func userConfigPathForMessages() string {
	path, err := GetUserConfigPath()
	if err != nil {
		return "config.toml"
	}
	return path
}

// SwitchAccount moves a Claude session to another named account (#924) and
// carries its conversation over, so the restarted session resumes with full
// context under the new account's auth.
//
// The sequence is: validate the account → locate the conversation on disk →
// stop a running session → copy the conversation into the target config dir →
// verify the copy → pre-accept folder trust → commit the account field →
// restart. Every step before the commit aborts with a nil result, leaving the
// instance untouched; the caller then has nothing to persist.
//
// The migration is copy-only: the source account keeps its copy.
func SwitchAccount(cfg *UserConfig, inst *Instance, account string, opts AccountSwitchOptions) (*AccountSwitchResult, error) {
	if inst == nil {
		return nil, errors.New("no session")
	}
	account = strings.TrimSpace(account)
	targetDir := cfg.GetProfileClaudeConfigDir(account)
	if targetDir == "" {
		hint := "none configured"
		if available := ConfiguredAccountNames(cfg); len(available) > 0 {
			hint = strings.Join(available, ", ")
		}
		return nil, fmt.Errorf("%w: account %q has no [profiles.%s.claude].config_dir in %s (configured accounts: %s)",
			ErrUnknownAccount, account, account, userConfigPathForMessages(), hint)
	}
	if inst.Tool != "claude" {
		return nil, fmt.Errorf("%w: switch-account only supports claude sessions (tool: %s)",
			ErrAccountSwitchUnsupported, inst.Tool)
	}

	selection := inst.CaptureRuntimeSelection()
	wasRunning := inst.Exists()
	if wasRunning {
		inst.SyncSessionIDsFromTmux()
	}

	srcDir := GetClaudeConfigDirForInstance(inst)
	locatedDir, locatedSID, srcSize := LocateConversationConfigDir(cfg, inst, srcDir)
	if locatedDir != "" {
		srcDir = locatedDir
		if inst.ClaudeSessionID == "" && locatedSID != "" {
			return nil, fmt.Errorf(
				"cannot identify %s's conversation: it has no recorded conversation id, and the only "+
					"candidate is the newest transcript in its working directory (%s), which may belong "+
					"to another session there. Nothing was copied and the account was NOT switched, "+
					"and the session was left running untouched. "+
					"Name the conversation explicitly if it is this session's own "+
					"(agent-deck session set claude-session-id <uuid> %s) and re-run.",
				inst.Title, locatedSID, inst.Title)
		}
	}

	result := &AccountSwitchResult{NewAccount: account}
	accountCommitted := false
	var migrated string
	var migErr error
	commit := func() error {
		if wasRunning && locatedDir != "" {
			if freshDir, _, freshSize := LocateConversationConfigDir(cfg, inst, srcDir); freshDir != "" {
				locatedDir, srcDir, srcSize = freshDir, freshDir, freshSize
			}
		}
		migrated, migErr = MigrateConversationFrom(inst, srcDir, targetDir)
		if migErr != nil && !errors.Is(migErr, ErrNoConversation) {
			return fmt.Errorf("conversation migration failed, account not switched: %w", migErr)
		}
		if locatedDir != "" {
			if err := VerifyConversationInDir(inst, targetDir, srcSize); err != nil {
				return fmt.Errorf("conversation not verified in target config dir, account not switched: %w", err)
			}
		}
		if blocked, why := AccountSwitchRestartUnsafe(inst, locatedDir, wasRunning, opts.NoRestart); blocked {
			return fmt.Errorf("conversation verification failed for %s: %s", inst.Title, why)
		}
		if err := PreAcceptClaudeTrust(filepath.Join(targetDir, ".claude.json"), inst.EffectiveWorkingDir()); err != nil {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("could not pre-accept folder trust in target config dir: %v", err))
		}
		oldAccount, err := commitSwitchAccount(inst, account, opts.Persist)
		if err != nil {
			return err
		}
		result.OldAccount = oldAccount
		result.MigratedPath = migrated
		result.Conversation = describeMigration(migrated, migErr, locatedDir)
		accountCommitted = true
		return nil
	}

	restartRequested := wasRunning && !opts.NoRestart
	if opts.Persist == nil {
		if wasRunning {
			return nil, errors.New("account switch persistence callback required for running session")
		}
		if err := commit(); err != nil {
			return nil, err
		}
		return result, nil
	}

	runtime, switchErr := inst.SwitchAccountRuntime(selection, restartRequested, commit)
	_, failure, warning := ConsumePhysicalRuntimeResult(inst, runtime, switchErr, nil)
	if warning != "" {
		result.Warnings = append(result.Warnings, warning)
	}
	if !accountCommitted {
		return nil, failure
	}
	if failure != nil {
		return result, fmt.Errorf("%w: %v (start it manually with: agent-deck session start %s)",
			ErrAccountSwitchRestartFailed, failure, inst.Title)
	}
	result.Restarted = restartRequested
	return result, nil
}

func commitSwitchAccount(inst *Instance, account string, persist func() error) (string, error) {
	oldAccount, postCommit, err := SetField(inst, FieldAccount, account, nil)
	if err != nil {
		return oldAccount, err
	}
	if postCommit != nil {
		postCommit()
	}
	if persist == nil {
		return oldAccount, nil
	}
	if err := persist(); err == nil {
		return oldAccount, nil
	} else {
		_, rollbackPostCommit, rollbackErr := SetField(inst, FieldAccount, oldAccount, nil)
		if rollbackPostCommit != nil {
			rollbackPostCommit()
		}
		if rollbackErr != nil {
			return oldAccount, fmt.Errorf("failed to save session state: %v; failed to restore account: %w", err, rollbackErr)
		}
		return oldAccount, fmt.Errorf("failed to save session state: %w", err)
	}
}

// describeMigration renders the human summary of what happened to the
// conversation file.
func describeMigration(migrated string, migErr error, locatedDir string) string {
	if migrated != "" {
		return fmt.Sprintf("conversation migrated to %s", migrated)
	}
	if migErr != nil {
		return "no conversation to migrate (fresh session)"
	}
	if locatedDir != "" {
		return "conversation already present in the target config dir (nothing to migrate)"
	}
	return "no conversation found on disk (nothing to migrate)"
}

// AccountSwitchRestartUnsafe reports whether SwitchAccount must ABORT rather
// than restart the session, and why (#1815 Guard 2).
//
// In the reported incident the switch correctly diagnosed that it had no
// transcript to move ("no conversation to migrate, fresh session") for a
// session that had demonstrably been running a conversation — and then
// restarted it anyway. The restart's disk-discovery prelude, with no id to
// work from, adopted the newest transcript filed under the shared working
// directory (one belonging to a different session) and resumed it. A failed
// verification has to stop the sequence.
//
// The discriminator is ClaudeDetectedAt: a session that never bound a
// conversation id has nothing to lose and nothing to hijack (the Start
// prelude's #608 gate skips discovery for it). A session that HAS run,
// arriving here with no recorded id and no conversation located in ANY
// configured config dir, is exactly the unsafe state.
//
// The abort is scoped to the case where this flow would itself perform the
// restart; NoRestart (or an already-stopped session) is the documented way
// through, and the resume-time identity guard covers the later manual start.
func AccountSwitchRestartUnsafe(inst *Instance, locatedDir string, wasRunning, noRestart bool) (bool, string) {
	if inst == nil || !wasRunning || noRestart {
		return false, ""
	}
	if locatedDir != "" || inst.ClaudeSessionID != "" || inst.ClaudeDetectedAt.IsZero() {
		return false, ""
	}
	return true, "this session previously held a Claude conversation, but none was found in any " +
		"configured config dir and it has no recorded conversation id; account not switched and " +
		"session NOT restarted (restarting in this state can adopt a transcript belonging to another " +
		"session in the same directory). Re-run with --no-restart to switch the account only, then " +
		"start it manually once the conversation is located"
}
