package host

import (
	"bufio"
	"fmt"
	"io"
	"slices"
	"strings"

	gnet "github.com/techspeque/gaia/internal/net"
)

// Allowlist names the tailnet peers that may call the host API (ADR-0004
// rule 5). The zero Allowlist admits no one.
type Allowlist struct {
	entries []allowEntry
}

type allowKind int

const (
	allowLogin allowKind = iota // user@example.com
	allowTag                    // tag:gaia
	allowNode                   // node:<name>
)

type allowEntry struct {
	kind  allowKind
	value string // lower-cased, except tags, which match exactly
}

// ParseAllowlist parses entries, each a login name (user@example.com), a
// tag (tag:name) or a node name (node:name).
func ParseAllowlist(entries []string) (Allowlist, error) {
	var a Allowlist
	for _, e := range entries {
		if err := a.add(e); err != nil {
			return Allowlist{}, err
		}
	}
	return a, nil
}

// ReadAllowlist parses one entry per line from r, ignoring blank lines and
// everything after a #. name labels errors.
func ReadAllowlist(r io.Reader, name string) (Allowlist, error) {
	var a Allowlist
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line, _, _ := strings.Cut(sc.Text(), "#")
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		if err := a.add(line); err != nil {
			return Allowlist{}, fmt.Errorf("%s:%d: %w", name, n, err)
		}
	}
	if err := sc.Err(); err != nil {
		return Allowlist{}, fmt.Errorf("%s: %w", name, err)
	}
	return a, nil
}

// Merge returns a with b's entries appended.
func (a Allowlist) Merge(b Allowlist) Allowlist {
	return Allowlist{entries: append(slices.Clip(a.entries), b.entries...)}
}

// Len returns the number of entries.
func (a Allowlist) Len() int { return len(a.entries) }

func (a *Allowlist) add(e string) error {
	if e == "" || strings.ContainsFunc(e, isSpaceOrControl) {
		return fmt.Errorf("allowlist entry %q: empty or contains whitespace", e)
	}
	switch {
	case strings.HasPrefix(e, "tag:"):
		if !validLabel(e[len("tag:"):]) {
			return fmt.Errorf("allowlist entry %q: a tag is tag: and letters, digits or -", e)
		}
		a.entries = append(a.entries, allowEntry{allowTag, e})
	case strings.HasPrefix(e, "node:"):
		if !validLabel(e[len("node:"):]) {
			return fmt.Errorf("allowlist entry %q: a node is node: and letters, digits or -", e)
		}
		a.entries = append(a.entries, allowEntry{allowNode, strings.ToLower(e[len("node:"):])})
	case strings.Count(e, "@") == 1 && !strings.HasPrefix(e, "@") && !strings.HasSuffix(e, "@"):
		a.entries = append(a.entries, allowEntry{allowLogin, strings.ToLower(e)})
	default:
		return fmt.Errorf("allowlist entry %q: want a login (user@example.com), tag:name or node:name", e)
	}
	return nil
}

// Allows reports whether any entry matches p. Login and node names match
// case-insensitively; tags match exactly.
func (a Allowlist) Allows(p gnet.Peer) bool {
	for _, e := range a.entries {
		switch e.kind {
		case allowLogin:
			if p.LoginName != "" && strings.ToLower(p.LoginName) == e.value {
				return true
			}
		case allowNode:
			if p.NodeName != "" && strings.ToLower(p.NodeName) == e.value {
				return true
			}
		case allowTag:
			if slices.Contains(p.Tags, e.value) {
				return true
			}
		}
	}
	return false
}

func validLabel(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func isSpaceOrControl(r rune) bool { return r <= ' ' || r == 0x7f }
