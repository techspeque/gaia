package core

import (
	"errors"
	"testing"
)

func TestNodeStartRequestValidate(t *testing.T) {
	id := NewSessionID([16]byte{7})
	ok := NodeStartRequest{ID: id, Mode: SessionHeadless, Argv: []string{"claude", ""}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	for name, r := range map[string]NodeStartRequest{
		"no id":      {Mode: SessionHeadless, Argv: []string{"a"}},
		"bad mode":   {ID: id, Mode: "pty", Argv: []string{"a"}},
		"empty argv": {ID: id, Mode: SessionHeadless},
		"empty prog": {ID: id, Mode: SessionInteractive, Argv: []string{""}},
	} {
		if err := r.Validate(); !isInvalid(err) {
			t.Errorf("%s: %v, want invalid_argument", name, err)
		}
	}
}

func TestExitCodeByName(t *testing.T) {
	for _, c := range ExitCodes {
		got, ok := ExitCodeByName(c.Name())
		if !ok || got != c {
			t.Errorf("%q: %d %v, want %d", c.Name(), got, ok, c)
		}
	}
	for _, n := range []string{"", "nope", "Node_Unavailable"} {
		if _, ok := ExitCodeByName(n); ok {
			t.Errorf("%q resolved", n)
		}
	}
}

func TestWithReason(t *testing.T) {
	e := &Error{Code: CodeUnavailable, Message: "m", Details: map[string]string{"node": "b"}}
	r := e.WithReason(ExitNodeUnavailable)
	if r.Details[DetailReason] != "node_unavailable" || r.Details["node"] != "b" {
		t.Errorf("details %v", r.Details)
	}
	if _, ok := e.Details[DetailReason]; ok {
		t.Error("WithReason changed the original")
	}
	if !errors.Is(r, NewError(CodeUnavailable, "")) {
		t.Error("code changed")
	}
}

func TestNodeAPIPaths(t *testing.T) {
	id := NewSessionID([16]byte{1})
	for got, want := range map[string]string{
		SessionStopPath(id):     "/api/v1/sessions/" + id.String() + "/stop",
		SessionLogsPath(id):     "/api/v1/sessions/" + id.String() + "/logs",
		NodeSessionStopPath(id): "/api/v1/node/sessions/" + id.String() + "/stop",
		NodeSessionLogsPath(id): "/api/v1/node/sessions/" + id.String() + "/logs",
		SessionReportsPath:      "/api/v1/session-reports",
	} {
		if got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
}
