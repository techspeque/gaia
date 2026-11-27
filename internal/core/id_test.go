package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func seq(start byte) [16]byte {
	var b [16]byte
	for i := range b {
		b[i] = start + byte(i)
	}
	return b
}

func TestIDRoundTrip(t *testing.T) {
	for _, b := range [][16]byte{{}, seq(1), seq(240), {0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}} {
		n := NewNodeID(b)
		if !strings.HasPrefix(n.String(), "node_") || len(n.String()) != len("node_")+idBodyLen {
			t.Fatalf("node id form %q", n)
		}
		got, err := ParseNodeID(n.String())
		if err != nil || got != n {
			t.Fatalf("ParseNodeID(%q) = %v, %v", n, got, err)
		}
		s := NewSessionID(b)
		if !strings.HasPrefix(s.String(), "sess_") {
			t.Fatalf("session id form %q", s)
		}
		gs, err := ParseSessionID(s.String())
		if err != nil || gs != s {
			t.Fatalf("ParseSessionID(%q) = %v, %v", s, gs, err)
		}
	}
}

func TestIDDistinctBytesDistinctIDs(t *testing.T) {
	seen := map[string]bool{}
	for i := range 16 {
		for bit := range 8 {
			var b [16]byte
			b[i] = 1 << bit
			s := NewNodeID(b).String()
			if seen[s] {
				t.Fatalf("collision on byte %d bit %d: %s", i, bit, s)
			}
			seen[s] = true
		}
	}
}

func TestIDJSONRoundTrip(t *testing.T) {
	type wrap struct {
		Node    NodeID    `json:"node"`
		Session SessionID `json:"session"`
	}
	in := wrap{NewNodeID(seq(7)), NewSessionID(seq(9))}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"node":"` + in.Node.String() + `","session":"` + in.Session.String() + `"}`
	if string(b) != want {
		t.Fatalf("json = %s, want %s", b, want)
	}
	var out wrap
	if err := json.Unmarshal(b, &out); err != nil || out != in {
		t.Fatalf("unmarshal = %+v, %v", out, err)
	}
}

func TestIDZeroValueRefusedOnMarshal(t *testing.T) {
	if _, err := json.Marshal(NodeID{}); err == nil {
		t.Fatal("zero NodeID marshalled")
	}
	if _, err := json.Marshal(SessionID{}); err == nil {
		t.Fatal("zero SessionID marshalled")
	}
}

func TestIDParseRejects(t *testing.T) {
	good := NewNodeID(seq(3)).String()
	body := strings.TrimPrefix(good, "node_")
	cases := map[string]string{
		"empty":             "",
		"no prefix":         body,
		"session prefix":    "sess_" + body,
		"upper prefix":      "NODE_" + body,
		"dash separator":    "node-" + body,
		"short":             good[:len(good)-1],
		"long":              good + "0",
		"uppercase body":    "node_" + strings.ToUpper(body),
		"excluded letter i": "node_i" + body[1:],
		"excluded letter u": "node_u" + body[1:],
		"symbol":            "node_" + body[:10] + "!" + body[11:],
		"non-canonical end": "node_" + body[:idBodyLen-1] + "1",
		"space":             " " + good,
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseNodeID(s)
			if err == nil {
				t.Fatalf("ParseNodeID(%q) accepted", s)
			}
			var e *Error
			if !errors.As(err, &e) || e.Code != CodeInvalidArgument {
				t.Fatalf("error %v is not invalid_argument", err)
			}
			var n NodeID
			if err := json.Unmarshal([]byte(`"`+s+`"`), &n); err == nil {
				t.Fatalf("json accepted %q", s)
			}
		})
	}
	if _, err := ParseSessionID(good); err == nil {
		t.Fatal("ParseSessionID accepted a node id")
	}
}
