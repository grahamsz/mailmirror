// File overview: Persistent crash reporting. Runtime crash dumps (panics, fatal
// runtime errors) and orderly fatal errors are appended to <data-dir>/crash.log
// so they survive container recreation even when stderr output is lost. A state
// file records how far the log had grown when the run started and whether the
// run ended cleanly, which lets the next start tell three cases apart: a clean
// exit, a run that reported why it died, and a run killed before it could write
// anything (SIGKILL, kernel OOM kill).
//
// Growth of the crash log is what identifies a reported failure, not the clean
// shutdown flag. An unrecovered panic runs deferred functions first and only
// then writes its dump, so by the time the runtime reports, this process has
// already recorded whatever it believed about its own exit. Comparing sizes
// catches the dump that arrives afterwards; recovering the panic to correct the
// flag would rewrite the stack trace this file exists to preserve.

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"
)

const (
	crashLogName     = "crash.log"
	crashLogPrevName = "crash.log.prev"
	crashStateName   = "crash-state.json"

	// crashLogMaxBytes bounds the append-only log. Only failures are written,
	// so this holds many incidents before the oldest are rotated away.
	crashLogMaxBytes = 1 << 20
)

// runState describes the run that wrote it. CrashLogBytes is the crash log size
// once the run had announced itself, so any later growth is a report that run
// produced. CleanShutdown records whether it got to end on its own terms. The
// file is rewritten rather than deleted: the size baseline has to outlive a
// clean shutdown, or a panic dump written after the process signed off would
// look like part of an already-known report.
type runState struct {
	StartedAt     string `json:"started_at"`
	PID           int    `json:"pid"`
	CrashLogBytes int64  `json:"crash_log_bytes"`
	CleanShutdown bool   `json:"clean_shutdown"`
}

// crashReporter persists crash evidence in the data directory. The crash log is
// opened for appending and is never truncated: a report is the only record of
// why a run died, so destroying one to make room defeats the whole mechanism.
type crashReporter struct {
	dataDir string
	file    *os.File
	// owned tracks whether beginRun claimed the state file. Without it a failure
	// before the instance lock was acquired could overwrite the state of a run
	// that is still going.
	owned    bool
	finished bool
	// baseline is the crash log size once this run had announced itself.
	baseline int64
}

// armCrashOutput routes runtime crash dumps into the crash log. It runs before
// the instance lock is held so that a port conflict or a bad configuration -
// exactly the failures that crash-loop a container - still leave a trace. Only
// creates and appends happen here; nothing a concurrently starting process owns
// is renamed, truncated, or removed.
func armCrashOutput(dataDir string) *crashReporter {
	c := &crashReporter{dataDir: dataDir}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		// Not fatal on its own: the instance lock reports the same directory
		// failure with the context an operator needs.
		log.Printf("crash reporting disabled: create data directory %s: %v", dataDir, err)
		return c
	}
	c.open()
	return c
}

func (c *crashReporter) crashLogPath() string { return filepath.Join(c.dataDir, crashLogName) }
func (c *crashReporter) statePath() string    { return filepath.Join(c.dataDir, crashStateName) }

// open points runtime crash output at the crash log. The runtime duplicates the
// descriptor, so it keeps writing to whichever file was armed last.
func (c *crashReporter) open() {
	file, err := os.OpenFile(c.crashLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("crash reporting disabled: open %s: %v", c.crashLogPath(), err)
		return
	}
	if err := debug.SetCrashOutput(file, debug.CrashOptions{}); err != nil {
		log.Printf("crash reporting disabled: arm %s: %v", c.crashLogPath(), err)
		_ = file.Close()
		return
	}
	c.file = file
}

// beginRun reports what the previous run left behind and arms detection for this
// one. It must run under the instance lock: this is the only part of crash
// reporting that rewrites shared state, so holding the lock keeps a second
// process from observing or clobbering a half-updated marker.
func (c *crashReporter) beginRun(version string) {
	previous, hadPreviousRun := readRunState(c.statePath())
	size := c.crashLogSize()
	switch {
	case hadPreviousRun && size > previous.CrashLogBytes:
		// Checked before CleanShutdown: a panic dump lands after the process
		// has already recorded how it thought it was ending.
		log.Printf("previous run ended with a crash or fatal error; the report is in %s", c.crashLogPath())
	case hadPreviousRun && !previous.CleanShutdown:
		log.Printf("previous run was terminated without a clean shutdown and left no crash report; "+
			"it was likely killed externally (kernel OOM kill or SIGKILL) or could not write %s - "+
			"check the container exit code and the kernel log", c.crashLogPath())
	}
	if size > crashLogMaxBytes {
		c.rotate()
	}
	c.appendf("=== rolltop %s started at %s pid=%d ===", version, time.Now().UTC().Format(time.RFC3339), os.Getpid())
	c.owned = true
	c.writeState(false)
}

// finish records how this run ended. It must run while the instance lock is
// still held, otherwise the next process can already have written its own marker
// and crash log by the time this one cleans up.
func (c *crashReporter) finish(err error) {
	if c.finished {
		return
	}
	c.finished = true
	if err == nil || isPlannedRestart(err) {
		// An orderly exit, including the deliberate restart for search index
		// recovery. The size baseline stays as it was, so a panic dump written
		// after this point is still recognized as new on the next start.
		c.writeState(true)
		c.close()
		return
	}
	// Leave the state marked unclean. Together with the size baseline it holds,
	// it tells the next start whether the reason below could be persisted.
	if !c.appendf("%s fatal: %v", time.Now().UTC().Format(time.RFC3339), err) {
		log.Printf("could not persist the fatal error to %s; the next start will report an unclean shutdown instead", c.crashLogPath())
	}
	c.close()
}

// rotate bounds the crash log. The current file is renamed rather than truncated
// so the reports it holds stay readable, and crash output is re-armed on the
// fresh file. A failed rename leaves everything in place and keeps appending: an
// oversized log is a far smaller problem than a destroyed one.
func (c *crashReporter) rotate() {
	previous := filepath.Join(c.dataDir, crashLogPrevName)
	if err := os.Rename(c.crashLogPath(), previous); err != nil {
		log.Printf("rotate oversized crash log %s: %v; continuing to append to it", c.crashLogPath(), err)
		return
	}
	log.Printf("crash log exceeded %d bytes; earlier reports moved to %s", crashLogMaxBytes, previous)
	c.close()
	c.open()
}

// appendf writes one line to the crash log and reports whether it landed.
func (c *crashReporter) appendf(format string, args ...any) bool {
	if c.file == nil {
		return false
	}
	if _, err := fmt.Fprintln(c.file, fmt.Sprintf(format, args...)); err != nil {
		log.Printf("write %s: %v", c.crashLogPath(), err)
		return false
	}
	return c.file.Sync() == nil
}

func (c *crashReporter) crashLogSize() int64 {
	info, err := os.Stat(c.crashLogPath())
	if err != nil {
		return 0
	}
	return info.Size()
}

// writeState records this run. The size baseline is captured on the first write
// and reused afterwards, so marking a clean shutdown cannot hide a report that
// arrives between the two writes.
func (c *crashReporter) writeState(cleanShutdown bool) {
	if !c.owned {
		return
	}
	if !cleanShutdown {
		c.baseline = c.crashLogSize()
	}
	state, err := json.Marshal(runState{
		StartedAt:     time.Now().UTC().Format(time.RFC3339),
		PID:           os.Getpid(),
		CrashLogBytes: c.baseline,
		CleanShutdown: cleanShutdown,
	})
	if err != nil {
		return
	}
	if err := os.WriteFile(c.statePath(), append(state, '\n'), 0o600); err != nil {
		log.Printf("write crash state %s: %v", c.statePath(), err)
	}
}

func (c *crashReporter) close() {
	if c.file == nil {
		return
	}
	_ = c.file.Close()
	c.file = nil
}

// readRunState reports what the previous run recorded, and whether there was a
// previous run at all. State that cannot be parsed still counts as evidence of
// an unclean shutdown; it claims an unreachable size so a crash log left from
// before is not misread as a fresh report.
func readRunState(path string) (runState, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return runState{}, false
	}
	var state runState
	if err := json.Unmarshal(data, &state); err != nil {
		return runState{CrashLogBytes: math.MaxInt64}, true
	}
	return state, true
}
