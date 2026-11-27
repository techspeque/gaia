package sock

import "golang.org/x/sys/unix"

// maxPathLen is sun_path (104) less its terminating NUL.
const maxPathLen = 103

func fdPeerUID(fd int) (int, error) {
	cred, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return 0, err
	}
	return int(cred.Uid), nil
}
