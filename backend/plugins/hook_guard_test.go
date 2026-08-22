package plugins

import (
	"errors"
	"testing"
	"time"
)

func TestCallHookReturnsValueAndPassesPluginErrorsThrough(t *testing.T) {
	wantErr := errors.New("plugin logic failed")
	value, err := CallHook(time.Second, func() (int, error) {
		return 7, wantErr
	})
	if value != 7 || !errors.Is(err, wantErr) {
		t.Fatalf("CallHook() = %d, %v", value, err)
	}
}

func TestCallHookConvertsPanicToGuardFailure(t *testing.T) {
	_, err := CallHook(time.Second, func() (string, error) {
		panic("boom")
	})
	if !IsHookGuardFailure(err) || !errors.Is(err, ErrHookPanic) {
		t.Fatalf("panic not converted: %v", err)
	}
}

func TestCallHookTimesOutAndAbandonsSlowPlugin(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	started := time.Now()
	_, err := CallHook(20*time.Millisecond, func() (int, error) {
		<-release
		return 1, nil
	})
	if !errors.Is(err, ErrHookTimeout) {
		t.Fatalf("slow hook error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("guard waited %s for a slow plugin", elapsed)
	}
}
