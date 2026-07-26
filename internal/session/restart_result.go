package session

import (
	"errors"
	"fmt"
)

// RestartPartialSuccessError means the replacement pane/runtime is already
// live, but persisting its runtime generation failed. Retrying or rolling back
// the restart is destructive; callers should keep the completed runtime and
// surface this as a durability warning.
type RestartPartialSuccessError struct {
	InstanceID string
	Err        error
}

func (e *RestartPartialSuccessError) Error() string {
	return fmt.Sprintf("restart completed for %s but runtime generation persistence failed: %v", e.InstanceID, e.Err)
}

func (e *RestartPartialSuccessError) Unwrap() error { return e.Err }

func (e *RestartPartialSuccessError) RestartCompleted() bool { return true }

// IsRestartPartialSuccess recognizes both the production error and compatible
// wrappers used by orchestration layers/tests without coupling them to its
// concrete type.
func IsRestartPartialSuccess(err error) bool {
	if err == nil {
		return false
	}
	var completed interface{ RestartCompleted() bool }
	return errors.As(err, &completed) && completed.RestartCompleted()
}
