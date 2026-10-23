package tmux

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/asheshgoplani/agent-deck/internal/logging"
	"github.com/asheshgoplani/agent-deck/internal/platform"
	"github.com/asheshgoplani/agent-deck/internal/shellwords"
)

var (
	statusLog  = logging.ForComponent(logging.CompStatus)
	respawnLog = logging.ForComponent(logging.CompSession)
	mcpLog     = logging.ForComponent(logging.CompMCP)
	perfLog    = logging.ForComponent(logging.CompPerf)
)

// execCommand is a swappable seam that defaults to exec.Command. Tests
// override it to inject failure into specific launcher names without
// mutating host PATH or systemd state. Production callers always read
// the default. See TestStartCommandSpec_FallsBackToDirect in
// tmux_fallback_test.go for the contract.
var execCommand = exec.Command

// Status-bar hint labels. The attach loop's detach/switch keys are configurable
// ([hotkeys].detach / [hotkeys].switch_session), so the status-right hint must
// follow the resolved bindings instead of hardcoding them. The UI layer pushes
// the resolved labels here via SetStatusHints whenever hotkeys are (re)resolved;
// the defaults keep the hint correct for the default config before that first
// call. switchHintEnabled is false when the switch key is unbound or collides
// with detach (the attach loop drops it in those cases), so the hint then omits
// the switch segment.
var (
	statusHintMu      sync.RWMutex
	detachHintLabel   = "ctrl+q"
	switchHintLabel   = "ctrl+s"
	switchHintEnabled = true
)

const SessionPrefix = "agentdeck_"

// IsTmuxAvailable checks if tmux is installed and accessible
// Returns nil if tmux is available, otherwise returns an error with details
func IsTmuxAvailable() error {
	cmd := exec.Command("tmux", "-V")
	output, err := commandCombinedOutput(cmd)
	if err != nil {
		return fmt.Errorf("tmux not found or not working: %w (output: %s)", err, string(output))
	}
	return nil
}

// TerminalInfo contains detected terminal information
type TerminalInfo struct {
	Name              string // Terminal name (warp, iterm2, kitty, alacritty, etc.)
	SupportsOSC8      bool   // Supports OSC 8 hyperlinks
	SupportsOSC52     bool   // Supports OSC 52 clipboard
	SupportsTrueColor bool   // Supports 24-bit color
}

// IsAtuinPTYProxy checks if the current shell session is running under atuin's
// pty-proxy. Atuin pty-proxy acts as a PTY MITM between the terminal and the
// shell, intercepting all I/O. It sets ATUIN_PTY_PROXY_ACTIVE when active.
//
// agent-deck's Bubble Tea TUI is incompatible with atuin pty-proxy because:
//   - os.Stdin/os.Stdout are pipes to atuin's proxy, not direct terminal FDs
//   - Alternate screen sequences (tea.WithAltScreen) may be swallowed
//   - Mouse tracking sequences (tea.WithMouseCellMotion) may not be forwarded
//
// Users should use `atuin init zsh` (or bash/fish) instead of
// `atuin pty-proxy init zsh` when running agent-deck.
func IsAtuinPTYProxy() bool {
	return os.Getenv("ATUIN_PTY_PROXY_ACTIVE") != ""
}

// DetectTerminal identifies the current terminal emulator from environment variables
// Returns terminal name: "warp", "iterm2", "kitty", "alacritty", "vscode", "windows-terminal", or "unknown"
func DetectTerminal() string {
	// Check terminal-specific environment variables (most reliable)

	// Warp Terminal
	if os.Getenv("TERM_PROGRAM") == "WarpTerminal" || os.Getenv("WARP_IS_LOCAL_SHELL_SESSION") != "" {
		return "warp"
	}

	// iTerm2
	if os.Getenv("TERM_PROGRAM") == "iTerm.app" || os.Getenv("ITERM_SESSION_ID") != "" {
		return "iterm2"
	}

	// kitty
	if os.Getenv("TERM") == "xterm-kitty" || os.Getenv("KITTY_WINDOW_ID") != "" {
		return "kitty"
	}

	// Alacritty
	if os.Getenv("ALACRITTY_SOCKET") != "" || os.Getenv("ALACRITTY_LOG") != "" {
		return "alacritty"
	}

	// VS Code integrated terminal
	if os.Getenv("TERM_PROGRAM") == "vscode" || os.Getenv("VSCODE_INJECTION") != "" {
		return "vscode"
	}

	// Windows Terminal
	if os.Getenv("WT_SESSION") != "" {
		return "windows-terminal"
	}

	// WezTerm
	if os.Getenv("TERM_PROGRAM") == "WezTerm" || os.Getenv("WEZTERM_PANE") != "" {
		return "wezterm"
	}

	// Apple Terminal.app
	if os.Getenv("TERM_PROGRAM") == "Apple_Terminal" {
		return "apple-terminal"
	}

	// Hyper
	if os.Getenv("TERM_PROGRAM") == "Hyper" {
		return "hyper"
	}

	// Check TERM_PROGRAM as fallback
	if termProgram := os.Getenv("TERM_PROGRAM"); termProgram != "" {
		return strings.ToLower(termProgram)
	}

	return "unknown"
}

// GetTerminalInfo returns detailed terminal capabilities
func GetTerminalInfo() TerminalInfo {
	terminal := DetectTerminal()

	info := TerminalInfo{
		Name:              terminal,
		SupportsOSC8:      false,
		SupportsOSC52:     false,
		SupportsTrueColor: false,
	}

	// Check COLORTERM for true color support
	colorterm := os.Getenv("COLORTERM")
	if colorterm == "truecolor" || colorterm == "24bit" {
		info.SupportsTrueColor = true
	}

	// Set capabilities based on terminal
	// Reference: https://github.com/Alhadis/OSC8-Adoption
	switch terminal {
	case "warp":
		// Warp: Full modern terminal support
		info.SupportsOSC8 = true  // Native clickable paths
		info.SupportsOSC52 = true // Clipboard integration
		info.SupportsTrueColor = true

	case "iterm2":
		// iTerm2: Excellent escape sequence support
		info.SupportsOSC8 = true
		info.SupportsOSC52 = true
		info.SupportsTrueColor = true

	case "kitty":
		// kitty: Full modern terminal support
		info.SupportsOSC8 = true
		info.SupportsOSC52 = true
		info.SupportsTrueColor = true

	case "alacritty":
		// Alacritty: OSC 8 since v0.11, OSC 52 supported
		info.SupportsOSC8 = true
		info.SupportsOSC52 = true
		info.SupportsTrueColor = true

	case "wezterm":
		// WezTerm: Full support
		info.SupportsOSC8 = true
		info.SupportsOSC52 = true
		info.SupportsTrueColor = true

	case "windows-terminal":
		// Windows Terminal: OSC 8 since v1.4
		info.SupportsOSC8 = true
		info.SupportsOSC52 = true
		info.SupportsTrueColor = true

	case "vscode":
		// VS Code: OSC 8 supported in integrated terminal
		info.SupportsOSC8 = true
		info.SupportsOSC52 = true
		info.SupportsTrueColor = true

	case "hyper":
		// Hyper: Limited OSC support
		info.SupportsOSC8 = false
		info.SupportsOSC52 = true
		info.SupportsTrueColor = true

	case "apple-terminal":
		// Apple Terminal.app: No OSC 8 support
		info.SupportsOSC8 = false
		info.SupportsOSC52 = false
		info.SupportsTrueColor = false

	default:
		// Unknown terminal - assume basic support
		// Most modern terminals support these features
		info.SupportsOSC8 = true // Optimistic default
		info.SupportsOSC52 = true
	}

	return info
}

func detectToolFromCommand(command string) string {
	cmdLower := launchCommands(strings.ToLower(strings.TrimSpace(command)))
	if cmdLower == "" {
		return ""
	}

	fields, valid := shellwords.Split(cmdLower)
	if valid {
		if tool := toolForExecutable(shellwords.ExecutableBase(fields)); tool != "" {
			return tool
		}
		if isOMPCommand(fields) {
			return "omp"
		}
	}

	switch {
	case strings.Contains(cmdLower, "claude"):
		return "claude"
	case strings.Contains(cmdLower, "gemini"):
		return "gemini"
	case strings.Contains(cmdLower, "opencode") || strings.Contains(cmdLower, "open code") || strings.Contains(cmdLower, "open-code"):
		return "opencode"
	case strings.Contains(cmdLower, "codex"):
		return "codex"
	case strings.Contains(cmdLower, "copilot") || strings.Contains(cmdLower, "@github/copilot"):
		return "copilot"
	case strings.Contains(cmdLower, "crush"):
		return "crush"
	// No fallback arm for muse: the executable basename in the base switch
	// above is the only classifier. Any mid-line token form (`my-wrapper
	// muse --flag`, `echo muse --help`) is indistinguishable from prose or
	// another tool's arguments at this layer, so wrappers fail closed to
	// shell here (they still launch verbatim via passthrough). This keeps
	// the tmux layer consistent with the registry's executableOnly rule.
	case strings.Contains(cmdLower, "cursor"):
		return "cursor"
	case strings.Contains(cmdLower, "hermes"):
		return "hermes"
	case isDeepSeekCommand(fields) ||
		strings.Contains(cmdLower, "@deepseek-ai/dsh") ||
		strings.Contains(cmdLower, "dsh --profile") ||
		strings.Contains(cmdLower, "dsh web") ||
		strings.Contains(cmdLower, "dsh plugin"):
		// Deliberately NOT a bare Contains on "dsh": those three letters appear
		// in "dshell", "dshboard", and any path containing them, and — unlike
		// the permissive arms above — `grep dsh README.md` is an entirely
		// ordinary thing to see in a pane. Detection therefore needs the token
		// in COMMAND position (isDeepSeekCommand, which sees through the
		// `DSH_HOME=… dsh` env prefix agent-deck itself emits), the documented
		// npx spelling, or a dsh subcommand/launcher flag right after it.
		return "deepseek"
	case strings.Contains(cmdLower, " pi ") || strings.HasPrefix(cmdLower, "pi "):
		return "pi"
	default:
		return ""
	}
}

// toolForExecutable returns the tool that an executable basename starts, or ""
// when the basename names no tool.
func toolForExecutable(base string) string {
	switch base {
	case "claude":
		return "claude"
	case "gemini":
		return "gemini"
	case "opencode", "opencode2", "open-code":
		return "opencode"
	case "codex":
		return "codex"
	case "copilot":
		return "copilot"
	case "crush":
		return "crush"
	case "muse":
		return "muse"
	case "cursor":
		return "cursor"
	case "agent":
		// Standalone Cursor Agent CLI binary (~/.local/bin/agent).
		return "cursor"
	case "hermes":
		return "hermes"
	case "dsh":
		// DeepSeek Harness. The binary is `dsh`; the agent-deck tool is
		// named for the vendor.
		return "deepseek"
	case "pi":
		return "pi"
	case "omp", "oh-my-pi":
		return "omp"
	}
	return ""
}

// setupCommands are what agent-deck runs around a tool launch: cd -- <project>,
// env files ([ -f … ] && source …), unset and export of its variables, the
// per-session directory (mkdir), and the exit-receipt trailer (umask, printf,
// exit). Their arguments are paths and variable names, never the tool.
var setupCommands = map[string]bool{
	"cd": true, "export": true, "unset": true, "source": true, ".": true,
	"[": true, "mkdir": true, "umask": true, "printf": true, "exit": true,
}

// launchCommands drops agent-deck's setup commands from a launch chain and
// expands `<shell> -c <script>` in place, leaving the commands that start the
// tool. #2683 put `unset CLAUDE_JOB_DIR` into every hook-status tool's chain,
// so the substring fallback in detectToolFromCommand read every pi, codex, and
// gemini launch as claude. A command that does not parse is returned unchanged.
//
// A command whose own executable is a tool is that tool's launch, so a shell in
// its arguments (`codex exec bash -c '...'`) is not expanded.
func launchCommands(command string) string {
	commands, ok := shellwords.Commands(command)
	if !ok {
		return command
	}
	var kept []string
	for _, cmd := range commands {
		words, valid := shellwords.Split(cmd)
		if !valid {
			kept = append(kept, cmd)
			continue
		}
		if toolForExecutable(shellwords.ExecutableBase(words)) != "" {
			kept = append(kept, cmd)
			continue
		}
		if script, isShell := shellScript(words); isShell {
			if inner := launchCommands(script); inner != "" {
				kept = append(kept, inner)
			}
			continue
		}
		if !isSetupCommand(words) {
			kept = append(kept, cmd)
		}
	}
	return strings.Join(kept, " ; ")
}

// shellScript returns the script of a `<shell> -c <script>` command wherever
// the shell appears in it: the exit-receipt wrapper runs the launch chain as
// `bash -c '<chain>'; code=$?; …`, an exec prefix may come before it, and a
// wrapper that puts a program in front of {command} receives the chain as
// `<program> bash -c '<chain>'` (substituteWrapperCommand).
func shellScript(words []string) (string, bool) {
	for i := 0; i+2 < len(words); i++ {
		switch filepath.Base(words[i]) {
		case "bash", "sh", "zsh", "dash", "ksh":
		default:
			continue
		}
		flag := words[i+1]
		if strings.HasPrefix(flag, "-") && !strings.HasPrefix(flag, "--") && strings.ContainsRune(flag, 'c') {
			return words[i+2], true
		}
	}
	return "", false
}

// isSetupCommand reports whether a simple command only prepares the pane: a
// bare assignment (session_dir=…, code=$?) or one of setupCommands, ignoring a
// subshell's opening parenthesis.
func isSetupCommand(words []string) bool {
	for _, word := range words {
		if isShellAssignmentToken(word) {
			continue
		}
		return setupCommands[strings.TrimLeft(word, "({")]
	}
	return true
}

// isOMPCommand recognizes only supported OMP launchers in command position.
// In particular, incidental argument text such as `grep omp README.md` is not
// evidence that the pane runs OMP.
func isOMPCommand(fields []string) bool {
	for idx, field := range fields {
		if isShellAssignmentToken(field) {
			continue
		}
		base := filepath.Base(strings.Trim(field, `"'`))
		base = strings.TrimSuffix(strings.TrimSuffix(base, ".exe"), ".cmd")
		if base == "env" {
			continue
		}
		if base == "omp" || base == "oh-my-pi" {
			return true
		}
		return base == "npx" && idx+1 < len(fields) &&
			strings.Trim(fields[idx+1], `"'`) == "@oh-my-pi/pi-coding-agent"
	}
	return false
}

// isDeepSeekCommand reports whether the first COMMAND-position token of a
// command line is the dsh binary. Leading `VAR=value` assignments are skipped:
// agent-deck's own launch string is `DSH_HOME=… AGENTDECK_…=… dsh --profile …`,
// so fields[0] is an assignment, not the program.
//
// This is the precise half of DeepSeek detection; the substring arms beside it
// cover the npx spelling and subcommand forms.
func isDeepSeekCommand(fields []string) bool {
	for _, field := range fields {
		if isShellAssignmentToken(field) {
			continue
		}
		base := filepath.Base(strings.Trim(field, `"'`))
		base = strings.TrimSuffix(strings.TrimSuffix(base, ".exe"), ".cmd")
		return base == "dsh"
	}
	return false
}

// isShellAssignmentToken reports whether a token is a `NAME=value` environment
// assignment rather than a program name.
func isShellAssignmentToken(token string) bool {
	idx := strings.IndexByte(token, '=')
	if idx <= 0 {
		return false
	}
	for i, r := range token[:idx] {
		isAlpha := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_'
		if i == 0 && !isAlpha {
			return false
		}
		if i > 0 && !isAlpha && !(r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// Session represents a tmux session
// NOTE: All mutable fields are protected by mu. The Bubble Tea event loop is single-threaded,
// but we use mutex protection for defensive programming and future-proofing.
type Session struct {
	// Name is the tmux session name. Start may rewrite it (fresh suffix when
	// the name is taken), so readers that can run concurrently with Start,
	// such as the status polls, use TmuxName() instead of the field (#2808).
	Name string
	// nameMu guards Name and Command against Start's rewrite. It is a leaf
	// lock: held only for the field access, never across a tmux call or while
	// taking mu, so it can be taken with or without mu held.
	nameMu      sync.RWMutex
	DisplayName string
	WorkDir     string
	// GroupPath is the agent-deck group/tree path this session belongs to
	// (e.g. "projects/devops"). It feeds the @agentdeck_group_path tmux user
	// option so a custom [display] title_format can render the group hierarchy
	// in the outer terminal title. Empty when the session has no group. Kept in
	// sync by the session layer (construction, reconnect, rename, regroup).
	GroupPath       string
	groupTitleMu    sync.Mutex
	Command         string
	Created         time.Time
	InstanceID      string // Agent-deck instance ID for hook callbacks
	startupAt       time.Time
	startupTimedOut bool // terminal for this pane generation after #1892's deadline
	// afterStartupTimeoutClaim is a test seam for scheduling a competing pane
	// generation after timeout recovery releases mu but before GetStatus decides
	// which generation's status to return.
	afterStartupTimeoutClaim func()
	// afterStartupAliveProbe is a test seam for scheduling a competing pane
	// generation (a respawn) between startupShowsAgentAlive's probe and the
	// generation-guarded clear of startupAt in GetStatus (#2361).
	afterStartupAliveProbe func()

	// WorkDirIsPlaceholder marks a session whose local WorkDir is not where the
	// work happens — today that means an SSH session, whose pane only runs an
	// `ssh` client while the project lives on the remote host. Such a session
	// keeps working even if the local directory disappears, so it opts out of
	// the #1713 working-directory guards (see workdir_guard.go) rather than
	// being refused a start over a path nothing reads.
	WorkDirIsPlaceholder bool

	// SocketName is the tmux `-L <name>` socket selector for this session.
	// When empty (pre-v1.7.50 default), every tmux call targets the user's
	// default server at $TMUX_TMPDIR/tmux-<uid>/default, preserving the
	// historical behavior exactly. When non-empty, every tmux subprocess
	// spawned by methods on this Session carries `-L <SocketName>` so the
	// agent-deck tmux server is fully isolated from the user's interactive
	// tmux.
	//
	// SocketName is populated at session-creation time from (in precedence
	// order) the CLI flag `--tmux-socket`, then `[tmux].socket_name` in
	// config.toml, then empty. It is persisted per-instance in SQLite so
	// subsequent restarts/revives reach the correct server even if the
	// installation-wide config later changes. See RFC socket-isolation
	// phase 1 and Instance.TmuxSocketName. Never mutate after Start().
	SocketName string

	// mu protects all mutable fields below from concurrent access
	mu sync.Mutex

	// PERFORMANCE: Lazy initialization flag
	// When true, ConfigureStatusBar/EnableMouseMode have been run
	// Allows deferring non-essential tmux configuration until first attach
	configured bool

	// PERFORMANCE: Cache CapturePane content for short duration (500ms)
	// Reduces subprocess spawns during rapid status checks/log events
	cacheMu      sync.RWMutex
	cacheContent string
	cacheTime    time.Time
	// cachePrimedUntil extends cacheContent past the 500ms window when it
	// came from a batched capture (PrimePaneCaptures): one status pass reads
	// every row's frame from that single tmux call even when the pass itself
	// runs longer than 500ms on a loaded host.
	cachePrimedUntil time.Time
	captureSf        singleflight.Group // Deduplicates concurrent CapturePane subprocess calls

	// Content tracking for HasUpdated (separate from StateTracker)
	lastHash    string
	lastContent string

	// Cached tool detection (avoids re-detecting every status check)
	detectedTool     string
	toolDetectedAt   time.Time
	toolDetectExpiry time.Duration // How long before re-detecting (default 30s)

	// Cached background-work probe (BackgroundWorkSince). The hook fast path in
	// UpdateStatus has no captured pane content, so it must capture separately to
	// check for in-flight background work; this bounds that to one capture per
	// bgWorkCacheTTL while a session sits at the prompt.
	bgWork               BackgroundWork
	bgWorkBlocked        bool
	bgWorkForegroundBusy bool
	bgWorkCheckedAt      time.Time

	// lastBackgroundWork is the background work (issue #2473) the last
	// prepared pane frame showed, read from the frame BEFORE the agent-roster
	// trim (the workflow row is drawn under the footer and the trim removes
	// it). Set by prepareFrame; read by the status and substate decisions on
	// the same frame.
	lastBackgroundWork BackgroundWork
	// lastBackgroundBlocked records that the same frame shows something that
	// outranks background work (an open menu, an error banner, the
	// model-unavailable no-op): see backgroundWorkOutrankedLocked.
	lastBackgroundBlocked bool

	// Simple state tracking (hash-based)
	stateTracker *StateTracker

	// Last status returned (for debugging)
	lastStableStatus string

	// lastSubstate is the additive Honest-Status-v2 refinement computed
	// alongside the coarse status during GetStatus (model-unavailable,
	// auth-401, idle-at-empty-prompt, running). Surfaced via GetSubstate so the
	// CLI/TUI/transition-event layers can report WHY a session is in its status
	// without changing the byte-stable canonical status string.
	lastSubstate Substate
	// lastSubstateDetail is free-text detail for lastSubstate when the
	// classifier captured one (the codex usage-limit retry time); "" otherwise.
	lastSubstateDetail string

	// completedTurnIdle / completedTurnSampledAt: the completed-turn verdict
	// (finished Claude turn at an idle prompt) of the last classified pane
	// frame and when it was captured — the hook-lag rule's evidence, taken
	// from reads GetStatus/GetSubstate make anyway (see completed_turn.go).
	completedTurnIdle      bool
	completedTurnSampledAt time.Time

	// lastSampleAuthFailure is the credential-failure verdict of the most recent
	// sample that could READ the pane, and lastAuthFailureContent the snapshot
	// backing it (see authfailure.go). Unlike lastSubstate neither is cleared
	// when the pane dies: they are the only surviving evidence of WHY the process
	// exited, and the death path — which runs when there is no pane left to read
	// — is their only consumer.
	lastSampleAuthFailure  bool
	lastAuthFailureContent string

	// hashFallbackOnce gates the one-time hash_fallback_used WARN landmark.
	// See logging_additions.go and logging-review G8.
	hashFallbackOnce sync.Once

	// OptionOverrides are user-specified tmux set-option overrides from config.
	// Applied AFTER all defaults in Start(), so they take precedence.
	// Keys are tmux option names, values are their settings.
	// Example: {"allow-passthrough": "all", "history-limit": "50000"}
	OptionOverrides map[string]string

	// RunCommandAsInitialProcess launches Start(command) as the pane's initial
	// process instead of sending it via SendKeysAndEnter after session creation.
	// Sandbox sessions enable this so pane-dead detection can restart exited tools.
	RunCommandAsInitialProcess bool

	// LaunchEnv is NAME=value pairs Start puts in the environment of the new
	// session and its first pane process (new-session -e, tmux >= 3.2; see
	// SupportsLaunchEnv). The pane's shell starts with them set, so nothing
	// has to be typed into it (#2817).
	LaunchEnv []string

	// VimMode guarantees the inner agent's input composer is in insert mode
	// before any text/Enter is delivered. When the inner tool (Claude Code with
	// `"editorMode": "vim"`) leaves its prompt in vim NORMAL mode — the default
	// state after a turn finishes — a bracketed paste still lands in the input
	// widget, but the trailing Enter is interpreted as a navigation keystroke
	// instead of submit, so the message is typed but never sent (issue #1264).
	// When true, SendEnter and SendKeysAndEnter prepend an Escape + `i` sequence
	// so the prompt is guaranteed to be in insert mode. The sequence is
	// idempotent: Escape always lands in normal mode and `i` always enters
	// insert, so it is safe even when the prompt is already in insert mode.
	// Off by default (zero value) — non-vim sessions and other tools are
	// unaffected. Populated at session-creation time from [claude].vim_mode.
	VimMode bool

	// LaunchInUserScope starts the tmux server through systemd-run --user --scope
	// so the server is owned by the user's systemd manager instead of the current
	// login session scope.
	LaunchInUserScope bool

	// LaunchAs overrides the spawn form (v1.7.21+). Valid values:
	// "scope", "service", "direct", "auto", or "" (defer to
	// LaunchInUserScope). "service" uses systemd-run --user --unit
	// <NAME>.service with Type=forking + Restart=on-failure so tmux
	// auto-restarts on OOM / SIGKILL / unexpected death. Unknown values
	// fall through to LaunchInUserScope behavior — populated by callers
	// from TmuxSettings.GetLaunchAs which already canonicalises.
	LaunchAs string

	// Custom patterns for generic tool support
	customToolName       string
	customBusyPatterns   []string
	customPromptPatterns []string
	customDetectPatterns []string

	// Configurable patterns (replaces hardcoded detection logic)
	// When non-nil, hasBusyIndicator and normalizeContent use these instead of hardcoded values
	resolvedPatterns *ResolvedPatterns

	// Cached PromptDetector (avoids allocating a new one on every hasPromptIndicator call)
	cachedPromptDetector     *PromptDetector
	cachedPromptDetectorTool string

	// Environment variable cache (reduces tmux show-environment subprocess spawns)
	envCache   map[string]envCacheEntry
	envCacheMu sync.RWMutex

	// injectStatusLine controls whether ConfigureStatusBar actually modifies tmux.
	// When false, the status bar configuration is skipped entirely.
	// Default: true (set via SetInjectStatusLine from user config)
	injectStatusLine bool

	// mouse controls whether tmux `mouse on` is set during session creation
	// and by EnableMouseMode. When false, tmux never captures mouse events so
	// the terminal emulator keeps them — fixes VS Code Linux integrated
	// terminal click-drag selection (issue #730).
	// Default: true (set via SetMouse from user config)
	mouse bool

	// indicZeroWidthMarks is [tmux] indic_zero_width_marks (#2334, default
	// false), set via SetIndicZeroWidthMarks. indicZeroWidthMarksSet records
	// that the config was applied at all: a Session built without it (tmux
	// discovery) must neither add the marks nor remove a user's opt-in.
	indicZeroWidthMarks    bool
	indicZeroWidthMarksSet bool

	// clearOnRestart controls whether RespawnPane clears the scrollback buffer.
	// When false (default), previous session output is preserved.
	// Set via SetClearOnRestart from user config.
	clearOnRestart bool

	// terminalChromeEnabled controls whether Attach emits outer-terminal
	// chrome sequences (currently the iTerm2 badge) on attach/detach.
	// Default: false (opt-in via [terminal].iterm_badge in user config; set
	// here through SetTerminalChromeEnabled). AGENTDECK_ITERM_BADGE=0|1
	// overrides this at runtime in either direction; see chrome.go.
	terminalChromeEnabled bool
}

const (
	envCacheTTL = 30 * time.Second
	// envNegativeCacheTTL bounds how long a miss ("variable not set", dead
	// session, probe timeout) is trusted. Shorter than envCacheTTL so a
	// session that gains CLAUDE_SESSION_ID out-of-process is still noticed
	// within seconds; in-process SetEnvironment invalidates the entry
	// immediately, so the common bind path never waits this out.
	envNegativeCacheTTL = 5 * time.Second
	startupStateWindow  = 2 * time.Minute
)

// invalidateCache clears the CapturePane cache.
// MUST be called after any action that might change terminal content.
func (s *Session) invalidateCache() {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	s.cacheContent = ""
	s.cacheTime = time.Time{}
	s.cachePrimedUntil = time.Time{}
}

// SetCustomPatterns sets custom patterns for generic tool support
// These patterns enable custom tools defined in config.toml to have proper status detection
func (s *Session) SetCustomPatterns(toolName string, busyPatterns, promptPatterns, detectPatterns []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.customToolName = toolName
	s.customBusyPatterns = busyPatterns
	s.customPromptPatterns = promptPatterns
	s.customDetectPatterns = detectPatterns
}

// SetPatterns sets the compiled ResolvedPatterns for configurable status detection.
// When set, hasBusyIndicator and normalizeContent use these instead of hardcoded values.
func (s *Session) SetPatterns(p *ResolvedPatterns) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolvedPatterns = p
}

// SetDetectPatterns sets tool auto-detection patterns (separate from busy/prompt patterns).
func (s *Session) SetDetectPatterns(toolName string, detectPatterns []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.customToolName = toolName
	s.customDetectPatterns = detectPatterns
}

// SetInjectStatusLine controls whether ConfigureStatusBar modifies tmux settings.
// When set to false, the status bar is left unchanged, preserving user's tmux config.
func (s *Session) SetInjectStatusLine(inject bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.injectStatusLine = inject
}

// SetTerminalChromeEnabled controls whether Attach emits outer-terminal
// chrome (currently the iTerm2 badge) on attach/detach. Mirrors the
// SetInjectStatusLine plumbing pattern: callers in internal/session read
// `[terminal].iterm_badge` from user config and forward it here.
// AGENTDECK_ITERM_BADGE overrides this at runtime; see chrome.go.
func (s *Session) SetTerminalChromeEnabled(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.terminalChromeEnabled = enabled
}

// terminalChromeIsEnabled is the read-side accessor used by Attach. Locked
// read so a concurrent Set call cannot publish a torn bool — same shape as
// the other Session getters.
func (s *Session) terminalChromeIsEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminalChromeEnabled
}

// SetMouse controls whether tmux mouse mode is enabled for this session.
// When false, the inline `mouse on` set-option during Start is skipped AND
// EnableMouseMode becomes a no-op — required for VS Code Linux integrated
// terminal click-drag selection (issue #730).
func (s *Session) SetMouse(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mouse = enabled
}

// SetIndicZeroWidthMarks mirrors [tmux] indic_zero_width_marks (#2334,
// default off): whether Start gives Indic spacing vowel signs zero width on
// the tmux server.
func (s *Session) SetIndicZeroWidthMarks(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.indicZeroWidthMarks = enabled
	s.indicZeroWidthMarksSet = true
}

// indicZeroWidthMarksArgs returns the opt-in's set-option chunks for Start's
// command chain; with the key off it removes entries a previous opt-in left.
// It reads the field without s.mu, like s.mouse on the same paths, because
// EnableMouseMode runs it under EnsureConfigured's lock.
func (s *Session) indicZeroWidthMarksArgs() []string {
	if !s.indicZeroWidthMarksSet {
		return nil
	}
	if _, overridden := s.OptionOverrides["codepoint-widths"]; overridden {
		return nil
	}
	ver := hostTmuxVersionString()
	if !s.indicZeroWidthMarks {
		removeOwnedIndicZeroWidthMarks(s.SocketName, ver)
		return nil
	}
	return indicZeroWidthArgs(ver)
}

// GetMouse reports whether tmux mouse mode is currently enabled for this
// session. Used by tests and by the Start / EnableMouseMode code paths to
// decide whether to set `mouse on`.
func (s *Session) GetMouse() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mouse
}

// SetClearOnRestart controls whether RespawnPane clears the scrollback buffer.
// When false (default), previous output is preserved on restart.
func (s *Session) SetClearOnRestart(clear bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearOnRestart = clear
}

// NewSession creates a new Session instance with a unique name
func NewSession(name, workDir string) *Session {
	sanitized := sanitizeName(name)
	// Add unique suffix to prevent name collisions
	uniqueSuffix := generateShortID()
	return &Session{
		Name:                  SessionPrefix + sanitized + "_" + uniqueSuffix,
		DisplayName:           name,
		WorkDir:               workDir,
		Created:               time.Now(),
		startupAt:             time.Now(),
		lastStableStatus:      "waiting",
		toolDetectExpiry:      30 * time.Second, // Re-detect tool every 30 seconds
		injectStatusLine:      true,             // Default: inject status bar
		mouse:                 true,             // Default: mouse on (#730 opt-out)
		terminalChromeEnabled: false,            // Default: opt-in (set true via [terminal].iterm_badge)
		// stateTracker and promptDetector will be created lazily on first status check
	}
}

// TmuxName returns the tmux session name under nameMu, so it is safe to call
// while another goroutine runs Start, which may rename the session (#2808).
func (s *Session) TmuxName() string {
	s.nameMu.RLock()
	defer s.nameMu.RUnlock()
	return s.Name
}

// launchCommand returns Command under nameMu; Start rewrites it while the
// status poll may be detecting the tool (#2808).
func (s *Session) launchCommand() string {
	s.nameMu.RLock()
	defer s.nameMu.RUnlock()
	return s.Command
}

// generateShortID generates a short random ID for uniqueness
func generateShortID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		// Fallback to timestamp
		return fmt.Sprintf("%d", time.Now().UnixNano()%100000)
	}
	return hex.EncodeToString(b)
}

// sanitizeNameRe matches characters not allowed in tmux session names.
var sanitizeNameRe = regexp.MustCompile(`[^a-zA-Z0-9-]+`)

// sanitizeName converts a display name to a valid tmux session name
func sanitizeName(name string) string {
	// Replace spaces and special characters with hyphens
	return sanitizeNameRe.ReplaceAllString(name, "-")
}

// gatedTmuxKeyOptionArgs returns the tmux argument chunks for agent-deck's
// key-handling defaults — escape-time, extended-keys, extended-keys-format and
// terminal-features — each gated through OptionOverrides so an explicit user
// setting wins (#1625). Every chunk begins with a ";" chain separator, so
// callers append the result after an existing set-option in the same tmux
// command (the same shape the window-size default already uses).
//
// extended-keys / extended-keys-format are *server* options (set -s). Emitting
// them unconditionally on every spawn overrode a deliberate
// `set -s extended-keys off` in the user's ~/.tmux.conf server-wide, which on
// some terminals (e.g. Windows Terminal + WSL2) stops Enter from submitting in
// the pane. Gating each key through OptionOverrides lets config.toml [tmux]
// options — and by extension the user's own tmux config — take effect instead of
// being silently clobbered.
//
// terminal-features is a server-wide ARRAY, so it gets its own treatment: the
// `-a` (append) form grew it by one item on every pass and never shrank
// (#2061). terminalFeatures performs guarded cleanup and verified no-overwrite
// insertion, subject to the collision limits documented in terminal_features.go.
// It can read and mutate server options, so the override gate must come first.
// A nil func emits nothing.
func gatedTmuxKeyOptionArgs(name string, overrides map[string]string, terminalFeatures func()) []string {
	args := make([]string, 0, 20)
	if _, ok := overrides["escape-time"]; !ok {
		args = append(args, ";", "set-option", "-t", name, "escape-time", "10")
	}
	if _, ok := overrides["extended-keys"]; !ok {
		args = append(args, ";", "set", "-sq", "extended-keys", "on")
	}
	if _, ok := overrides["extended-keys-format"]; !ok {
		// csi-u so modified keys reach the pane as ESC[13;2u (the kitty
		// keyboard-protocol form Claude Code reads) rather than the default
		// xterm modifyOtherKeys ESC[27;2;13~, which Claude Code ignores —
		// otherwise Shift+Enter collapses to a bare Enter and submits.
		args = append(args, ";", "set", "-sq", "extended-keys-format", "csi-u")
	}
	if _, ok := overrides["terminal-features"]; !ok && terminalFeatures != nil {
		terminalFeatures()
	}
	return args
}

// Start creates and starts a tmux session.
// By default, command is sent after session creation (legacy behavior).
// When RunCommandAsInitialProcess is true, command is passed directly to tmux
// new-session and becomes the pane's initial process.
func (s *Session) Start(command string) error {
	// Defense in depth against the 2026-04-17 three-cascade bug.
	// See assertTestTmuxIsolation for the full rationale.
	assertTestTmuxIsolation()

	// #1713: resolve and validate the working directory BEFORE mutating session
	// state or spawning anything. tmux does NOT fail when -c points at a missing
	// directory — it silently starts the pane in $HOME — so without this check a
	// session whose project directory (or worktree) was deleted starts "fine"
	// and runs the agent against the user's home directory instead. Failing here
	// keeps the session untouched and surfaces the real reason at the CLI.
	workDir := s.WorkDir
	if workDir == "" {
		workDir = os.Getenv("HOME")
	}
	resolvedWorkDir, workDirErr := resolveStartWorkDir(workDir)
	if workDirErr != nil {
		if !s.WorkDirIsPlaceholder {
			statusLog.Warn("tmux_start_refused_bad_workdir",
				slog.String("session", logging.SanitizeValue(s.Name)),
				slog.String("requested_workdir", logging.SanitizeValue(s.WorkDir)),
				slog.String("error", workDirErr.Error()))
			return workDirErr
		}
		// An SSH session's local path is a placeholder: the pane only runs an
		// ssh client, so a missing local directory must not block the start.
		// Keep tmux's historical $HOME landing, but say so instead of hiding it.
		statusLog.Warn("tmux_start_placeholder_workdir_fallback",
			slog.String("session", logging.SanitizeValue(s.Name)),
			slog.String("requested_workdir", logging.SanitizeValue(workDir)),
			slog.String("error", workDirErr.Error()))
		home, homeErr := resolveStartWorkDir(os.Getenv("HOME"))
		if homeErr != nil {
			return workDirErr
		}
		resolvedWorkDir = home
	}
	workDir = resolvedWorkDir

	s.nameMu.Lock()
	s.Command = command
	s.nameMu.Unlock()
	s.invalidateCache()
	s.Created = time.Now()
	s.mu.Lock()
	s.startupAt = s.Created
	s.startupTimedOut = false
	s.lastStableStatus = "waiting"
	s.stateTracker = nil
	s.cachedPromptDetector = nil
	s.cachedPromptDetectorTool = ""
	s.mu.Unlock()

	// Check if session already exists (shouldn't happen with unique IDs, but handle gracefully)
	if s.Exists() {
		// Session with this exact name exists - regenerate with new unique suffix
		sanitized := sanitizeName(s.DisplayName)
		renamed := SessionPrefix + sanitized + "_" + generateShortID()
		s.nameMu.Lock()
		s.Name = renamed
		s.nameMu.Unlock()
	}

	// Create new tmux session in detached mode with the command as the initial
	// process. This avoids the slow shell-wait-sendkeys path (~2s pane ready poll).
	// Commands containing bash-specific syntax are wrapped for fish compatibility.
	//
	// workDir was resolved and validated at the top of Start (#1713).
	launcher, args := s.startCommandSpec(workDir, command)
	// newSpawnCommand (not bare execCommand) so the spawn — and any tmux server
	// it starts — runs from SpawnBaseDir and can never inherit a directory that
	// is later deleted. See workdir_guard.go.
	cmd := newSpawnCommand(launcher, args...)
	output, err := commandCombinedOutput(cmd)
	if err != nil {
		if launcher == "tmux" {
			if recovered, recoverErr := recoverFromStaleDefaultSocketIfNeeded(string(output)); recoverErr != nil {
				statusLog.Warn("tmux_stale_socket_recovery_failed",
					slog.String("session", logging.SanitizeValue(s.Name)),
					slog.String("error", recoverErr.Error()),
				)
			} else if recovered {
				statusLog.Warn("tmux_start_retry_after_socket_recovery",
					slog.String("session", s.Name),
				)
				output, err = commandCombinedOutput(newSpawnCommand(launcher, args...))
			}
		}
	}
	if err != nil && launcher == "systemd-run" {
		// systemd-run detection said yes but invocation failed (e.g. dbus
		// down, lingering disabled, broken user manager). Log a structured
		// warning and retry with softer wrap forms so session creation is
		// never blocked.
		//
		// Three-tier fallback chain (v1.7.21):
		//  1. Originally requested form (service or scope) — already failed above
		//  2. If originally service: try scope-mode systemd-run
		//  3. Direct tmux (no systemd wrap)
		//
		// Each failed tier is logged and its error collected. If all three
		// fail the returned error carries all three diagnostics so operators
		// can triage via a single log grep.
		statusLog.Warn("tmux_systemd_run_fallback",
			slog.String("session", s.Name),
			slog.String("error", err.Error()),
			slog.String("output", string(output)))

		initialErr := err
		initialOutput := output
		initialLabel := "systemd-run path"
		if wasServiceModeArgs(args) {
			initialLabel = "service path"
		} else if wasScopeModeArgs(args) {
			initialLabel = "scope path"
		}

		tmuxArgs := stripSystemdRunPrefix(args)

		// Tier 2: if the first attempt was service-mode, try scope-mode
		// as an intermediate step BEFORE falling all the way to direct.
		// This matters when the user manager supports scopes but
		// services fail (e.g. transient-unit restart-property constraints
		// on very old systemd).
		var scopeErr error
		var scopeOutput []byte
		triedScope := false
		if wasServiceModeArgs(args) {
			scopeRetryArgs := buildScopeArgsFromTmuxArgs(s.Name, tmuxArgs)
			scopeOutput, scopeErr = commandCombinedOutput(newSpawnCommand("systemd-run", scopeRetryArgs...))
			triedScope = true
			if scopeErr == nil {
				output = scopeOutput
				err = nil
			} else {
				statusLog.Warn("tmux_systemd_run_scope_fallback_failed",
					slog.String("session", s.Name),
					slog.String("error", scopeErr.Error()),
					slog.String("output", string(scopeOutput)))
			}
		}

		// Tier 3: direct tmux. Only attempted if we're still in an error
		// state after tier 2 (or if tier 2 was skipped because the
		// initial attempt was scope-mode, in which case it's the next
		// tier down).
		if err != nil {
			retryOutput, retryErr := commandCombinedOutput(newSpawnCommand("tmux", tmuxArgs...))
			if retryErr == nil {
				output = retryOutput
				err = nil
			} else {
				// All tiers failed — compose a single error that lists
				// every diagnostic.
				if triedScope {
					return fmt.Errorf(
						"failed to create tmux session: %s: %w (output: %s); scope path: %v (output: %s); direct retry: %v (output: %s)",
						initialLabel, initialErr, string(initialOutput),
						scopeErr, string(scopeOutput),
						retryErr, string(retryOutput))
				}
				return fmt.Errorf(
					"failed to create tmux session: systemd-run path: %w (output: %s); direct retry: %v (output: %s)",
					initialErr, string(initialOutput), retryErr, string(retryOutput))
			}
		}
	}
	if err != nil {
		return fmt.Errorf("failed to create tmux session: %w (output: %s)", err, string(output))
	}

	// Register session in cache immediately to prevent race condition
	// where Exists() returns false because cache was refreshed before session creation
	registerSessionInCache(s.Name)

	// #1713: a tmux server whose OWN cwd was unlinked stops honouring -c and
	// births every pane in that dead directory, where the pane's shell cannot
	// getcwd() and the agent never execs. SpawnBaseDir prevents that for servers
	// agent-deck starts, but a server started earlier (or by another tool) can
	// already be poisoned, so confirm where the pane actually landed. Reporting
	// such a session as started is the exact "looked created, never ran the
	// agent" failure from the report — tear it down and say why instead.
	//
	// #2214 (shell-tool gap): when RunCommandAsInitialProcess is false and a
	// command is pending, this pane was just opened as a BARE interactive
	// shell — no cd-assert has been sent into it yet, so checking now would
	// inspect the still-poisoned pane and fail closed before the send-keys
	// fallback below ever gets a chance to recover it exactly like the
	// initial-process path does. Defer the guard for that path until after
	// the cd-assert command has actually been sent (see the check further
	// down, right after SendKeysAndEnter).
	deferCwdGuardForShellFallback := command != "" && !s.RunCommandAsInitialProcess
	if !deferCwdGuardForShellFallback {
		if cwdErr := s.verifyPaneWorkDirUnlessPlaceholder(workDir); cwdErr != nil {
			return s.killAfterPaneCwdFailure(cwdErr)
		}
	}

	// PERFORMANCE: Batch all session options into a single subprocess call.
	// Before: 7 separate exec.Command calls = 7 subprocess spawns (~50-70ms)
	// After:  1 exec.Command call = 1 subprocess spawn (~7-10ms)
	//
	// Options set:
	// - window-style/window-active-style: Prevent color issues in some terminals (Warp, etc.)
	// - mouse on: Mouse scrolling, text selection, pane resizing
	// - allow-passthrough on: OSC 8 hyperlinks, OSC 52 clipboard (tmux 3.2+, -q for older)
	// - set-clipboard on: Clipboard integration (Warp, iTerm2, kitty, etc.)
	// - escape-time 10: Fast Vim/editor responsiveness (default 500ms is too slow)
	//
	// Note: history-limit is NOT set here — the user's tmux.conf value is respected.
	// Users can override via [tmux] options = { "history-limit" = "50000" } in config.toml.
	// - extended-keys on: Forward Shift+Enter and other modified keys to apps (tmux 3.2+)
	// - extended-keys-format csi-u: Deliver them as ESC[13;2u (kitty form Claude Code reads), not xterm ESC[27;2;13~ (tmux 3.4+)
	// - terminal-features hyperlinks+extkeys: Track hyperlinks and enable extended key reporting (tmux 3.4+, server-wide)
	//
	// Note: remain-on-exit is NOT set here — it is only enabled for sandbox sessions
	// via OptionOverrides to avoid changing behaviour for non-sandbox sessions.
	themeStyle := currentTmuxThemeStyle()

	startArgs := make([]string, 0, 40)
	if _, ok := s.OptionOverrides["window-style"]; !ok {
		startArgs = append(startArgs, "set-option", "-t", s.Name, "window-style", themeStyle.windowStyle, ";")
	}
	if _, ok := s.OptionOverrides["window-active-style"]; !ok {
		startArgs = append(startArgs, "set-option", "-t", s.Name, "window-active-style", themeStyle.windowActiveStyle, ";")
	}
	// #730: users opt out of mouse capture via [tmux].mouse = false so
	// terminals like VS Code Linux can do native click-drag selection.
	if s.mouse {
		startArgs = append(startArgs,
			"set-option", "-t", s.Name, "mouse", "on", ";")
	}
	startArgs = append(startArgs,
		"set-option", "-t", s.Name, "-q", "allow-passthrough", "on", ";",
		"set-option", "-t", s.Name, "set-clipboard", "on")
	// #1625: the key-handling defaults are gated through OptionOverrides so an
	// explicit user tmux setting wins (see gatedTmuxKeyOptionArgs).
	startArgs = append(startArgs, gatedTmuxKeyOptionArgs(s.Name, s.OptionOverrides, s.configureTerminalFeatures)...)
	// #2334: opt-in only; see complex_script_widths.go for the trade-off.
	startArgs = append(startArgs, s.indicZeroWidthMarksArgs()...)
	// Multi-client size policy (#2186, #2259, shared attach): every window
	// of a Deck session follows the client that is using it
	// (window-size=latest, aggressive-resize on; `largest` on a tmux < 3.1),
	// defined once in windowPolicyOptions. The lines appended here reach the
	// initial window and publish the effective values as @agentdeck_*
	// session options; NewShellWindow re-applies the policy for windows Deck
	// itself opens, and a window a user opens by hand (tmux's own `c`
	// binding) gets it from a server-wide after-new-window hook that reads
	// those options (see installWindowPolicyHook). ApplySharedViewSize
	// re-applies it before every attach (sharedview.go).
	startArgs = append(startArgs, s.windowPolicyStartArgs()...)
	_ = commandRun(s.tmuxCmd(startArgs...))
	s.installWindowPolicyHook()

	// Bind Ctrl+Q to detach at the tmux level as fallback for terminals where
	// XON/XOFF flow control intercepts the key before it reaches the PTY stdin
	// reader (e.g. iTerm2 on macOS). Only binds on agentdeck-managed sessions.
	//
	// #1808: tmux key bindings live on the SERVER (one per socket), not on the
	// session — a baked-in `if-shell [ "#{session_name}" = "<s.Name>" ]`
	// guard, re-installed by every Start(), only ever matched whichever
	// session started most recently on this socket. Because the binding is
	// "-n -T root", tmux's root key table consumes Ctrl+Q on every client
	// attached to the socket before it ever reaches the pane — so in every
	// other session sharing the socket, the guard's empty else-branch did NOT
	// let the key fall through to the running app; it silently swallowed the
	// keypress instead, so Ctrl+Q simply did nothing until a client attached
	// to the one session Start() most recently ran on the socket. Use
	// if-shell -F to re-evaluate a tmux FORMAT against the CURRENT client's
	// session at keypress time instead of baking in one session name — this
	// scopes the detach to "whichever agentdeck session this client is
	// attached to" rather than "the one session that happened to Start()
	// last", and matches the same prefix-match pattern
	// BindMouseStatusRightDetach already uses for its detach binding.
	//
	// #1820: the guard used to be a `run-shell` shell script with the
	// session name substituted into a single-quoted shell word by tmux's
	// FORMATS expansion BEFORE the string reached /bin/sh — a session name
	// containing a single quote (tmux permits quotes in names; only "."
	// and ":" are forbidden) could break out of the quoting and have the
	// remainder executed as shell commands on whichever socket owns the
	// pane, including the user's own default tmux server. `if-shell -F`
	// evaluates its pattern (#{m:pattern,string}) entirely inside tmux's own
	// format engine — nothing is ever handed to a shell, so no session name
	// can break out of anything — and "detach-client" runs directly on the
	// invoking client's own command queue (no run-shell subprocess), so it
	// detaches exactly the client that pressed the key even when the session
	// has more than one client attached (e.g. a web `tmux -C` control client
	// alongside a native terminal client; see controlpipe.go).
	// Bounded — see tmuxMutationTimeout. Best-effort already, and it runs on
	// the session-create path where a wedged client would stall the create.
	_ = s.runBoundedMutation(append([]string{"bind-key", "-n", "-T", "root", "C-q"}, ctrlQDetachBindArgs()...)...)

	// Apply user-specified tmux option overrides from config (after defaults).
	// These are batched into a single call when multiple overrides are present.
	if len(s.OptionOverrides) > 0 {
		args := make([]string, 0, len(s.OptionOverrides)*6)
		first := true
		for key, value := range s.OptionOverrides {
			if !first {
				args = append(args, ";")
			}
			args = append(args, "set-option", "-t", s.Name, "-q", key, value)
			first = false
		}
		_ = commandRun(s.tmuxCmd(args...))
	}

	// Configure status bar with session info for easy identification
	// Shows: session title on left, project folder on right
	s.ConfigureStatusBar()
	s.ConfigureTerminalTitle()

	// Wait for the pane shell to be ready before sending the command via send-keys.
	// On WSL/Linux non-interactive contexts, pane initialisation can take 100-500ms and
	// sending keys before the shell is ready causes them to be silently swallowed.
	// Non-fatal best-effort guard: if the timeout expires, log a warning and continue
	// anyway (degraded path, same as the behaviour before this guard was added).
	if command != "" && !s.RunCommandAsInitialProcess {
		paneReadyTimeout := 2 * time.Second
		if platform.IsWSL() {
			paneReadyTimeout = 5 * time.Second
		}
		if err := waitForPaneReady(s, paneReadyTimeout); err != nil {
			statusLog.Warn("pane_ready_timeout",
				slog.String("session", s.Name),
				slog.String("timeout", paneReadyTimeout.String()),
				slog.String("error", err.Error()),
			)
		}
	}

	// Fallback: if RunCommandAsInitialProcess is false, send command via send-keys.
	if command != "" && !s.RunCommandAsInitialProcess {
		// Always wrap in bash -c so the command runs under bash regardless
		// of the user's login shell. See #526 and bashCWrap for details.
		//
		// #2214: this pane's initial process was already spawned into workDir
		// via -c above (no command yet at that point), so a poisoned server's
		// dead cwd could already have landed the pane's shell in the wrong
		// place before we ever get here. cd-assert the sent command itself so
		// the same guarantee applies to the send-keys fallback path.
		if err := s.SendKeysAndEnter(bashCWrap(cwdAssertCommand(workDir, command))); err != nil {
			return fmt.Errorf("failed to send command: %w", err)
		}

		// #2214: this is exactly the path whose cwd guard was deferred above
		// (deferCwdGuardForShellFallback is true for every session reaching
		// here). Run it now, after the cd-assert has actually been sent, so a
		// genuinely poisoned server is still caught — just without rejecting
		// the pane before it had a chance to recover.
		if cwdErr := s.verifyPaneWorkDirUnlessPlaceholder(workDir); cwdErr != nil {
			return s.killAfterPaneCwdFailure(cwdErr)
		}
	}

	// Connect control mode pipe for event-driven status detection
	if pm := GetPipeManager(); pm != nil {
		if err := pm.Connect(s.Name, s.SocketName); err != nil {
			statusLog.Debug(
				"control_pipe_connect_failed",
				slog.String("session", s.Name),
				slog.String("error", err.Error()),
			)
		}
	}

	// Note: We tried using tmux hooks for instant GREEN status detection:
	// - alert-activity: Only fires for background windows (not current window)
	// - after-send-keys: Fires for ALL send-keys calls (too noisy, catches agent-deck operations)
	// Neither works reliably for detecting user input. We use polling for GREEN instead.
	// The Stop hook (via Claude settings) handles instant YELLOW detection.

	return nil
}

// killAfterPaneCwdFailure tears down a session whose pane provably landed in a
// dead directory (#1713/#2214) and returns the original cwd error, so Start
// never reports a session that looked created but never ran the agent. A
// failure to clean up is logged, never substituted for the real cause.
func (s *Session) killAfterPaneCwdFailure(cwdErr error) error {
	if killErr := s.Kill(); killErr != nil {
		statusLog.Warn("deleted_cwd_session_cleanup_failed",
			slog.String("session", logging.SanitizeValue(s.Name)),
			slog.String("error", killErr.Error()))
	}
	return cwdErr
}

// ProbeExists asks the tmux server on this session's own socket whether a
// session with EXACTLY this name exists, bypassing every cache and every
// "assume alive" fallback that Exists uses on the status hot path. It is the
// authoritative check for a caller that is about to report a spawn as
// successful (#2099): Exists answers positive from a cached listing, a timed
// out probe or a protocol-mismatched socket, none of which prove that THIS
// session is up.
//
// The `=` target prefix makes tmux match the name exactly instead of by
// prefix, so a sibling named like this session plus a suffix cannot answer
// for it. A completed client proves absence only with tmux's canonical
// answer that this exact session or the server is missing
// (isCanonicalMissingSessionResult, as for probeSessionExistence). Other
// failures are indeterminate and reported as errors, never as either verdict:
// a probe that timed out, was refused by a protocol-mismatched server, never
// produced a completed tmux client, or completed with any other diagnostic.
// Callers deciding whether a session's process tree may be treated as absent
// (#1873) depend on that distinction.
func (s *Session) ProbeExists() (bool, error) {
	return ProbeSessionOnSocket(s.SocketName, s.TmuxName())
}

// ProbeSessionOnSocket is ProbeExists for a session known only by its tmux
// name and socket (Instance.TmuxSocketName verbatim; "" is the default
// server): an exact-name, uncached has-session whose false is proven absence
// and whose error is indeterminate.
func ProbeSessionOnSocket(socketName, name string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), hasSessionProbeTimeout)
	defer cancel()
	_, err := commandOutput(tmuxExecContext(ctx, socketName, "has-session", "-t", "="+name))
	if err == nil {
		return true, nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		return false, fmt.Errorf("tmux has-session probe for %q timed out after %s", name, hasSessionProbeTimeout)
	}
	if socketHasProtocolMismatch(socketName) {
		return false, fmt.Errorf("tmux client/server protocol version mismatch on socket %q", socketName)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || !exitErr.Exited() {
		return false, fmt.Errorf("tmux has-session probe for %q did not complete: %w", name, err)
	}
	if isCanonicalMissingSessionResult(err, name) {
		return false, nil
	}
	return false, fmt.Errorf("tmux has-session probe for %q was inconclusive: %w", name, err)
}

// Exists checks if the tmux session exists
// Uses cached session list when available (refreshed by RefreshExistingSessions)
// Falls back to direct tmux call if cache is stale
func (s *Session) Exists() bool {
	// #755: the cache describes the DefaultSocketName() server, so a session on
	// a different socket must not be answered from it (a same-named entry is not
	// the same session). Keep that guard.
	//
	// Within the guard, trust only a POSITIVE hit. A NEGATIVE/stale result is
	// NOT trusted: the cache can transiently miss a live session when
	// agent-deck sessions span multiple sockets (RefreshAllActivities merges one
	// pipe per socket, and the subprocess fallback covers only DefaultSocketName,
	// so a refresh sourced from the "wrong" socket omits this one). Confirm a
	// "not in cache" reading with the live pipe / a direct probe on the
	// session's OWN socket before declaring it dead. Trusting a negative cache
	// hit flipped live sessions on a second socket to StatusError/tmux_missing
	// (multi-socket cache aliasing), after which restart machinery could kill
	// the still-running pane.
	if s.existsInProcess() {
		return true
	}
	return s.existsProbe()
}

// existsProbe is the subprocess half of Exists: a bounded has-session on the
// session's own socket.
func (s *Session) existsProbe() bool {
	// Cache is stale (or skipped for an isolated socket): fall back to a
	// direct tmux check on the session's own socket. Bound it: a server that
	// is briefly busy can make the probe hang, and a probe that never answers
	// is indeterminate — assume the session still exists rather than reporting
	// it dead (which would flip a live session to StatusError). Only a probe
	// that actually completes with a non-success status means "gone".
	ctx, cancel := context.WithTimeout(context.Background(), hasSessionProbeTimeout)
	defer cancel()
	err := commandRun(s.tmuxCmdContext(ctx, "has-session", "-t", s.TmuxName()))
	if ctx.Err() == context.DeadlineExceeded {
		return true // probe timed out: indeterminate, assume still alive
	}
	// A completed non-zero exit is only AUTHORITATIVE if the client reached the
	// server. A client/server protocol version mismatch fails every command on
	// this socket fast with a non-zero exit, and the refusal itself proves the
	// server — and therefore this session — is alive: only the client binary is
	// wrong. Treating it as "gone" flipped live sessions to StatusError (via
	// terminatedPaneStatus) even though nothing crashed. Assume alive, like the
	// timeout branch, and let a later poll with a matching client resolve it.
	//
	// The verdict is a property of the client/server binary pair, not of this
	// session, so it is settled out of band and cached per socket. That keeps
	// this probe on Run() with nil stdio: no pipe, no copy goroutine, nothing
	// for WaitDelay to bound on the status hot path.
	if err != nil && socketHasProtocolMismatch(s.SocketName) {
		return true
	}
	return err == nil
}

// AbsentFromCache reports, without spawning a subprocess, that the current
// session listing positively shows this session missing from the default
// server and no PipeManager connection holds it. Exists() does not trust that
// negative because it can transiently miss a LIVE session; a caller may only
// use it to keep an already-dead session dead, where a miss just delays
// noticing a revival until the next refresh.
func (s *Session) AbsentFromCache() bool {
	if strings.TrimSpace(s.SocketName) != DefaultSocketName() {
		return false
	}
	if exists, cacheValid := sessionExistsFromCache(s.Name); !cacheValid || exists {
		return false
	}
	if pm := GetPipeManager(); pm != nil && pm.IsConnected(s.Name) {
		return false
	}
	return true
}

// existsInProcess is the positive half of Exists that needs no subprocess: a
// positive hit in the default-socket cache (#755 guard) or a live PipeManager
// control connection.
func (s *Session) existsInProcess() bool {
	if strings.TrimSpace(s.SocketName) == DefaultSocketName() {
		if exists, cacheValid := sessionExistsFromCache(s.TmuxName()); cacheValid && exists {
			return true
		}
	}
	// If PipeManager has a live control connection, the session definitely exists.
	if pm := GetPipeManager(); pm != nil {
		if pm.IsConnected(s.TmuxName()) {
			return true
		}
	}
	return false
}

// ExistsListed is Exists for a status pass that lists this session's socket
// with ListSessionNamesOnSocket. listed is called only when Exists would probe
// tmux (no in-process positive). When its listing proves the session absent,
// ExistsListed answers (false, true) without a has-session of its own, so a
// dead row costs no tmux subprocess. Without such proof (nil listing, or a
// name tmux could resolve another way) it answers exactly like Exists, with
// listedAbsent false.
//
// A listing that names the session is not used as a positive: such a row
// still runs its own has-session, as Exists would. So a live row with no
// in-process positive (another socket without a PipeManager connection, or
// a stale default-socket cache) costs that has-session plus its share of
// one list-sessions per socket per listing age.
func (s *Session) ExistsListed(listed func() map[string]struct{}) (exists, listedAbsent bool) {
	if s.existsInProcess() {
		return true, false
	}
	if listingProvesAbsent(listed(), s.TmuxName()) {
		return false, true
	}
	return s.existsProbe(), false
}

// listingProvesAbsent reports whether a complete list-sessions result proves
// that `has-session -t name` would fail. tmux resolves a session target by
// exact name, then as a glob, then by unique prefix, and reads ':' '.' and a
// leading '$' '@' '%' as other target syntax and a leading '=' as an exact
// match of the rest, so absence is proven only for a plain name that no listed
// session starts with.
func listingProvesAbsent(listed map[string]struct{}, name string) bool {
	if listed == nil || name == "" || strings.ContainsAny(name, ":.*?[\\$@%=") {
		return false
	}
	for n := range listed {
		if strings.HasPrefix(n, name) {
			return false
		}
	}
	return true
}

// ExistsCached is a cheap, non-blocking liveness check for hot periodic loops
// that iterate over ALL sessions every tick (e.g. the background configure loop
// and theme propagation). It NEVER spawns a subprocess on the calling
// goroutine: a single wedged/absent server multiplied by ~1000 sessions is the
// exact subprocess-storm that froze the UI main goroutine 4–7s per tick (see
// the nav-freeze fix). It answers only from in-process state:
//
//   - a POSITIVE session-cache hit on the session's OWN socket, or
//   - a live PipeManager control connection, or
//   - a POSITIVE hit in the per-socket cache (isolated sockets).
//
// The socket guard mirrors Exists(): the shared sessionCache describes
// DefaultSocketName() only, so a same-named entry on a different socket is not
// this session (#755 / multi-socket cache aliasing). Sessions on an isolated
// socket are therefore answered from a SEPARATE per-socket cache, refreshed in
// the background by one `list-sessions` per socket per TTL. Without it those
// sessions would read false FOREVER (never in the default cache; PipeManager
// only connects wanted/attached sessions), permanently starving them of
// EnsureConfigured and theme propagation — a bug the "retries next tick"
// framing hid. Bounding the refresh by SOCKET count, not session count, keeps
// the no-storm invariant that motivated this method.
//
// A false is still a deliberate under-report (cold cache, dead server, probe
// failure): the only cost is that a cosmetic loop skips this session until the
// cache warms. Callers MUST NOT feed ExistsCached() into destructive/restart
// machinery — use Exists() (which confirms a negative with a real probe).
func (s *Session) ExistsCached() bool {
	if strings.TrimSpace(s.SocketName) == DefaultSocketName() {
		if exists, cacheValid := sessionExistsFromCache(s.TmuxName()); cacheValid && exists {
			return true
		}
	} else if sessionExistsOnSocketCached(s.SocketName, s.TmuxName()) {
		return true
	}
	if pm := GetPipeManager(); pm != nil {
		if pm.IsConnected(s.TmuxName()) {
			return true
		}
	}
	return false
}

// IsPaneDead returns true if the session's pane process has exited.
// Uses the cached pane info (refreshed once per tick) for zero-cost lookups.
// Falls back to a direct tmux query of the session's first window, addressed
// by ^ rather than index 0 so a user's base-index or pane-base-index cannot
// make the probe miss (and so read every dead pane as alive). Only the
// primary pane's line is read (see primaryPaneLine), so the other panes of a
// split window can neither mask nor fake its exit.
func (s *Session) IsPaneDead() bool {
	if info, ok := GetCachedPaneInfo(s.TmuxName()); ok {
		return info.Dead
	}
	// Cache miss: direct tmux check targeting the primary pane. Bound it the
	// same way Exists() bounds has-session — a wedged tmux server must not hang
	// this probe, since it runs under the notify-daemon's single-threaded poll
	// loop (via UpdateStatus → GetStatus) where a stall freezes all delivery.
	// A timed-out (indeterminate) probe is treated as "not dead": reporting a
	// live pane as dead would flip the session to an error state.
	ctx, cancel := context.WithTimeout(context.Background(), hasSessionProbeTimeout)
	defer cancel()
	out, err := commandOutput(s.tmuxCmdContext(ctx, "list-panes", "-t", "="+s.primaryWindowTarget(), "-F", "#{pane_dead}"))
	if err != nil {
		return false
	}
	return primaryPaneLine(string(out)) == "1"
}

// PaneDeadExitStatus returns the exit code of the process that ran in the
// session's primary pane, and true, but ONLY when the pane died while
// remain-on-exit was enabled so tmux still holds the dead pane and its exit
// status (#{pane_dead_status}).
//
// The second return is false whenever no exit code is available: the pane is
// still alive, the session is gone, or the pane was torn down without
// remain-on-exit (in which case tmux discards the exit status along with the
// pane, and #{pane_dead_status} comes back empty). Callers use the (code, ok)
// pair to distinguish a clean exit (0) from a crash (non-zero) instead of
// treating every terminated pane as an error.
func (s *Session) PaneDeadExitStatus() (int, bool) {
	// A session the listing shows absent has no pane to report on; skip the
	// fork that would only fail.
	if s.AbsentFromCache() {
		return 0, false
	}
	// Bounded like IsPaneDead: this runs on the notify-daemon poll loop, so a
	// wedged tmux server must not stall it. Targeted like IsPaneDead too: an
	// index-0 target misses under base-index 1, and every clean one-shot exit
	// would then be classified without its exit code.
	ctx, cancel := context.WithTimeout(context.Background(), hasSessionProbeTimeout)
	defer cancel()
	out, err := commandOutput(s.tmuxCmdContext(ctx, "list-panes", "-t", "="+s.primaryWindowTarget(), "-F", "#{pane_dead}|#{pane_dead_status}"))
	if err != nil {
		return 0, false
	}
	return parsePaneDeadStatus(string(out))
}

// parsePaneDeadStatus interprets the "#{pane_dead}|#{pane_dead_status}" line
// tmux emits for the primary pane, the first line of a window's list-panes
// result. It returns (code, true) only for a dead pane whose exit status is a
// parseable integer — i.e. one preserved by remain-on-exit. A live pane
// ("0|..."), or a dead pane with an empty status field (no remain-on-exit),
// yields (0, false). Pure so the parsing is unit-testable.
func parsePaneDeadStatus(raw string) (int, bool) {
	dead, status, ok := strings.Cut(primaryPaneLine(raw), "|")
	if !ok || dead != "1" {
		return 0, false // pane not dead → no meaningful exit status
	}
	code, err := strconv.Atoi(strings.TrimSpace(status))
	if err != nil {
		return 0, false // remain-on-exit off → pane_dead_status is empty
	}
	return code, true
}

// EnableMouseMode enables mouse scrolling, clipboard integration, and optimal settings
// Safe to call multiple times - just sets the options again
//
// Enables:
// - mouse on: Mouse wheel scrolling, text selection, pane resizing
// - set-clipboard on: OSC 52 clipboard integration (works with modern terminals)
// - allow-passthrough on: OSC 8 hyperlinks, advanced escape sequences (tmux 3.2+)
// - escape-time 10: Fast Vim/editor responsiveness (default 500ms is too slow)
//
// Terminal compatibility:
// - Warp, iTerm2, kitty, Alacritty, WezTerm: Full support (hyperlinks, clipboard, true color)
// - Windows Terminal, VS Code: Full support
// - Apple Terminal.app: Limited (no hyperlinks or clipboard)
//
// Note: With mouse mode on, hold Shift while selecting to use native terminal selection
// instead of tmux's selection (useful for copying to system clipboard in some terminals)
func (s *Session) EnableMouseMode() error {
	// #730: when the user opted out via [tmux].mouse = false, skip the mouse
	// set-option entirely so terminals like VS Code Linux keep click-drag
	// selection. Enhancements below are unaffected.
	if s.mouse {
		// CRITICAL: Mouse mode must succeed - keep as separate call for error handling
		// This is the only essential feature; all others are enhancements
		// Bounded — see tmuxMutationTimeout. The one option whose failure is
		// fatal, so it must not be the one that hangs forever.
		if err := s.runBoundedMutation("set-option", "-t", s.Name, "mouse", "on"); err != nil {
			return err
		}
	}

	// PERFORMANCE: Batch all non-fatal enhancements into single subprocess call
	// Uses tmux command chaining with \; separator (67% reduction in subprocess calls)
	// Before: 5 separate exec.Command calls = 5 subprocess spawns
	// After: 1 exec.Command call = 1 subprocess spawn
	//
	// Enhancements included:
	// - set-clipboard on: OSC 52 clipboard integration (Warp, iTerm2, kitty, etc.)
	// - allow-passthrough on: OSC 8 hyperlinks, advanced escape sequences (tmux 3.2+)
	// - extended-keys on: Forward Shift+Enter and other modified keys to apps (tmux 3.2+)
	// - extended-keys-format csi-u: Deliver them as ESC[13;2u (kitty form Claude Code reads), not xterm ESC[27;2;13~ (tmux 3.4+)
	// - terminal-features hyperlinks+extkeys: Track hyperlinks and enable extended key reporting (tmux 3.4+)
	// - escape-time 10: Fast Vim/editor responsiveness (default 500ms is too slow)
	//
	// Uses -q flag where supported to silently ignore on older tmux versions
	enhanceArgs := []string{
		"set-option", "-t", s.Name, "set-clipboard", "on", ";",
		"set-option", "-t", s.Name, "-q", "allow-passthrough", "on",
	}
	// #1625: gate the key-handling defaults through OptionOverrides so an explicit
	// user tmux setting wins (mirrors Start; see gatedTmuxKeyOptionArgs).
	enhanceArgs = append(enhanceArgs, gatedTmuxKeyOptionArgs(s.Name, s.OptionOverrides, s.configureTerminalFeatures)...)
	enhanceArgs = append(enhanceArgs, s.indicZeroWidthMarksArgs()...)
	enhanceCmd := s.tmuxCmd(enhanceArgs...)
	// Ignore errors - all these are non-fatal enhancements
	// Older tmux versions may not support some options
	_ = commandRun(enhanceCmd)

	return nil
}

// stableSessionTarget is the physical tmux runtime selected by a Session name.
// SessionID and PaneID are immutable for their tmux lifetimes; PanePID binds the
// process-tree proof to the process still installed in that pane.
type stableSessionTarget struct {
	SessionName string
	SessionID   string
	SocketName  string
	PaneID      string
	PanePID     int
}

var errStableSessionTargetChanged = errors.New("tmux: stable session target changed")
var errStableSessionMutationIndeterminate = errors.New("tmux: stable session mutation outcome is indeterminate")

const stableSessionTargetFormatFields = 4

var (
	captureStableSessionProcessTreeFn  = captureStableSessionProcessTree
	stableSessionConditionalMutationFn = func(ctx context.Context, socketName string, args ...string) ([]byte, error) {
		return tmuxExecContext(ctx, socketName, args...).Output()
	}
	probeSessionExistenceFn = probeSessionExistence
)

// captureStableSessionProcessTree resolves the mutable Session name once, then
// captures and revalidates the process tree exclusively through the immutable
// pane ID. The returned tmux IDs and retained process handles describe one
// physical runtime or the function fails closed.
func captureStableSessionProcessTree(session *Session) (stableSessionTarget, []ProcessIdentity, error) {
	if session == nil || session.Name == "" {
		return stableSessionTarget{}, nil, fmt.Errorf("tmux: invalid session target")
	}
	out, err := session.runBoundedOutput(
		"display-message", "-t", session.Name+":", "-p",
		tmuxFmt("#{session_id}", "#{session_name}", "#{pane_id}", "#{pane_pid}"),
	)
	if err != nil {
		return stableSessionTarget{}, nil, err
	}
	fields := strings.SplitN(strings.TrimSpace(string(out)), tmuxFieldSep, stableSessionTargetFormatFields)
	if len(fields) != stableSessionTargetFormatFields {
		return stableSessionTarget{}, nil, fmt.Errorf("tmux: malformed stable session target")
	}
	target := stableSessionTarget{
		SessionID:   strings.TrimSpace(fields[0]),
		SessionName: strings.TrimSpace(fields[1]),
		SocketName:  session.SocketName,
		PaneID:      strings.TrimSpace(fields[2]),
	}
	target.PanePID, err = strconv.Atoi(strings.TrimSpace(fields[3]))
	if target.SessionName != session.Name || !validTmuxStableID(target.SessionID, '$') ||
		!validTmuxStableID(target.PaneID, '%') || err != nil || target.PanePID <= 0 {
		return stableSessionTarget{}, nil, errStableSessionTargetChanged
	}

	identities, err := captureStableProcessTree(func() ([]int, error) {
		panePID, pids, probeErr := paneProcessTreeForPaneID(target.SocketName, target.PaneID)
		if probeErr != nil {
			return nil, probeErr
		}
		if panePID != target.PanePID {
			return nil, errStableSessionTargetChanged
		}
		return pids, nil
	}, target.PanePID, errStableSessionTargetChanged)
	if err != nil {
		return target, nil, err
	}
	return target, identities, nil
}

func stableSessionTargetCondition(target stableSessionTarget) (string, error) {
	return runtimeCandidateCondition([][2]string{
		{"session_id", target.SessionID},
		{"session_name", target.SessionName},
		{"pane_id", target.PaneID},
		{"pane_pid", strconv.Itoa(target.PanePID)},
	})
}

func stableSessionMutationCommand(commands ...[]string) string {
	parts := make([]string, 0, len(commands))
	for _, command := range commands {
		quoted := make([]string, len(command))
		for index, arg := range command {
			quoted[index] = tmuxQuote(arg)
		}
		parts = append(parts, strings.Join(quoted, " "))
	}
	return strings.Join(parts, " ; ")
}

// mutateStableSessionTarget executes the identity check and mutation in one
// tmux-server command queue. A reused Session name can never redirect the true
// branch because every destructive target is an immutable ID.
func mutateStableSessionTarget(target stableSessionTarget, commands ...[]string) error {
	condition, err := stableSessionTargetCondition(target)
	if err != nil {
		return err
	}
	const mismatchMessage = "agent-deck-stable-session-target-changed"
	ctx, cancel := context.WithTimeout(context.Background(), tmuxMutationTimeout)
	defer cancel()
	out, err := stableSessionConditionalMutationFn(ctx, target.SocketName,
		"if-shell", "-F", "-t", target.PaneID, condition,
		stableSessionMutationCommand(commands...),
		"display-message -p "+mismatchMessage,
	)
	if err != nil {
		return fmt.Errorf("%w: %w", errStableSessionMutationIndeterminate, annotateDeadline(ctx.Err(), err))
	}
	response := strings.TrimSpace(string(out))
	if response == mismatchMessage {
		return errStableSessionTargetChanged
	}
	if response != "" {
		return fmt.Errorf("tmux: unexpected stable-session conditional response %q", response)
	}
	return nil
}

type sessionExistence uint8

const (
	sessionExistenceIndeterminate sessionExistence = iota
	sessionExistencePresent
	sessionExistenceAbsent
)

// probeSessionExistence is deliberately tri-state. Only tmux's canonical
// no-session/no-server answers prove absence; launch, permission, socket,
// protocol, and timeout failures remain indeterminate. An exact-name target
// ("=name", see ProbeExactSession) is accepted: tmux names the session it
// cannot find without the "=".
func probeSessionExistence(socketName, target string) (sessionExistence, error) {
	ctx, cancel := context.WithTimeout(context.Background(), hasSessionProbeTimeout)
	defer cancel()
	_, err := tmuxExecContext(ctx, socketName, "has-session", "-t", target).Output()
	if err == nil {
		return sessionExistencePresent, nil
	}
	if ctx.Err() != nil {
		return sessionExistenceIndeterminate, annotateDeadline(ctx.Err(), err)
	}
	if isCanonicalMissingSessionResult(err, strings.TrimPrefix(target, "=")) {
		return sessionExistenceAbsent, nil
	}
	return sessionExistenceIndeterminate, err
}

func isCanonicalMissingSessionResult(err error, target string) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	stderr := strings.TrimSpace(strings.ToLower(string(exitErr.Stderr)))
	missingPrefix := "can't find session:"
	if strings.HasPrefix(stderr, missingPrefix) {
		return strings.TrimSpace(strings.TrimPrefix(stderr, missingPrefix)) == strings.ToLower(target)
	}
	return stderr == "no sessions" || strings.HasPrefix(stderr, "no server running on ")
}

// sessionAbsentAfterFailure preserves primary as the returned cause unless an
// explicit canonical probe proves the exact target is absent.
func sessionAbsentAfterFailure(socketName, target string, primary error) (bool, error) {
	state, probeErr := probeSessionExistenceFn(socketName, target)
	if state == sessionExistenceAbsent && probeErr == nil {
		return true, nil
	}
	if probeErr != nil {
		return false, fmt.Errorf("%w (session absence probe was indeterminate: %v)", primary, probeErr)
	}
	return false, primary
}

// Kill terminates the tmux session.
// Like RespawnPane, this captures the process tree first and ensures all
// processes actually die. tmux kill-session sends SIGHUP which some CLI
// tools (e.g. Claude Code 2.1.27+) ignore, leaving orphan processes.
func (s *Session) Kill() error {
	// Disconnect control mode pipe
	if pm := GetPipeManager(); pm != nil {
		pm.Disconnect(s.Name)
	}

	// Remove old log file if it exists (from pre-control-pipe era)
	logFile := s.LogFile()
	os.Remove(logFile) // Ignore errors

	// Capture retained process handles and prove the same tree is still rooted
	// in this pane before mutating tmux.
	target, oldIdentities, identityErr := captureStableSessionProcessTreeFn(s)
	if identityErr != nil {
		captureErr := fmt.Errorf("tmux: capture process identities before kill: %w", identityErr)
		probeTarget := s.Name
		if target.SessionID != "" {
			probeTarget = target.SessionID
		}
		if absent, resolvedErr := sessionAbsentAfterFailure(s.SocketName, probeTarget, captureErr); absent {
			return nil
		} else {
			return resolvedErr
		}
	}
	identitiesOwned := true
	defer func() {
		if identitiesOwned {
			CloseProcessIdentities(oldIdentities)
		}
	}()
	respawnLog.Info("pre_kill_process_tree", slog.String("session", logging.SanitizeValue(s.Name)), slog.Any("pids", processIdentityPIDs(oldIdentities)))

	// Kill the tmux session. Bounded — see tmuxMutationTimeout. If a client is
	// SIGKILLed at the deadline, only a canonical has-session answer proving the
	// immutable session ID absent resolves the ambiguous outcome as success.
	err := mutateStableSessionTarget(target, []string{"kill-session", "-t", target.SessionID})
	if err != nil {
		if !errors.Is(err, errStableSessionMutationIndeterminate) &&
			!errors.Is(err, errStableSessionTargetChanged) {
			return err
		}
		absent, resolvedErr := sessionAbsentAfterFailure(s.SocketName, target.SessionID, err)
		if !absent {
			return resolvedErr
		}
	}

	// Verify old processes are dead; escalate to SIGKILL if needed. No new
	// process exists on this path — the session is gone — so nothing is spared.
	// Transfer ownership only after the mutation succeeded or a canonical probe
	// proved the targeted session gone. A failed mutation against a still-live
	// session must never authorize auxiliary signals against its pane.
	if len(oldIdentities) > 0 {
		identitiesOwned = false
		go s.ensureProcessesDead(oldIdentities, nil)
	}

	// Killing a session that no longer exists is success, not failure: tmux
	// `kill-session` exits non-zero ("can't find session") for an already-dead
	// session. Treating that as fatal made archiveSession abort and silently
	// fail to persist the archive when re-archiving a session whose tmux was
	// already gone (the post-Unarchive path — Unarchive clears the flag without
	// restarting tmux). Only surface the error if the session is genuinely
	// still alive after the kill attempt.
	return nil
}

// PanePID returns the pane's initial process id, or an error when the probe is
// indeterminate.
//
// #1873: the ownership receipt is claimed from this pid the instant a spawn
// commits, so the caller needs the probe outcome, not a degraded 0. A 0 with no
// error would be recorded as "we own pid 0" or, worse, as "the spawn owns
// nothing" — both of which turn an unproven claim into a durable one.
func (s *Session) PanePID() (int, error) {
	return PanePIDOfSession(s.SocketName, s.TmuxName())
}

// PrimaryPanePID is PanePID for the managed agent window, whichever window
// is active.
func (s *Session) PrimaryPanePID() (int, error) {
	out, err := runBoundedOutput(s.SocketName, "list-panes", "-t", s.primaryWindowTarget(), "-F", "#{pane_pid}")
	return parsePanePID(out, err)
}

// AllPanePIDs lists the pane pid of every pane in every window of the
// session.
func (s *Session) AllPanePIDs() ([]int, error) {
	out, err := runBoundedOutput(s.SocketName, "list-panes", "-s", "-t", s.TmuxName(), "-F", "#{pane_pid}")
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return nil, err
	}
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if pid, convErr := strconv.Atoi(strings.TrimSpace(line)); convErr == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// PanePIDOfSession is the name-explicit variant, for callers holding a session
// name from somewhere other than a live Session — notably an ownership receipt,
// which records the tmux session its leader was launched into. An Instance's
// in-memory tmux name can drift away from the live session (that is its own
// bug class); the name written into the receipt at spawn cannot.
func PanePIDOfSession(socketName, sessionName string) (int, error) {
	if strings.TrimSpace(sessionName) == "" {
		return 0, fmt.Errorf("no tmux session name")
	}
	out, err := runBoundedOutput(socketName, "list-panes", "-t", sessionName+":", "-F", "#{pane_pid}")
	return parsePanePID(out, err)
}

// paneProcessTree returns the pane process tree with probe failures preserved.
//
// The distinction matters on exactly one path: the post-respawn probe in
// RespawnPane. Its result feeds the `pid == newPanePID` guard in
// ensureProcessesDead, and a degraded panePID of 0 matches no real process — so
// an indeterminate probe silently disengages the guard and the escalation can
// SIGTERM->SIGKILL the process the user just restarted. See
// escalateAfterRespawn.
func (s *Session) paneProcessTree() (panePID int, allPIDs []int, err error) {
	return paneProcessTreeForTarget(s.SocketName, s.TmuxName()+":")
}

// paneProcessTreeForTarget captures the first pane's process tree for a tmux
// session or window target, preserving the historical Session behavior.
func paneProcessTreeForTarget(socketName, target string) (panePID int, allPIDs []int, err error) {
	// Bounded — see tmuxPollTimeout. Runs on the respawn path: a hang here
	// stalls the restart that is supposed to clear the bad state.
	out, err := runBoundedOutput(socketName, "list-panes", "-t", target, "-F", "#{pane_pid}")
	panePID, err = parsePanePID(out, err)
	if err != nil {
		return 0, nil, err
	}
	pids, err := processTreeFromPanePID(panePID)
	return panePID, pids, err
}

// paneProcessTreeForPaneID resolves the process tree through an immutable pane
// ID. display-message targets that exact pane; list-panes would instead expand
// a pane target to every pane in its window and could capture the wrong root.
func paneProcessTreeForPaneID(socketName, paneID string) (panePID int, allPIDs []int, err error) {
	out, err := runBoundedOutput(socketName, "display-message", "-t", paneID, "-p", "#{pane_pid}")
	panePID, err = parsePanePID(out, err)
	if err != nil {
		return 0, nil, err
	}
	pids, err := processTreeFromPanePID(panePID)
	return panePID, pids, err
}

func processTreeFromPanePID(panePID int) ([]int, error) {
	// Collect the pane PID plus all descendants via pgrep -P (recursive).
	allPIDs := []int{panePID}
	queue := []int{panePID}
	seen := map[int]struct{}{panePID: {}}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		pgrepOut, err := commandOutput(exec.Command("pgrep", "-P", strconv.Itoa(parent)))
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
				continue // pgrep's documented "no matching processes" result.
			}
			return nil, fmt.Errorf("enumerate children of pid %d: %w", parent, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(pgrepOut)), "\n") {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(line))
			if parseErr != nil || pid <= 0 {
				return nil, fmt.Errorf("parse child pid %q for parent %d", line, parent)
			}
			if _, duplicate := seen[pid]; duplicate {
				continue
			}
			seen[pid] = struct{}{}
			allPIDs = append(allPIDs, pid)
			queue = append(queue, pid)
		}
	}
	return allPIDs, nil
}

// parsePanePID turns a `list-panes -F #{pane_pid}` result into the pane's PID.
//
// It honors the WaitDelay contract documented at tmuxSubprocessWaitDelay: when
// cmd.Wait abandons the I/O goroutine mid-read (exec.ErrWaitDelay), the bytes
// the subprocess already wrote are still in the buffer, so a body that parses
// cleanly is authoritative and the error is not. That case is the norm, not an
// edge, under the bridged-stdio setups WaitDelay exists for — Claude Code
// /remote-control, ssh ControlMaster — where treating it as failure would make
// every probe indeterminate and permanently skip the reap.
//
// Everything else is indeterminate: a client killed at its deadline, a missing
// session, an empty or garbled body. None of them may be reported as "the pane
// has no process", because callers answer an empty tree by skipping the
// SIGTERM->SIGKILL escalation.
func parsePanePID(out []byte, err error) (int, error) {
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return 0, err
	}
	// Take only the first line (handles multi-pane sessions safely)
	pidStr := strings.TrimSpace(string(out))
	if idx := strings.IndexByte(pidStr, '\n'); idx >= 0 {
		pidStr = strings.TrimSpace(pidStr[:idx])
	}
	pid, convErr := strconv.Atoi(pidStr)
	if convErr != nil || pid <= 0 {
		if err != nil {
			return 0, fmt.Errorf("no usable pane_pid in %q: %w", pidStr, err)
		}
		return 0, fmt.Errorf("unparseable pane_pid %q", pidStr)
	}
	return pid, nil
}

// ensureProcessesDead reaps identities captured before a tmux mutation. On
// Linux it escalates from SIGTERM to SIGKILL through the same retained pidfd
// used for the final pre-mutation liveness proof.
//
// newPIDs is the process tree the respawn just created, and is excluded from
// the escalation as an additional conservative guard. The retained process
// handle is the authoritative PID-reuse defense; a command-name allowlist is
// not.
func (s *Session) ensureProcessesDead(oldIdentities []ProcessIdentity, newPIDs []int) {
	if len(oldIdentities) == 0 {
		return
	}

	spared := make(map[int]struct{}, len(newPIDs))
	for _, pid := range newPIDs {
		spared[pid] = struct{}{}
	}
	reap := make([]ProcessIdentity, 0, len(oldIdentities))
	for _, identity := range oldIdentities {
		// Never escalate against anything the respawn just created
		if _, isNew := spared[identity.PID]; isNew {
			_ = identity.Close()
			continue
		}
		reap = append(reap, identity)
	}
	if err := ReapProcessIdentities(reap, ProcessReapTiming{
		InitialGrace: 500 * time.Millisecond,
		TermGrace:    time.Second,
	}); err != nil {
		respawnLog.Warn("respawn_process_reap_failed", slog.Any("error", err))
	}
}

// respawnPanePIDRetryDelay is how long escalateAfterRespawn waits before
// re-probing a pane PID the first probe could not resolve. One deadline's worth
// of grace: the client that ate the first probe is SIGKILLed at
// tmuxPollTimeout, and its replacement talks to a server that is not itself
// wedged (the 2026-07-21 leak is per-client), so the retry usually lands.
const respawnPanePIDRetryDelay = 500 * time.Millisecond

// escalateAfterRespawn runs the post-respawn SIGTERM->SIGKILL escalation that
// reaps agents ignoring tmux's SIGHUP (Claude Code 2.1.27+).
//
// It exists to keep one specific mistake from being possible: treating an
// INDETERMINATE pane-PID probe as a resolved one. ensureProcessesDead's only
// protection for the freshly respawned tree is that its PIDs are excluded, and
// the bounded probe degrades to an empty tree on timeout. Pass that through and
// the guard is off; if tmux handed the new pane a PID from the pre-respawn tree
// (the reuse case the guard was written for), the escalation kills the process
// the user just restarted.
//
// So a probe failure is retried once, off RespawnPane's critical path, and if
// it still cannot be resolved the escalation is skipped entirely.
//
// That trade is deliberate and asymmetric, but be clear about its cost: the
// skipped escalation LEAKS a SIGHUP-immune survivor permanently. Nothing else
// reaps it — reapOrphanedPollClients only kills processes whose comm is "tmux",
// and after `respawn-pane -k` the survivor is reparented out of the pane, so no
// later getPaneProcessTree walk will ever include it. It needs a manual kill,
// which is why the skip logs at Warn. We accept a leak that is logged and
// visible in `ps` over destroying live work on a guess, with no warning and no
// recovery.
func (s *Session) escalateAfterRespawn(oldIdentities []ProcessIdentity, newPIDs []int, probeErr error) {
	s.escalateAfterRespawnTarget(oldIdentities, newPIDs, probeErr, func() ([]int, error) {
		_, pids, err := s.paneProcessTree()
		return pids, err
	})
}

// escalateAfterRespawnTarget is the escalation core every respawn path runs
// (escalateAfterRespawn documents its policy). It is probe-agnostic: it never
// resolves the pane itself, and a failed first probe is retried through the
// caller-supplied reprobe. Each caller passes the probe that produced newPIDs,
// so the retry resolves the respawned pane exactly as the first probe did.
// Session.RespawnPane and RespawnRuntimeGenerationCandidate pass their probe
// of the immutable pane ID they respawned; escalateAfterRespawn passes
// upstream's by-name probe (s.paneProcessTree).
func (s *Session) escalateAfterRespawnTarget(oldIdentities []ProcessIdentity, newPIDs []int, probeErr error, reprobe func() ([]int, error)) {
	if len(oldIdentities) == 0 {
		return
	}

	if probeErr != nil {
		time.Sleep(respawnPanePIDRetryDelay)
		retryPIDs, retryErr := reprobe()
		if retryErr != nil {
			respawnLog.Warn("respawn_escalation_skipped_unknown_pane_pid",
				slog.String("session", s.Name),
				slog.Any("old_processes", oldIdentities),
				slog.String("probe_error", probeErr.Error()),
				slog.String("retry_error", retryErr.Error()))
			CloseProcessIdentities(oldIdentities)
			return
		}
		newPIDs = retryPIDs
	}

	s.ensureProcessesDead(oldIdentities, newPIDs)
}

// RespawnPane kills the current process in the pane and starts a new command.
// This is more reliable than sending Ctrl+C and waiting for shell prompt.
// The -k flag kills the current process before respawning.
//
// IMPORTANT: After respawn, this function verifies that old processes actually
// died. Some CLI tools (notably Claude Code 2.1.27+) ignore SIGHUP sent by
// tmux respawn-pane, leaving orphan processes that consume CPU indefinitely.
// If old processes survive, we escalate through SIGTERM → SIGKILL.
func (s *Session) RespawnPane(command string) error {
	s.invalidateCache()

	// Capture retained process handles, then prove the exact same tree remains
	// rooted in the pane before respawning it.
	target, oldIdentities, identityErr := captureStableSessionProcessTreeFn(s)
	if identityErr != nil {
		captureErr := fmt.Errorf("tmux: capture process identities before respawn: %w", identityErr)
		probeTarget := s.Name
		if target.SessionID != "" {
			probeTarget = target.SessionID
		}
		if absent, resolvedErr := sessionAbsentAfterFailure(s.SocketName, probeTarget, captureErr); absent {
			return fmt.Errorf("session does not exist: %s", s.Name)
		} else {
			return resolvedErr
		}
	}
	respawnLog.Info("pre_respawn_process_tree", slog.Any("pids", processIdentityPIDs(oldIdentities)))
	identitiesOwned := true
	defer func() {
		if identitiesOwned {
			CloseProcessIdentities(oldIdentities)
		}
	}()

	// Build respawn-pane command
	// -k: Kill current process
	// -t: Target the immutable pane ID captured with the process proof.
	// command: New command to run
	args := []string{"respawn-pane", "-k", "-t", target.PaneID}
	if command != "" {
		wrapped, wrapErr := wrapRespawnCommand(command)
		if wrapErr != nil {
			return wrapErr
		}
		// Deliver as separate argv tokens (bash, -lc, command) so tmux
		// execvp()s bash directly as the new pane leader instead of routing
		// the restart command through the server default-shell. Same #1567 /
		// #1580 rationale as the initial-process spawn in startCommandSpec.
		args = append(args, wrapped...)
	}

	mutations := make([][]string, 0, 2)
	if s.clearOnRestart {
		mutations = append(mutations, []string{"clear-history", "-t", target.PaneID})
	}
	mutations = append(mutations, args)
	// Serialize the conditional pane replacement and startup-clock publication
	// with expireStartupHandover so neither can expire a newer process.
	s.mu.Lock()
	// LOC-4: the respawn command carries the session's env prelude; log the
	// variable names only, never their values.
	if mcpLog.Enabled(context.Background(), slog.LevelDebug) {
		redacted := make([][]string, len(mutations))
		for idx, mutation := range mutations {
			redacted[idx] = logging.RedactCommandEnvArgs(mutation)
		}
		mcpLog.Debug("respawn_pane_executing", slog.Any("args", redacted))
	}
	if err := mutateStableSessionTarget(target, mutations...); err != nil {
		s.mu.Unlock()
		mcpLog.Debug("respawn_pane_error", slog.String("error", err.Error()))
		return fmt.Errorf("failed to respawn pane: %w", err)
	}
	s.startupAt = time.Now()
	s.startupTimedOut = false
	s.lastStableStatus = "waiting"
	s.stateTracker = nil
	s.cachedPromptDetector = nil
	s.cachedPromptDetectorTool = ""
	s.mu.Unlock()
	if s.clearOnRestart {
		respawnLog.Info("cleared_scrollback", slog.String("session", s.Name))
	}

	// Capture the NEW process tree so we don't accidentally kill anything the
	// respawn just created. Keep the probe error: "could not tell" must not be
	// spent as an empty tree, which would silently disable that very guard
	// (see escalateAfterRespawn). The escalation retries with this same probe.
	probeNewTree := func() ([]int, error) {
		_, pids, err := paneProcessTreeForPaneID(s.SocketName, target.PaneID)
		return pids, err
	}
	newPIDs, newTreeErr := probeNewTree()

	// Verify old processes are dead; escalate to SIGKILL if needed
	// Run in background so RespawnPane returns quickly
	identitiesOwned = false
	go s.escalateAfterRespawnTarget(oldIdentities, newPIDs, newTreeErr, probeNewTree)

	// A control-mode client is attached to the session rather than the pane
	// process; reconnecting via a mutable name could attach to a replacement.

	return nil
}

// wrapRespawnCommand returns the argv tokens ({bash, -lc, command}) that
// respawn-pane appends so tmux execvp()s bash directly as the new pane leader
// rather than routing the command through the server default-shell. See the
// #1567 / #1580 fix rationale in startCommandSpec.
func wrapRespawnCommand(command string) ([]string, error) {
	return wrapRespawnCommandWithResolver(command, exec.LookPath)
}

func wrapRespawnCommandWithResolver(command string, lookPath func(string) (string, error)) ([]string, error) {
	bashPath, err := lookPath(bashBinary)
	if err != nil {
		return nil, fmt.Errorf("bash not found in PATH: %w", err)
	}
	return []string{bashPath, "-lc", command}, nil
}

// DetectTool detects which AI coding tool is running in the session
// Uses caching to avoid re-detection on every call
func (s *Session) DetectTool() string {
	// Check cache first without probing processes on the status hot path.
	s.mu.Lock()
	if s.detectedTool != "" && time.Since(s.toolDetectedAt) < s.toolDetectExpiry {
		tool := s.detectedTool
		s.mu.Unlock()
		return tool
	}
	s.mu.Unlock()

	// If a custom tool name is set, return it directly.
	// Custom tools have their underlying command detected at creation time;
	// runtime detection should preserve the custom name.
	s.mu.Lock()
	if s.customToolName != "" {
		name := s.customToolName
		s.detectedTool = name
		s.toolDetectedAt = time.Now()
		s.mu.Unlock()
		return name
	}
	s.mu.Unlock()

	// Detect tool from command first (most reliable)
	if tool := detectToolFromCommand(s.launchCommand()); tool != "" {
		// Configured launch identity. This is what a restart should run; it is
		// not an observation of the pane, and a nested process must not replace it.
		s.rememberTool(tool)
		return tool
	}

	// A foreground sample can be a child command of an already recognized
	// agent. Preserve that runtime until an explicit ForceDetectTool instead
	// of relabeling it from a single child-process sample.
	s.mu.Lock()
	if previous := s.detectedTool; previous != "" && previous != "shell" {
		s.toolDetectedAt = time.Now()
		s.mu.Unlock()
		return previous
	}
	s.mu.Unlock()

	// A session created as "shell" can later launch a supported tool. Only
	// promote it when the pane's current command identifies that tool.
	if paneInfo, ok := GetCachedPaneInfo(s.TmuxName()); ok {
		if tool := detectToolFromCommand(paneInfo.CurrentCommand); tool != "" {
			s.rememberTool(tool)
			return tool
		}
	}

	// Screen text is not evidence of process identity. Shell output, editor
	// buffers, and agent conversations can all contain another tool's banner
	// or vendor name. In particular, an agent inside a Neovim terminal leaves
	// nvim as tmux's foreground command. Without a process tree, failing closed
	// to shell is safer than persisting an unrelated tool.
	s.rememberTool("shell")
	return "shell"
}

// rememberTool caches command-based detection without changing launch identity.
func (s *Session) rememberTool(tool string) {
	s.mu.Lock()
	s.detectedTool = tool
	s.toolDetectedAt = time.Now()
	s.mu.Unlock()
}

// ForceDetectTool forces a re-detection of the tool, ignoring cache
func (s *Session) ForceDetectTool() string {
	s.mu.Lock()
	s.detectedTool = ""
	s.toolDetectedAt = time.Time{}
	s.mu.Unlock()
	return s.DetectTool()
}

// GetWorkDir returns the current working directory of the tmux pane
// This is the live directory from the pane, not the initial WorkDir
func (s *Session) GetWorkDir() string {
	if !s.Exists() {
		return ""
	}

	// Bounded: a wedged server / destroyed target must not hang this poll (see
	// tmuxPollTimeout). Bare .Output() here was one of the orphan-spin sources.
	output, err := s.runBoundedOutput("display-message", "-t", s.TmuxName(), "-p", "#{pane_current_path}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// IsAltScreen reports whether the pane is currently showing the alternate
// screen — i.e. a full-screen TUI such as Claude in fullscreen mode. Such panes
// keep NO tmux scrollback (history_size stays 0 regardless of history-limit), so
// the in-attach scrollback pager (#1491) has nothing to capture and, worse,
// would swallow the PageUp the app itself uses to scroll. The attach loop
// consults this (via AttachOptions.ScrollbackGate) to leave bare PageUp for the
// app when it is fullscreen.
//
// Bounded so a wedged server can never hang the keystroke that triggered the
// query. On error it returns (false, err); the caller preserves the pager on
// error rather than silently disabling a configured feature.
func (s *Session) IsAltScreen() (bool, error) {
	output, err := s.runBoundedOutput("display-message", "-t", s.TmuxName(), "-p", "#{alternate_on}")
	if err != nil {
		return false, err
	}
	return parseAlternateOn(string(output)), nil
}

// parseAlternateOn parses tmux's #{alternate_on} flag: an exact "1" means the
// alternate screen is active, anything else means the normal screen. tmux
// appends a trailing newline, so surrounding whitespace is trimmed. Extracted so
// the parsing is unit-testable without a live tmux server.
func parseAlternateOn(output string) bool {
	return strings.TrimSpace(output) == "1"
}
