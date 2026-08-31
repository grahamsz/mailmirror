// File overview: Test helper for asserting on standard-library log output.
// Several packages need to capture log.Printf lines; keeping one implementation
// here stops each of them from re-deriving the save/restore dance and forgetting
// a piece of the logger state.

package testlog

import (
	"bytes"
	"log"
	"testing"
)

// Capture redirects the standard logger into the returned buffer for the rest of
// the test and restores the previous writer, flags, and prefix afterwards. It is
// not safe for parallel tests: the standard logger is process-global.
func Capture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	previousPrefix := log.Prefix()
	log.SetOutput(&logs)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
		log.SetPrefix(previousPrefix)
	})
	return &logs
}
