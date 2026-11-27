package core

import (
	"fmt"
	"strings"
)

// Id prefixes. The string form of an id is "<prefix>_<body>".
const (
	NodeIDPrefix    = "node"
	SessionIDPrefix = "sess"
)

// idAlphabet is lowercase Crockford base32: no i, l, o or u.
const idAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// idBodyLen is the length of 16 bytes (128 bits) in base32, rounded up.
const idBodyLen = 26

// NodeID identifies a node. The zero value is not a valid id.
type NodeID struct{ s string }

// SessionID identifies a session. The zero value is not a valid id.
type SessionID struct{ s string }

// NewNodeID builds a NodeID from 16 caller-supplied random bytes. core reads
// no entropy source itself.
func NewNodeID(b [16]byte) NodeID { return NodeID{NodeIDPrefix + "_" + encodeIDBody(b)} }

// NewSessionID builds a SessionID from 16 caller-supplied random bytes.
func NewSessionID(b [16]byte) SessionID {
	return SessionID{SessionIDPrefix + "_" + encodeIDBody(b)}
}

// ParseNodeID parses the string form of a NodeID.
func ParseNodeID(s string) (NodeID, error) {
	if err := parseID(NodeIDPrefix, s); err != nil {
		return NodeID{}, err
	}
	return NodeID{s}, nil
}

// ParseSessionID parses the string form of a SessionID.
func ParseSessionID(s string) (SessionID, error) {
	if err := parseID(SessionIDPrefix, s); err != nil {
		return SessionID{}, err
	}
	return SessionID{s}, nil
}

// String returns the id's string form, or "" for the zero value.
func (id NodeID) String() string { return id.s }

// IsZero reports whether id is the zero value.
func (id NodeID) IsZero() bool { return id.s == "" }

// MarshalText encodes the id as its string form. The zero value is refused.
func (id NodeID) MarshalText() ([]byte, error) {
	if id.IsZero() {
		return nil, invalidID(NodeIDPrefix, "", "empty id")
	}
	return []byte(id.s), nil
}

// UnmarshalText parses the id's string form.
func (id *NodeID) UnmarshalText(b []byte) error {
	v, err := ParseNodeID(string(b))
	if err != nil {
		return err
	}
	*id = v
	return nil
}

// String returns the id's string form, or "" for the zero value.
func (id SessionID) String() string { return id.s }

// IsZero reports whether id is the zero value.
func (id SessionID) IsZero() bool { return id.s == "" }

// MarshalText encodes the id as its string form. The zero value is refused.
func (id SessionID) MarshalText() ([]byte, error) {
	if id.IsZero() {
		return nil, invalidID(SessionIDPrefix, "", "empty id")
	}
	return []byte(id.s), nil
}

// UnmarshalText parses the id's string form.
func (id *SessionID) UnmarshalText(b []byte) error {
	v, err := ParseSessionID(string(b))
	if err != nil {
		return err
	}
	*id = v
	return nil
}

// encodeIDBody encodes 16 bytes as 26 base32 characters, most significant
// bits first; the final character carries the last 3 bits padded with zeros.
func encodeIDBody(b [16]byte) string {
	var out [idBodyLen]byte
	var acc uint32
	bits := 0
	n := 0
	for _, c := range b {
		acc = acc<<8 | uint32(c)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out[n] = idAlphabet[(acc>>uint(bits))&31]
			n++
		}
	}
	out[n] = idAlphabet[(acc<<uint(5-bits))&31]
	return string(out[:])
}

// parseID checks s is "<prefix>_<26 alphabet chars>" whose padding bits are
// zero, so each id has exactly one string form.
func parseID(prefix, s string) error {
	body, ok := strings.CutPrefix(s, prefix+"_")
	if !ok {
		return invalidID(prefix, s, "want prefix "+prefix+"_")
	}
	if len(body) != idBodyLen {
		return invalidID(prefix, s, fmt.Sprintf("want %d characters after the prefix, got %d", idBodyLen, len(body)))
	}
	for i := range len(body) {
		if strings.IndexByte(idAlphabet, body[i]) < 0 {
			return invalidID(prefix, s, fmt.Sprintf("invalid character %q", body[i]))
		}
	}
	// 26*5 = 130 bits carry 128: the last character's low 2 bits must be 0.
	if strings.IndexByte(idAlphabet, body[idBodyLen-1])&3 != 0 {
		return invalidID(prefix, s, "non-canonical final character")
	}
	return nil
}

func invalidID(prefix, s, reason string) *Error {
	return &Error{
		Code:    CodeInvalidArgument,
		Message: fmt.Sprintf("invalid %s id %q: %s", prefix, s, reason),
		Details: map[string]string{"id": s, "kind": prefix},
	}
}
