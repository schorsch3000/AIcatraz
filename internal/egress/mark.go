// SPDX-License-Identifier: GPL-3.0-only

package egress

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// Mark is the socket mark that the sidecar's own connections carry. The
// firewall accepts marked packets and does not redirect them; the jail can't
// set a mark (it needs NET_ADMIN, which only the sidecar has).
const Mark = 1

// markedDialer returns a dialer whose sockets carry mark (none if 0).
func markedDialer(mark int) *net.Dialer {
	d := &net.Dialer{}
	if mark != 0 {
		d.Control = func(network, address string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) {
				serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, mark)
			}); err != nil {
				return err
			}
			return serr
		}
	}
	return d
}
