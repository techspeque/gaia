package core

import (
	"strconv"
	"time"
)

// APIVersion is the host HTTP API version this binary speaks. It changes
// only on a breaking change to the API.
const APIVersion = 1

// APIPrefix is the path prefix of every host API route.
const APIPrefix = "/api/v1"

// NodesPath is the collection of registered nodes.
const NodesPath = APIPrefix + "/nodes"

// VersionPath reports the daemon's API and Gaia versions.
const VersionPath = APIPrefix + "/version"

// HealthzPath answers whether the daemon is up. It is outside /api/v1 and
// reveals nothing about the machine.
const HealthzPath = "/healthz"

// HealthResponse is the body of GET /healthz. Status is always "ok".
type HealthResponse struct {
	Status string `json:"status"`
}

// VersionResponse is the body of GET /api/v1/version.
type VersionResponse struct {
	APIVersion  int    `json:"api_version"`
	GaiaVersion string `json:"gaia_version"`
}

// Node is a registered machine as the API returns it (OVERVIEW §2.2).
type Node struct {
	ID              NodeID    `json:"id"`
	Name            string    `json:"name"`
	TailnetIdentity string    `json:"tailnet_identity"`
	OS              string    `json:"os"`
	Arch            string    `json:"arch"`
	GaiaVersion     string    `json:"gaia_version"`
	APIVersion      int       `json:"api_version"`
	State           NodeState `json:"state"`
	LastSeen        time.Time `json:"last_seen"`
}

// ListNodesResponse is the body of GET /api/v1/nodes.
type ListNodesResponse struct {
	Nodes []Node `json:"nodes"`
}

// RegisterNodeRequest is the body of POST /api/v1/nodes. Registering a known
// tailnet identity again updates that node.
type RegisterNodeRequest struct {
	Name            string `json:"name"`
	TailnetIdentity string `json:"tailnet_identity"`
	OS              string `json:"os"`
	Arch            string `json:"arch"`
	GaiaVersion     string `json:"gaia_version"`
	APIVersion      int    `json:"api_version"`
}

// RegisterNodeResponse is the body of a successful registration.
// HeartbeatIntervalMS is how often the host expects the node's heartbeat.
type RegisterNodeResponse struct {
	Node                Node  `json:"node"`
	HeartbeatIntervalMS int64 `json:"heartbeat_interval_ms"`
}

// HeartbeatPath is where a node POSTs its heartbeat.
const HeartbeatPath = APIPrefix + "/heartbeat"

// HeartbeatRequest is the body of a node's heartbeat.
type HeartbeatRequest struct {
	ID NodeID `json:"id"`
}

// HeartbeatResponse is the body of a successful heartbeat.
type HeartbeatResponse struct {
	Node Node `json:"node"`
}

// RemoveNodeResponse is the body of a successful DELETE on NodePath.
type RemoveNodeResponse struct {
	Node Node `json:"node"`
}

// NodePath is the resource of the node named name. Valid names need no
// escaping in a path (ValidNodeName).
func NodePath(name string) string { return NodesPath + "/" + name }

// maxNodeNameLen is the length limit of a DNS label.
const maxNodeNameLen = 63

// ValidNodeName returns a CodeInvalidArgument error unless name has the
// form of a DNS label: 1 to 63 of a-z, 0-9 and '-', not starting or ending
// with '-'.
func ValidNodeName(name string) error {
	bad := func(reason string) error {
		return &Error{
			Code:    CodeInvalidArgument,
			Message: "invalid node name " + quote(name) + ": " + reason,
			Details: map[string]string{"name": name},
		}
	}
	switch {
	case name == "":
		return bad("empty")
	case len(name) > maxNodeNameLen:
		return bad("longer than 63 characters")
	case name[0] == '-' || name[len(name)-1] == '-':
		return bad("starts or ends with '-'")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return bad("use only a-z, 0-9 and '-'")
		}
	}
	return nil
}

// quote quotes s for a message, cut to 80 bytes.
func quote(s string) string {
	if len(s) > 80 {
		s = s[:80]
	}
	return strconv.Quote(s)
}
