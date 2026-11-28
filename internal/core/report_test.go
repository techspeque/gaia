package core

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSessionReportValidate(t *testing.T) {
	id := NewSessionID([16]byte{9})
	code := 143
	end := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	var zero time.Time
	ok := []SessionReport{
		{SessionID: id, Seq: 1, State: SessionRunning},
		{SessionID: id, Seq: 2, State: SessionLost},
		{SessionID: id, Seq: 3, State: SessionExited, ExitCode: &code, EndedAt: &end},
		{SessionID: id, Seq: 3, State: SessionStopped, ExitCode: &code, EndedAt: &end},
		{SessionID: id, Seq: 3, State: SessionStopped, EndedAt: &end},
		{SessionID: id, Seq: 1, State: SessionFailed, EndedAt: &end},
	}
	for _, r := range ok {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
	bad := map[string]SessionReport{
		"no id":            {Seq: 1, State: SessionRunning},
		"seq 0":            {SessionID: id, State: SessionRunning},
		"bad state":        {SessionID: id, Seq: 1, State: "gone"},
		"running ended":    {SessionID: id, Seq: 1, State: SessionRunning, EndedAt: &end},
		"exited no end":    {SessionID: id, Seq: 1, State: SessionExited, ExitCode: &code},
		"exited no code":   {SessionID: id, Seq: 1, State: SessionExited, EndedAt: &end},
		"zero end":         {SessionID: id, Seq: 1, State: SessionFailed, EndedAt: &zero},
		"failed with code": {SessionID: id, Seq: 1, State: SessionFailed, EndedAt: &end, ExitCode: &code},
		"lost with code":   {SessionID: id, Seq: 1, State: SessionLost, ExitCode: &code},
	}
	for name, r := range bad {
		if err := r.Validate(); !isInvalid(err) {
			t.Errorf("%s: %v, want invalid_argument", name, err)
		}
	}
}

// TestSessionReportShape fixes the report's JSON form.
func TestSessionReportShape(t *testing.T) {
	code := 3
	end := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	r := SessionReport{SessionID: NewSessionID([16]byte{9}), Seq: 4, State: SessionExited, ExitCode: &code, EndedAt: &end}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"session_id":"` + r.SessionID.String() + `","seq":4,"state":"exited","exit_code":3,"ended_at":"2026-11-28T12:00:00Z"}`
	if string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}
}
