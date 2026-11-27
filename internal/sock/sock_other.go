//go:build !linux && !darwin

package sock

import (
	"errors"
	"io/fs"
	"net"
	"os"
)

// The daemon roles do not run here yet (phase 0 plan); Listen refuses.
const supported = false

const maxPathLen = 103

var errUnsupported = errors.New("the local socket is not supported on this platform yet")

func lockFile(string) (*os.File, error) { return nil, errUnsupported }
func isRefused(error) bool              { return false }
func fileOwner(fs.FileInfo) (int, bool) { return 0, false }
func peerUID(net.Conn) (int, error)     { return 0, errUnsupported }
