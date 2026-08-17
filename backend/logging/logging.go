// File overview: Minimal leveled logging on top of the standard log package.
// Debugf lines are suppressed unless ROLLTOP_LOG_LEVEL=debug, so production
// logs carry only operational messages while development keeps the verbose
// plugin and unsubscribe traces.

package logging

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
)

var (
	mu           sync.RWMutex
	debugEnabled *bool
)

// DebugEnabled reports whether debug-level lines should be written. The first
// call reads ROLLTOP_LOG_LEVEL from the environment; SetDebug overrides it.
func DebugEnabled() bool {
	mu.RLock()
	if debugEnabled != nil {
		enabled := *debugEnabled
		mu.RUnlock()
		return enabled
	}
	mu.RUnlock()
	enabled := strings.EqualFold(strings.TrimSpace(os.Getenv("ROLLTOP_LOG_LEVEL")), "debug")
	mu.Lock()
	if debugEnabled == nil {
		debugEnabled = &enabled
	}
	enabled = *debugEnabled
	mu.Unlock()
	return enabled
}

// SetDebug forces debug logging on or off, overriding the environment. Tests
// use it to exercise both levels without mutating process environment state.
func SetDebug(enabled bool) {
	mu.Lock()
	debugEnabled = &enabled
	mu.Unlock()
}

// Debugf logs like log.Printf with a "debug " prefix, but only when debug
// logging is enabled. Line separators in the rendered message are escaped so
// values taken from mail content or URLs cannot forge extra log records.
// Arguments are evaluated eagerly even when debug is off, so keep expensive
// arguments behind a DebugEnabled() guard on hot paths.
func Debugf(format string, args ...any) {
	if !DebugEnabled() {
		return
	}
	message := fmt.Sprintf(format, args...)
	if strings.ContainsAny(message, "\r\n") {
		message = strings.NewReplacer("\r", `\r`, "\n", `\n`).Replace(message)
	}
	log.Printf("debug %s", message)
}
