//go:build !(linux || darwin)

package supervise

import (
	"io/fs"
	"os"

	"github.com/techspeque/gaia/internal/core"
)

const supported = false

const oNoFollow = 0

func fileUID(fs.FileInfo) (int, bool) { return 0, false }

func maxSocketPath() int { return 0 }

type child struct {
	out *os.File
	tty *os.File
	pid int
}

func startChild(Spec) (*child, error) {
	return nil, core.NewError(core.CodeUnavailable, "the session supervisor is not supported on this platform yet")
}

func (*child) wait() (*int, string)     { return nil, "" }
func (*child) terminate()               {}
func (*child) kill()                    {}
func (*child) resize(_, _ uint16) error { return core.NewError(core.CodeUnavailable, "unsupported") }
func isEndOfOutput(error) bool          { return false }
func ignoreHangup()                     {}

var stopSignals = []os.Signal{os.Interrupt}
