package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	maxURLLen  = 4096
	maxNameLen = 48
)

var (
	// remoteHelper is git's "<transport>::<address>" form. ext:: runs a
	// command, so every helper form is refused.
	remoteHelper = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*::`)
	// scpLike is git's "[user@]host:path" form.
	scpLike = regexp.MustCompile(`^([A-Za-z0-9._-]+@)?[A-Za-z0-9.-]+:.`)

	allowedSchemes = map[string]bool{
		"ssh": true, "git": true, "http": true, "https": true, "file": true,
		"git+ssh": true, "ssh+git": true,
	}
)

// ValidateURL refuses a remote URL that git could treat as an option, that
// runs a command through a remote helper, that carries a credential (it
// would be written to .git/config on the node), or that is not an absolute
// location.
func ValidateURL(raw string) error {
	if raw == "" {
		return invalid("repository URL is empty")
	}
	if len(raw) > maxURLLen {
		return invalid("repository URL is too long")
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < 0x20 || raw[i] == 0x7f {
			return invalid("repository URL contains a control character")
		}
	}
	if raw[0] == '-' || raw[0] == '+' {
		return invalid("repository URL starts with '-' or '+'")
	}
	if remoteHelper.MatchString(raw) {
		return invalid("repository URL uses a remote helper transport")
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return invalid("repository URL does not parse")
		}
		if !allowedSchemes[strings.ToLower(u.Scheme)] {
			return invalid("repository URL scheme is not supported")
		}
		if u.User != nil {
			if _, ok := u.User.Password(); ok {
				return invalid("repository URL carries a password")
			}
			s := strings.ToLower(u.Scheme)
			if s == "http" || s == "https" {
				return invalid("repository URL carries a user name, which may be a token")
			}
		}
		return nil
	}
	if strings.HasPrefix(raw, "/") || filepath.IsAbs(raw) {
		return nil
	}
	if scpLike.MatchString(raw) {
		return nil
	}
	return invalid("repository URL is not absolute")
}

// DirName is the checkout's directory name for a remote URL: a readable part
// from the URL and a hash of the exact URL. It is always one path component,
// never "." or "..", and never starts with ".".
func DirName(raw string) (string, error) {
	if err := ValidateURL(raw); err != nil {
		return "", err
	}
	rest := raw
	if _, after, ok := strings.Cut(rest, "://"); ok {
		rest = after
		host, path, _ := strings.Cut(rest, "/")
		if i := strings.LastIndex(host, "@"); i >= 0 {
			host = host[i+1:]
		}
		rest = host + "/" + path
	} else if m := scpLike.FindStringSubmatch(rest); m != nil && m[1] != "" {
		rest = strings.TrimPrefix(rest, m[1])
	}
	var b strings.Builder
	for i := 0; i < len(rest) && b.Len() < maxNameLen; i++ {
		c := rest[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	name := b.String()
	if name == "" || !isAlnum(name[0]) {
		name = "r" + name
	}
	sum := sha256.Sum256([]byte(raw))
	return name + "-" + hex.EncodeToString(sum[:8]), nil
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// validateBranch refuses an empty branch, one git could read as an option or
// a forced refspec, and any name git rejects as a branch.
func validateBranch(branch string) error {
	if branch == "" {
		return invalid("branch is empty")
	}
	if branch[0] == '-' || branch[0] == '+' {
		return invalid("branch starts with '-' or '+'")
	}
	if strings.ContainsAny(branch, ":") {
		return invalid("branch contains ':'")
	}
	return nil
}
