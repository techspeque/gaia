package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestSuperviseDispatch proves `gaia supervise` reaches the supervisor and
// refuses bad flags as the other daemons do, without listing it in the
// usage text.
func TestSuperviseDispatch(t *testing.T) {
	for _, args := range [][]string{{"supervise"}, {"supervise", "--bogus"}, {"supervise", "--session-dir", "d", "extra"}} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 2 {
			t.Errorf("%q: exit %d, want 2; stderr %q", args, code, errOut.String())
		}
		if strings.Contains(errOut.String(), "unknown command") {
			t.Errorf("%q: not dispatched: %q", args, errOut.String())
		}
	}
	if strings.Contains(usage, "supervise") {
		t.Error("the internal supervise command is listed in the usage text")
	}
}
