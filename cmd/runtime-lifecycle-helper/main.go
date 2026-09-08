//go:build runtime_lifecycle_helper

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

const barrierEnv = "AGENTDECK_RUNTIME_HELPER_BARRIER"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: runtime-lifecycle-helper MODE DB [ARGS...]")
	}
	mode, path := args[0], args[1]
	db, err := statedb.Open(path)
	if err != nil {
		return err
	}
	defer db.Close()

	if mode == "migrate" {
		if err := waitForRelease(); err != nil {
			return err
		}
		if err := db.Migrate(); err != nil {
			if isSQLiteBusy(err) {
				fmt.Println("busy")
				return nil
			}
			return err
		}
		fmt.Println("ok")
		return nil
	}
	if os.Getenv("AGENTDECK_RUNTIME_HELPER_BUSY_TIMEOUT") == "0" {
		db.DB().SetMaxOpenConns(1)
		if _, err := db.DB().Exec("PRAGMA busy_timeout=0"); err != nil {
			return err
		}
	}
	if os.Getenv("AGENTDECK_RUNTIME_HELPER_RETRY_BARRIER") == "1" {
		slog.SetDefault(slog.New(&retryBarrierHandler{
			Handler: slog.NewTextHandler(os.Stderr, nil),
		}))
	}
	if mode == "lock" {
		return holdWriteLock(db)
	}
	incarnation := ""
	if mode == "transition" || mode == "binding" || mode == "status" {
		row, err := db.LoadInstanceByID("one")
		if err != nil {
			return err
		}
		if row == nil || row.Incarnation == "" {
			return statedb.ErrInstanceParentConflict
		}
		incarnation = row.Incarnation
	}
	if err := waitForRelease(); err != nil {
		return err
	}

	switch mode {
	case "transition":
		if len(args) != 4 {
			return errors.New("transition requires EXPECTED TMUX")
		}
		expected, err := parseUint(args[2])
		if err != nil {
			return err
		}
		err = db.CommitRuntimeTransition(expected, incarnation, statedb.RuntimeState{
			InstanceID: "one", Generation: expected + 1,
			TmuxSession: args[3], TmuxSocketName: "isolated",
			Status: "starting", LastStartedAt: time.Unix(200, 0).UTC(),
		})
		if errors.Is(err, statedb.ErrRuntimeGenerationConflict) {
			fmt.Println("conflict")
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Println("won:" + args[3])
		return nil

	case "binding":
		if len(args) != 5 {
			return errors.New("binding requires GENERATION REVISION VALUE")
		}
		generation, err := parseUint(args[2])
		if err != nil {
			return err
		}
		revision, err := parseUint(args[3])
		if err != nil {
			return err
		}
		_, applied, err := db.WriteRuntimeBindingIfVersion(
			"one", incarnation, generation, "claude", revision, args[4], time.Unix(300, 0).UTC())
		if err != nil {
			return err
		}
		if !applied {
			fmt.Println("conflict")
			return nil
		}
		fmt.Println("won:" + args[4])
		return nil

	case "status":
		if len(args) != 5 {
			return errors.New("status requires GENERATION REVISION VALUE")
		}
		generation, err := parseUint(args[2])
		if err != nil {
			return err
		}
		revision, err := parseUint(args[3])
		if err != nil {
			return err
		}
		applied, err := db.WriteStatusIfVersion("one", incarnation, generation, revision, args[4])
		if err != nil {
			return err
		}
		if !applied {
			fmt.Println("conflict")
			return nil
		}
		fmt.Println("won:" + args[4])
		return nil

	case "stale-save":
		if len(args) != 2 {
			return errors.New("stale-save takes no extra arguments")
		}
		if err := db.UpsertInstances([]*statedb.InstanceRow{{
			ID: "one", Title: "metadata-won", ProjectPath: "/tmp/one",
			GroupPath: "my-sessions", Tool: "claude", Status: "stale",
			TmuxSession: "stale-tmux", CreatedAt: time.Unix(1, 0).UTC(),
			ToolData: []byte(`{"claude_session_id":"stale-binding","notes":"metadata-won"}`),
		}}); err != nil {
			return err
		}
		fmt.Println("metadata")
		return nil
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}
}

type retryBarrierHandler struct {
	slog.Handler
	once sync.Once
	err  error
}

func (h *retryBarrierHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "statedb: SQLITE_BUSY retry" {
		h.once.Do(func() { h.err = waitForRetryRelease() })
		if h.err != nil {
			return h.err
		}
	}
	return h.Handler.Handle(ctx, record)
}

func isSQLiteBusy(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "sqlite_busy") || strings.Contains(message, "database is locked")
}

func parseUint(value string) (uint64, error) {
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", value, err)
	}
	return parsed, nil
}

func waitForRelease() error {
	if os.Getenv(barrierEnv) != "1" {
		return nil
	}
	return waitForBarrier(3, 4, "runtime-helper")
}

func waitForRetryRelease() error {
	return waitForBarrier(5, 6, "runtime-helper-retry")
}

func waitForBarrier(readyFD, releaseFD uintptr, name string) error {
	ready := os.NewFile(readyFD, name+"-ready")
	release := os.NewFile(releaseFD, name+"-release")
	if ready == nil || release == nil {
		return errors.New("runtime helper barrier is absent")
	}
	defer ready.Close()
	defer release.Close()
	if _, err := ready.Write([]byte{1}); err != nil {
		return err
	}
	var signal [1]byte
	_, err := io.ReadFull(release, signal[:])
	return err
}

func holdWriteLock(db *statedb.StateDB) error {
	ctx := context.Background()
	conn, err := db.DB().Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if err := waitForRelease(); err != nil {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return err
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		return err
	}
	fmt.Println("unlocked")
	return nil
}
