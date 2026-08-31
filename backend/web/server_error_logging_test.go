// File overview: Tests that handler failures reach the operator log with the
// endpoint that produced them, and that ordinary client disconnects do not.

package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rolltop/internal/testlog"
)

func TestServerErrorLogsCauseWithRequest(t *testing.T) {
	logs := testlog.Capture(t)
	s := &Server{}
	rec := httptest.NewRecorder()

	s.serverError(rec, httptest.NewRequest(http.MethodGet, "/api/messages/42", nil), errors.New("query messages: disk I/O error"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	got := logs.String()
	if !strings.Contains(got, "query messages: disk I/O error") {
		t.Fatalf("log %q does not contain the underlying error", got)
	}
	if !strings.Contains(got, "GET /api/messages/42") {
		t.Fatalf("log %q does not identify the endpoint", got)
	}
}

// Search terms are mail content, so the query string must stay out of the log.
func TestServerErrorLogsPathWithoutQueryString(t *testing.T) {
	logs := testlog.Capture(t)
	s := &Server{}

	s.serverError(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/search?q=salary+negotiation", nil), errors.New("search failed"))

	if strings.Contains(logs.String(), "salary") {
		t.Fatalf("log leaked the search query: %q", logs.String())
	}
}

// A decoded request path can carry newlines, which would otherwise let a caller
// forge additional log records.
func TestServerErrorEscapesLineSeparatorsInPath(t *testing.T) {
	logs := testlog.Capture(t)
	s := &Server{}
	r := httptest.NewRequest(http.MethodGet, "/api/mail", nil)
	r.URL.Path = "/api/mail\nerror forged: nothing to see"

	s.serverError(httptest.NewRecorder(), r, errors.New("boom"))

	if strings.Count(strings.TrimSuffix(logs.String(), "\n"), "\n") != 0 {
		t.Fatalf("log line was split by a crafted path: %q", logs.String())
	}
}

func TestServerErrorTreatsClientDisconnectsAsRoutine(t *testing.T) {
	for name, err := range map[string]error{
		"canceled":  context.Canceled,
		"deadline":  context.DeadlineExceeded,
		"wrapped":   errors.Join(errors.New("load thread"), context.Canceled),
		"deadline2": errors.Join(errors.New("reserve foreground slot"), context.DeadlineExceeded),
	} {
		t.Run(name, func(t *testing.T) {
			logs := testlog.Capture(t)
			s := &Server{}
			rec := httptest.NewRecorder()

			s.serverError(rec, httptest.NewRequest(http.MethodGet, "/api/mail", nil), err)

			if rec.Code != http.StatusRequestTimeout {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestTimeout)
			}
			if logs.Len() != 0 {
				t.Fatalf("client disconnect was logged as a server failure: %q", logs.String())
			}
		})
	}
}

// The plugin ABI cannot pass the request; the failure must still be logged.
func TestServerErrorWithoutRequestStillLogs(t *testing.T) {
	logs := testlog.Capture(t)
	s := &Server{}

	s.ServerError(httptest.NewRecorder(), errors.New("plugin store failure"))

	if !strings.Contains(logs.String(), "plugin store failure") {
		t.Fatalf("log %q does not contain the plugin error", logs.String())
	}
}

func TestAPIErrorKeepsCallerStatusAndLogsCause(t *testing.T) {
	logs := testlog.Capture(t)
	s := &Server{}
	rec := httptest.NewRecorder()

	s.apiError(rec, httptest.NewRequest(http.MethodPost, "/api/messages/move", nil),
		http.StatusServiceUnavailable, "could not schedule message move", errors.New("sync runner stopped"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(rec.Body.String(), "could not schedule message move") {
		t.Fatalf("client message missing: %q", rec.Body.String())
	}
	if !strings.Contains(logs.String(), "sync runner stopped") {
		t.Fatalf("log %q does not contain the cause", logs.String())
	}
}

func TestAPIErrorDoesNotLogClientDisconnects(t *testing.T) {
	logs := testlog.Capture(t)
	s := &Server{}
	rec := httptest.NewRecorder()

	s.apiError(rec, httptest.NewRequest(http.MethodPost, "/api/messages/move", nil),
		http.StatusServiceUnavailable, "could not schedule message move", context.Canceled)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if logs.Len() != 0 {
		t.Fatalf("client disconnect was logged: %q", logs.String())
	}
}
