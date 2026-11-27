package core

import (
	"errors"
	"strings"
	"testing"
)

func TestValidNodeName(t *testing.T) {
	for _, ok := range []string{"a", "build-box", "node1", "0", strings.Repeat("a", 63)} {
		if err := ValidNodeName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-a", "a-", "Build", "a.b", "a_b", "a b", "a/b", "../x", "ü", strings.Repeat("a", 64)} {
		err := ValidNodeName(bad)
		if !errors.Is(err, NewError(CodeInvalidArgument, "")) {
			t.Errorf("%q: %v, want invalid_argument", bad, err)
		}
	}
}

func TestNodePath(t *testing.T) {
	if got := NodePath("build-box"); got != "/api/v1/nodes/build-box" {
		t.Errorf("NodePath = %q", got)
	}
}
