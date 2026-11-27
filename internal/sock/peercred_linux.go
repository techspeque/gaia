package sock

import "golang.org/x/sys/unix"

// maxPathLen is sun_path (108) less its terminating NUL.
const maxPathLen = 107

func fdPeerUID(fd int) (int, error) {
	cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0, err
	}
	return int(cred.Uid), nil
}
