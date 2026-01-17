//go:build unix

package traffic

import (
	"syscall"

	"golang.org/x/sys/unix"
)

func setSocketOptions(network, address string, c syscall.RawConn) error {
	var sysErr error
	err := c.Control(func(fd uintptr) {
		// Increase socket buffer sizes
		sysErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 4*1024*1024)
		if sysErr != nil {
			return
		}
		sysErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUF, 4*1024*1024)
		if sysErr != nil {
			return
		}
		// Enable TCP_NODELAY
		sysErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)
	})
	if err != nil {
		return err
	}
	return sysErr
}
