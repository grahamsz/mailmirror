// File overview: Tests for persistent crash reporting: telling a clean exit, a
// reported failure, and a silent kill apart across restarts, and never losing a
// report that was already written.

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rolltop/internal/testlog"
)

// restart replays what a process boundary does: arm crash output, announce the
// run, then end it with err. It returns the log the started run produced.
func restart(t *testing.T, dataDir string, err error) string {
	t.Helper()
	logs := testlog.Capture(t)
	reporter := armCrashOutput(dataDir)
	reporter.beginRun("test")
	reporter.finish(err)
	return logs.String()
}

func TestFirstStartReportsNothing(t *testing.T) {
	dataDir := t.TempDir()

	logs := restart(t, dataDir, nil)

	if strings.Contains(logs, "previous run") {
		t.Fatalf("first start reported previous-run evidence: %q", logs)
	}
	state, ok := readRunState(filepath.Join(dataDir, crashStateName))
	if !ok || !state.CleanShutdown {
		t.Fatalf("clean shutdown not recorded: state=%+v ok=%t", state, ok)
	}
}

func TestCleanShutdownStaysQuietOnNextStart(t *testing.T) {
	dataDir := t.TempDir()
	restart(t, dataDir, nil)

	logs := restart(t, dataDir, nil)

	if strings.Contains(logs, "previous run") {
		t.Fatalf("clean shutdown reported as an incident: %q", logs)
	}
}

func TestFatalErrorIsReportedOnNextStart(t *testing.T) {
	dataDir := t.TempDir()
	restart(t, dataDir, errors.New("listen on :8080: address already in use"))

	logs := restart(t, dataDir, nil)

	if !strings.Contains(logs, "previous run ended with a crash or fatal error") {
		t.Fatalf("fatal exit not reported on the next start: %q", logs)
	}
	report, err := os.ReadFile(filepath.Join(dataDir, crashLogName))
	if err != nil {
		t.Fatalf("crash log missing: %v", err)
	}
	if !strings.Contains(string(report), "fatal: listen on :8080: address already in use") {
		t.Fatalf("crash log does not hold the fatal error: %q", report)
	}
}

// An unrecovered panic runs deferred functions first and writes its dump only
// afterwards, so the run signs off as clean and the dump lands after that. The
// dump must still be reported on the next start.
func TestPanicDumpAfterCleanSignOffIsReportedOnNextStart(t *testing.T) {
	dataDir := t.TempDir()
	func() {
		testlog.Capture(t)
		reporter := armCrashOutput(dataDir)
		reporter.beginRun("test")
		// The deferred crash.finish in run() sees no error while unwinding.
		reporter.finish(nil)
		// Only now does the runtime write the dump through the armed file.
		appendToCrashLog(t, dataDir, "panic: simulated\n\ngoroutine 1 [running]:")
	}()

	logs := restart(t, dataDir, nil)

	if !strings.Contains(logs, "previous run ended with a crash or fatal error") {
		t.Fatalf("panic dump written after the deferred sign-off was not reported: %q", logs)
	}
}

// appendToCrashLog writes through a second handle, the way the runtime writes a
// crash dump through the descriptor it duplicated when the output was armed.
func appendToCrashLog(t *testing.T, dataDir, contents string) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(dataDir, crashLogName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(contents + "\n"); err != nil {
		t.Fatal(err)
	}
}

func TestSilentKillIsReportedOnNextStart(t *testing.T) {
	dataDir := t.TempDir()
	func() {
		testlog.Capture(t)
		reporter := armCrashOutput(dataDir)
		reporter.beginRun("test")
		// No finish and no output: the process was killed outright.
	}()

	logs := restart(t, dataDir, nil)

	if !strings.Contains(logs, "killed externally") {
		t.Fatalf("silent kill not reported on the next start: %q", logs)
	}
}

// The deliberate restart for search index recovery is an intended outcome, so it
// must not be filed as a crash or leave evidence behind.
func TestPlannedRestartIsNotACrash(t *testing.T) {
	dataDir := t.TempDir()
	restartErr := fmt.Errorf("search index writer stalled for user %d; %w", int64(1), errRestartForRecovery)
	restart(t, dataDir, restartErr)

	logs := restart(t, dataDir, nil)

	if strings.Contains(logs, "previous run") {
		t.Fatalf("planned restart reported as an incident: %q", logs)
	}
	if _, err := os.Stat(filepath.Join(dataDir, crashLogName)); err == nil {
		contents, _ := os.ReadFile(filepath.Join(dataDir, crashLogName))
		if strings.Contains(string(contents), "fatal:") {
			t.Fatalf("planned restart recorded as a fatal error: %q", contents)
		}
	}
}

// The restart itself is planned, but cleanup that did not complete is a real
// failure and must not inherit the planned classification.
func TestFailedRestartCleanupIsRecordedAsFatal(t *testing.T) {
	dataDir := t.TempDir()
	cleanupErr := fmt.Errorf("search index writer stalled for user %d; restart cleanup failed: %w",
		int64(1), errSearchWriterRestartShutdownTimeout)
	if isPlannedRestart(cleanupErr) {
		t.Fatal("a failed restart cleanup is still classified as a planned restart")
	}
	restart(t, dataDir, cleanupErr)

	logs := restart(t, dataDir, nil)

	if !strings.Contains(logs, "previous run ended with a crash or fatal error") {
		t.Fatalf("failed restart cleanup not reported on the next start: %q", logs)
	}
	contents, err := os.ReadFile(filepath.Join(dataDir, crashLogName))
	if err != nil {
		t.Fatalf("crash log missing: %v", err)
	}
	if !strings.Contains(string(contents), "restart cleanup failed") {
		t.Fatalf("crash log does not hold the cleanup failure: %q", contents)
	}
}

// The report of an earlier crash must survive every later start, including the
// rotation that bounds the log.
func TestEarlierReportsSurviveLaterStarts(t *testing.T) {
	dataDir := t.TempDir()
	restart(t, dataDir, errors.New("first failure"))
	restart(t, dataDir, errors.New("second failure"))

	contents, err := os.ReadFile(filepath.Join(dataDir, crashLogName))
	if err != nil {
		t.Fatalf("crash log missing: %v", err)
	}
	for _, want := range []string{"first failure", "second failure"} {
		if !strings.Contains(string(contents), want) {
			t.Fatalf("crash log lost %q: %s", want, contents)
		}
	}
}

func TestOversizedCrashLogRotatesInsteadOfTruncating(t *testing.T) {
	dataDir := t.TempDir()
	crashPath := filepath.Join(dataDir, crashLogName)
	if err := os.WriteFile(crashPath, append([]byte("panic: the original report\n"), make([]byte, crashLogMaxBytes)...), 0o600); err != nil {
		t.Fatal(err)
	}

	restart(t, dataDir, nil)

	preserved, err := os.ReadFile(filepath.Join(dataDir, crashLogPrevName))
	if err != nil {
		t.Fatalf("rotated crash log missing: %v", err)
	}
	if !strings.Contains(string(preserved), "panic: the original report") {
		t.Fatal("rotated crash log does not hold the original report")
	}
}

// Rotation is the only operation that can lose reports, so a rename it cannot
// perform must leave the existing log intact rather than start a fresh one.
func TestFailedRotationKeepsTheExistingReport(t *testing.T) {
	dataDir := t.TempDir()
	crashPath := filepath.Join(dataDir, crashLogName)
	if err := os.WriteFile(crashPath, append([]byte("panic: the original report\n"), make([]byte, crashLogMaxBytes)...), 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory cannot be replaced by a rename of a regular file.
	if err := os.Mkdir(filepath.Join(dataDir, crashLogPrevName), 0o700); err != nil {
		t.Fatal(err)
	}

	restart(t, dataDir, nil)

	contents, err := os.ReadFile(crashPath)
	if err != nil {
		t.Fatalf("crash log missing after a failed rotation: %v", err)
	}
	if !strings.Contains(string(contents), "panic: the original report") {
		t.Fatalf("failed rotation destroyed the report it could not preserve: %q", truncateForMessage(contents))
	}
}

// With the crash log unwritable there is nothing to append a fatal error to, so
// the state must stay unclean: it is the only remaining evidence the run failed.
func TestUnwritableCrashLogStillReportsUncleanShutdown(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dataDir, crashLogName), 0o700); err != nil {
		t.Fatal(err)
	}
	func() {
		testlog.Capture(t)
		reporter := armCrashOutput(dataDir)
		reporter.beginRun("test")
		reporter.finish(errors.New("store failed to open"))
	}()

	logs := restart(t, dataDir, nil)

	if !strings.Contains(logs, "without a clean shutdown") {
		t.Fatalf("unpersisted fatal left no trace for the next start: %q", logs)
	}
}

// A marker left by a still-running process must not be deleted by a run that
// failed before it acquired the instance lock.
func TestFailureBeforeBeginRunLeavesLiveStateAlone(t *testing.T) {
	dataDir := t.TempDir()
	testlog.Capture(t)
	live := armCrashOutput(dataDir)
	live.beginRun("test")

	early := armCrashOutput(dataDir)
	early.finish(nil)

	state, ok := readRunState(filepath.Join(dataDir, crashStateName))
	if !ok || state.CleanShutdown {
		t.Fatalf("a run that never started overwrote the live run's state: state=%+v ok=%t", state, ok)
	}
}

func TestFinishIsIdempotent(t *testing.T) {
	dataDir := t.TempDir()
	testlog.Capture(t)
	reporter := armCrashOutput(dataDir)
	reporter.beginRun("test")
	reporter.finish(errors.New("boom"))
	reporter.finish(errors.New("boom"))

	contents, err := os.ReadFile(filepath.Join(dataDir, crashLogName))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(contents), "fatal: boom"); got != 1 {
		t.Fatalf("fatal recorded %d times, want 1", got)
	}
}

func truncateForMessage(contents []byte) string {
	if len(contents) > 120 {
		return string(contents[:120]) + "..."
	}
	return string(contents)
}
