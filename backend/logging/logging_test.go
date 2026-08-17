package logging

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestDebugfRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() {
		log.SetOutput(orig)
		SetDebug(false)
	})

	SetDebug(false)
	Debugf("hidden line plugin_id=%s", "example")
	if buf.Len() != 0 {
		t.Fatalf("debug line was written with debug disabled: %q", buf.String())
	}

	SetDebug(true)
	Debugf("visible line plugin_id=%s", "example")
	if got := buf.String(); !strings.Contains(got, "debug visible line plugin_id=example") {
		t.Fatalf("debug line missing with debug enabled: %q", got)
	}
}

func TestDebugfEscapesLineSeparators(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() {
		log.SetOutput(orig)
		SetDebug(false)
	})

	SetDebug(true)
	Debugf("target=%s", "https://example.test/x\r\n2026/01/01 forged line")
	got := buf.String()
	if strings.Count(got, "\n") != 1 {
		t.Fatalf("expected a single log record, got: %q", got)
	}
	if !strings.Contains(got, `target=https://example.test/x\r\n2026/01/01 forged line`) {
		t.Fatalf("separators were not escaped: %q", got)
	}
}
