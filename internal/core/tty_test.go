package core

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestCheckOrigin(t *testing.T) {
	cases := []struct {
		origin, host string
		ok           bool
	}{
		{"", "gaia", true},
		{"https://gaia-host", "gaia-host", true},
		{"http://gaia-host:8080", "gaia-host:8080", true},
		{"HTTPS://Gaia-Host", "gaia-host", true},
		{"https://evil.example", "gaia-host", false},
		{"https://gaia-host:444", "gaia-host", false},
		{"https://gaia-host", "gaia-host:444", false},
		{"null", "gaia-host", false},
		{"file://gaia-host", "gaia-host", false},
		{"https://", "", false},
		{"https://gaia-host/path", "gaia-host/path", false},
		{"https://user@gaia-host", "user@gaia-host", false},
		{"gaia-host", "gaia-host", false},
		{"://gaia-host", "gaia-host", false},
	}
	for _, c := range cases {
		err := CheckOrigin(c.origin, c.host)
		if (err == nil) != c.ok {
			t.Errorf("CheckOrigin(%q, %q) = %v, want ok=%v", c.origin, c.host, err, c.ok)
		}
		if err != nil && !errors.Is(err, NewError(CodeForbidden, "")) {
			t.Errorf("CheckOrigin(%q, %q) = %v, want forbidden", c.origin, c.host, err)
		}
	}
}

func TestTTYMessageJSON(t *testing.T) {
	cases := []struct {
		m    TTYMessage
		want string
	}{
		{TTYMessage{Type: TTYHello, Mode: SessionInteractive, Offset: 0}, `{"type":"hello","mode":"interactive","offset":0}`},
		{TTYMessage{Type: TTYHello, Mode: SessionHeadless, Offset: 42}, `{"type":"hello","mode":"headless","offset":42}`},
		{TTYMessage{Type: TTYResize, Rows: 24, Cols: 80}, `{"type":"resize","offset":0,"rows":24,"cols":80}`},
		{TTYMessage{Type: TTYNotice, Message: "no"}, `{"type":"notice","offset":0,"message":"no"}`},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.m)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.want {
			t.Errorf("%+v encodes as %s, want %s", c.m, b, c.want)
		}
		var back TTYMessage
		if err := json.Unmarshal(b, &back); err != nil || back != c.m {
			t.Errorf("%s decodes as %+v (%v)", b, back, err)
		}
	}
	id := NewSessionID([16]byte{1})
	if got, want := SessionTTYPath(id), SessionPath(id)+"/tty"; got != want {
		t.Errorf("SessionTTYPath = %s, want %s", got, want)
	}
	if got, want := NodeSessionTTYPath(id), NodeSessionPath(id)+"/tty"; got != want {
		t.Errorf("NodeSessionTTYPath = %s, want %s", got, want)
	}
}
