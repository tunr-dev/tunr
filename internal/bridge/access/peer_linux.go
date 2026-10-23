//go:build linux

package access

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// PeerCheckSupported reports whether SameUserOnly is enforced on this OS.
const PeerCheckSupported = true

// peerAllowed reports whether the local process on the other end of c runs
// as the same user as this one (or root). It finds the client's socket in
// /proc/net/tcp{,6}: its local address is c's remote address and its remote
// address is our listening address.
func peerAllowed(c net.Conn) (bool, error) {
	peer, ok1 := c.RemoteAddr().(*net.TCPAddr)
	self, ok2 := c.LocalAddr().(*net.TCPAddr)
	if !ok1 || !ok2 {
		return false, fmt.Errorf("not a TCP connection")
	}
	file := "/proc/net/tcp"
	if peer.IP.To4() == nil {
		file = "/proc/net/tcp6"
	}
	uid, err := socketUID(file, procAddr(peer), procAddr(self))
	if err != nil {
		return false, err
	}
	me := os.Getuid()
	return uid == me || uid == 0, nil
}

// procAddr formats an address the way /proc/net/tcp prints it: each 32-bit
// word of the network-order address read in host order, then the port.
func procAddr(a *net.TCPAddr) string {
	ip := a.IP.To4()
	if ip == nil {
		ip = a.IP.To16()
	}
	var b strings.Builder
	for i := 0; i < len(ip); i += 4 {
		fmt.Fprintf(&b, "%08X", binary.NativeEndian.Uint32(ip[i:i+4]))
	}
	fmt.Fprintf(&b, ":%04X", a.Port)
	return b.String()
}

func socketUID(file, local, remote string) (int, error) {
	f, err := os.Open(file)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) < 8 || fs[1] != local || fs[2] != remote {
			continue
		}
		return strconv.Atoi(fs[7])
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("peer socket %s not found", local)
}
