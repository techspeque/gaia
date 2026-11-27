package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const testSecret = "sk-tst-hunter2-value-7f3a"

// leaks reports whether out shows the secret or a piece of it, in plain
// form, as hex or as code points.
func leaks(out string) bool {
	for _, needle := range []string{testSecret, "hunter2", "7f3a", fmt.Sprintf("%x", testSecret)[:12], "U+0073"} {
		if strings.Contains(strings.ToLower(out), strings.ToLower(needle)) {
			return true
		}
	}
	return false
}

func TestSecretFormatting(t *testing.T) {
	s := NewSecret(testSecret)
	type exported struct {
		Token Secret
	}
	type unexported struct {
		token Secret
	}
	type ptr struct {
		Token *Secret
	}
	values := map[string]any{
		"value":            s,
		"pointer":          &s,
		"exported field":   exported{s},
		"unexported field": unexported{s},
		"pointer field":    ptr{&s},
		"slice":            []Secret{s},
		"map":              map[string]Secret{"k": s},
		"any slice":        []any{s},
	}
	verbs := []string{
		"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T", "%p",
		"%10s", "%-10s", "%.3s", "%.30s", "% x", "%#x", "%+q", "%#q", "%U", "%c", "%b", "%o", "%e", "%t",
	}
	for name, v := range values {
		for _, verb := range verbs {
			out := fmt.Sprintf(verb, v)
			if leaks(out) {
				t.Errorf("%s with %s leaked: %s", name, verb, out)
			}
		}
		for _, out := range []string{fmt.Sprint(v), fmt.Sprintln(v), fmt.Sprintf("%v %v", v, v)} {
			if leaks(out) {
				t.Errorf("%s via Sprint leaked: %s", name, out)
			}
		}
	}
	if s.String() != Redacted || s.GoString() != Redacted {
		t.Fatal("String or GoString not redacted")
	}
	if fmt.Sprint(s) != Redacted {
		t.Fatalf("Sprint = %q", fmt.Sprint(s))
	}
	if err := fmt.Errorf("auth: %v", s); leaks(err.Error()) {
		t.Fatal("error wrapping leaked")
	}
}

func TestSecretEncoding(t *testing.T) {
	s := NewSecret(testSecret)
	type wrap struct {
		Token  Secret            `json:"token"`
		Ptr    *Secret           `json:"ptr"`
		List   []Secret          `json:"list"`
		ByName map[string]Secret `json:"by_name"`
		hidden Secret
	}
	for name, v := range map[string]any{
		"bare":   s,
		"ptr":    &s,
		"nested": wrap{Token: s, Ptr: &s, List: []Secret{s}, ByName: map[string]Secret{"a": s}, hidden: s},
	} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if leaks(string(b)) || !strings.Contains(string(b), Redacted) {
			t.Errorf("%s json = %s", name, b)
		}
		b, err = json.MarshalIndent(v, "", " ")
		if err != nil || leaks(string(b)) {
			t.Errorf("%s indented json = %s, %v", name, b, err)
		}
	}
	text, err := s.MarshalText()
	if err != nil || string(text) != Redacted {
		t.Fatalf("text = %s, %v", text, err)
	}
}

func TestSecretReveal(t *testing.T) {
	if got := NewSecret(testSecret).Reveal(); got != testSecret {
		t.Fatalf("Reveal = %q", got)
	}
	var zero Secret
	if zero.Reveal() != "" || !zero.Empty() || NewSecret("x").Empty() {
		t.Fatal("zero/empty handling wrong")
	}
	if fmt.Sprint(zero) != Redacted {
		t.Fatal("zero secret not redacted")
	}
}
