package core

import "fmt"

// Redacted is what every encoding of a Secret produces.
const Redacted = "[REDACTED]"

// Secret holds a secret value that no formatting or encoding reveals. The
// value is reachable only through Reveal, so every use is explicit.
//
// The value is held in a closure. When a Secret is an unexported field of
// another struct, fmt cannot call its methods and walks it by reflection;
// a func is the one kind it then prints only as an address, for every verb.
// A string or a pointer would be printed in full. Secret is not comparable,
// which also keeps secrets out of == (not constant-time) and map keys.
// The zero Secret is empty.
type Secret struct{ value func() string }

// NewSecret wraps v.
func NewSecret(v string) Secret { return Secret{func() string { return v }} }

// Reveal returns the secret value. Call it only where the value must leave
// the process's memory: an agent's environment, an upstream auth header.
func (s Secret) Reveal() string {
	if s.value == nil {
		return ""
	}
	return s.value()
}

// Empty reports whether the secret holds no value.
func (s Secret) Empty() bool { return s.Reveal() == "" }

// String returns Redacted.
func (Secret) String() string { return Redacted }

// GoString returns Redacted, for %#v.
func (Secret) GoString() string { return Redacted }

// Format writes Redacted for every verb, flag, width and precision.
func (Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(Redacted)) }

// MarshalText returns Redacted.
func (Secret) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// MarshalJSON returns Redacted as a JSON string.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }
