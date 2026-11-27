package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestErrorJSONShape(t *testing.T) {
	cases := []struct {
		err  *Error
		want string
	}{
		{NewError(CodeNotFound, "no such node"), `{"code":"not_found","message":"no such node","details":{}}`},
		{&Error{Code: CodeConflict, Message: "m", Details: map[string]string{"a": "b"}}, `{"code":"conflict","message":"m","details":{"a":"b"}}`},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.err)
		if err != nil || string(b) != c.want {
			t.Fatalf("json = %s, %v; want %s", b, err, c.want)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(b, &keys); err != nil || len(keys) != 3 {
			t.Fatalf("keys = %v, %v", keys, err)
		}
		var back Error
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if back.Code != c.err.Code || back.Message != c.err.Message {
			t.Fatalf("round trip = %+v", back)
		}
		if len(c.err.Details) > 0 && !reflect.DeepEqual(back.Details, c.err.Details) {
			t.Fatalf("details = %v", back.Details)
		}
	}
}

func TestErrorResponseShape(t *testing.T) {
	b, err := json.Marshal(ErrorResponse{NewError(CodeInternal, "x")})
	if err != nil || string(b) != `{"error":{"code":"internal","message":"x","details":{}}}` {
		t.Fatalf("json = %s, %v", b, err)
	}
}

func TestErrorIsMatchesCode(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", &Error{Code: CodeNotFound, Message: "node x"})
	if !errors.Is(err, NewError(CodeNotFound, "")) {
		t.Fatal("errors.Is did not match by code")
	}
	if errors.Is(err, NewError(CodeConflict, "")) {
		t.Fatal("errors.Is matched a different code")
	}
	if errors.Is(err, errors.New("not_found")) {
		t.Fatal("errors.Is matched a non-core error")
	}
	var e *Error
	if !errors.As(err, &e) || e.Message != "node x" {
		t.Fatal("errors.As failed")
	}
	if got := e.Error(); got != "not_found: node x" {
		t.Fatalf("Error() = %q", got)
	}
	if got := NewError(CodeInternal, "").Error(); got != "internal" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestErrorCodesUnique(t *testing.T) {
	seen := map[ErrorCode]bool{}
	for _, c := range ErrorCodes {
		if c == "" || seen[c] {
			t.Fatalf("bad or duplicate code %q", c)
		}
		seen[c] = true
	}
}
