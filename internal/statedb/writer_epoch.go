package statedb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

var ErrIncompatibleWriterSchema = errors.New("statedb: incompatible live writer schema")

type writerEpochQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

var readWriterProcessStartToken = tmux.ReadProcessStartToken

func (s *StateDB) currentWriterProcessStartToken() (string, error) {
	token, err := durableWriterProcessStartToken(s.pid)
	if err != nil {
		return "", fmt.Errorf("statedb: capture current writer process identity: %w", err)
	}
	return token, nil
}

// durableWriterProcessStartToken qualifies Linux's boot-relative process
// start tick with boot_id so a persisted row cannot match after a reboot.
// Darwin's kernel start token is already an epoch timestamp.
func durableWriterProcessStartToken(pid int) (string, error) {
	startToken, err := readWriterProcessStartToken(pid)
	if err != nil {
		return "", err
	}
	bootID := ""
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
		if err != nil {
			return "", fmt.Errorf("read Linux boot ID: %w", err)
		}
		bootID = strings.TrimSpace(string(data))
	}
	return qualifyWriterProcessStartToken(runtime.GOOS, bootID, startToken)
}

func qualifyWriterProcessStartToken(goos, bootID, startToken string) (string, error) {
	if goos != "linux" {
		return startToken, nil
	}
	if bootID == "" {
		return "", errors.New("empty Linux boot ID")
	}
	return bootID + ":" + startToken, nil
}

// RequireRuntimeWriterCompatibility refuses runtime mutations while any live
// writer from another schema epoch remains registered.
func (s *StateDB) RequireRuntimeWriterCompatibility() error {
	return s.requireRuntimeWriterCompatibility(s.db, time.Now())
}

func (s *StateDB) requireRuntimeWriterCompatibility(q writerEpochQueryer, _ time.Time) error {
	rows, err := q.QueryContext(context.Background(), `
		SELECT pid, writer_schema_version, writer_process_start_token
		FROM instance_heartbeats
		ORDER BY pid`)
	if err != nil {
		return fmt.Errorf("statedb: inspect writer schemas: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var pid, version int
		var startToken string
		if err := rows.Scan(&pid, &version, &startToken); err != nil {
			return fmt.Errorf("statedb: scan writer schema: %w", err)
		}
		if version == SchemaVersion {
			continue
		}
		// A process executing this code proves that any pre-registration row
		// from an older epoch with its numeric PID belongs to an earlier process
		// lifetime.
		if pid == s.pid {
			if version < SchemaVersion {
				continue
			}
			return fmt.Errorf("%w: pid=%d writer=%d required=%d", ErrIncompatibleWriterSchema, pid, version, SchemaVersion)
		}

		live, probeErr := incompatibleWriterStillLive(pid, startToken)
		if probeErr != nil {
			return fmt.Errorf("%w: pid=%d writer=%d required=%d: %v",
				ErrIncompatibleWriterSchema, pid, version, SchemaVersion, probeErr)
		}
		if live {
			return fmt.Errorf("%w: pid=%d writer=%d required=%d", ErrIncompatibleWriterSchema, pid, version, SchemaVersion)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("statedb: iterate writer schemas: %w", err)
	}
	return nil
}

func incompatibleWriterStillLive(pid int, recordedStartToken string) (bool, error) {
	if recordedStartToken != "" {
		startToken, err := durableWriterProcessStartToken(pid)
		if err == nil {
			return startToken == recordedStartToken, nil
		}
		alive, liveErr := writerPIDAlive(pid)
		if liveErr != nil {
			return false, fmt.Errorf("inspect writer pid %d after identity probe failed: %w", pid, liveErr)
		}
		if !alive {
			return false, nil
		}
		return false, fmt.Errorf("inspect writer pid %d identity: %w", pid, err)
	}
	return writerPIDAlive(pid)
}

func writerPIDAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, err
	}
	err = process.Signal(syscall.Signal(0))
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, os.ErrProcessDone), errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, err
	}
}
