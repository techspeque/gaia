package core

import "time"

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
type RegisterNodeResponse struct {
	Node Node `json:"node"`
}

// HeartbeatRequest is the body of a node's heartbeat.
type HeartbeatRequest struct {
	ID NodeID `json:"id"`
}

// HeartbeatResponse is the body of a successful heartbeat.
type HeartbeatResponse struct {
	Node Node `json:"node"`
}
