// File overview: Panic and soft-timeout guards for in-process backend plugin
// hook calls on the sync/indexing pipeline.

package plugins

import (
	"errors"
	"fmt"
	"time"
)

// ErrHookTimeout reports that a plugin hook did not return within its budget.
// In-process code cannot be preempted, so the calling pipeline abandons the
// result (the stuck goroutine leaks until the plugin call eventually returns)
// rather than wedging mail sync behind a misbehaving plugin.
var ErrHookTimeout = errors.New("plugin hook timed out")

// ErrHookPanic reports that a plugin hook panicked. The panic is converted to
// an error so one broken plugin cannot crash the whole process; the panic value
// is deliberately not propagated because it can contain message-derived content.
var ErrHookPanic = errors.New("plugin hook panicked")

// IsHookGuardFailure reports whether an error came from the guard itself rather
// than from normal plugin logic. Callers use this to skip a plugin without
// failing the surrounding sync operation.
func IsHookGuardFailure(err error) bool {
	return errors.Is(err, ErrHookTimeout) || errors.Is(err, ErrHookPanic)
}

// CallHook runs one plugin invocation with panic recovery and a wall-clock
// budget. The returned error wraps ErrHookTimeout or ErrHookPanic when the
// guard fired; plugin-returned errors pass through unchanged.
func CallHook[T any](timeout time.Duration, call func() (T, error)) (T, error) {
	type outcome struct {
		value T
		err   error
	}
	ch := make(chan outcome, 1)
	go func() {
		var zero T
		defer func() {
			if r := recover(); r != nil {
				ch <- outcome{value: zero, err: fmt.Errorf("%w: recovered from plugin panic", ErrHookPanic)}
			}
		}()
		value, err := call()
		ch <- outcome{value: value, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		return res.value, res.err
	case <-timer.C:
		var zero T
		return zero, fmt.Errorf("%w after %s", ErrHookTimeout, timeout)
	}
}
