package core

import "strings"

// SessionTTYPath is where a client attaches to a session's terminal: a
// WebSocket (GET with an upgrade).
//
// Binary frames carry raw bytes: the session's output from server to
// client, input from client to server. Text frames carry one JSON
// TTYMessage each. The server's first message is TTYHello. The server
// closes with status 1000 (normal closure) once the session has ended and
// every byte of its output was sent; any other end is a dropped
// connection. A refusal before the upgrade is the usual JSON error.
func SessionTTYPath(id SessionID) string { return SessionPath(id) + "/tty" }

// NodeSessionTTYPath is the same on the node API, for the host.
func NodeSessionTTYPath(id SessionID) string { return NodeSessionPath(id) + "/tty" }

// TTYMessage types.
const (
	// TTYHello is the server's first message: the session's mode and the
	// scrollback offset of the first output byte that follows.
	TTYHello = "hello"
	// TTYResize is a client's terminal size.
	TTYResize = "resize"
	// TTYNotice is a message from the server for the person attached, such
	// as input refused on a headless session.
	TTYNotice = "notice"
)

// TTYCloseNormal is the WebSocket close status a terminal ends with when
// the session ended and all its output was sent.
const TTYCloseNormal = 1000

// TTYMessage is one text frame of a terminal WebSocket.
type TTYMessage struct {
	Type    string      `json:"type"`
	Mode    SessionMode `json:"mode,omitempty"`    // hello
	Offset  int64       `json:"offset"`            // hello
	Rows    uint16      `json:"rows,omitempty"`    // resize
	Cols    uint16      `json:"cols,omitempty"`    // resize
	Message string      `json:"message,omitempty"` // notice
}

// CheckOrigin admits a terminal WebSocket request with the Origin header
// origin to a server reached as host (the request's Host): an absent
// origin (the CLI and the host's own calls send none) or one whose host is
// host, compared without case. Anything else, an unparsable or "null"
// origin included, is CodeForbidden: a browser page from another origin
// must not drive a session.
func CheckOrigin(origin, host string) error {
	if origin == "" {
		return nil
	}
	// An origin is scheme "://" host [":" port], nothing more (RFC 6454).
	scheme, h, ok := strings.Cut(origin, "://")
	scheme = strings.ToLower(scheme)
	if ok && (scheme == "http" || scheme == "https") && h != "" &&
		!strings.ContainsAny(h, "/?#@ \t\r\n") && strings.EqualFold(h, host) {
		return nil
	}
	return &Error{
		Code:    CodeForbidden,
		Message: "terminal connections are accepted only from the gaia CLI and from the host's own pages",
		Details: map[string]string{"origin": origin[:min(len(origin), 200)]},
	}
}
