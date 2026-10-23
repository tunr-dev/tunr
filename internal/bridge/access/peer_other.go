//go:build !linux

package access

import "net"

// PeerCheckSupported reports whether SameUserOnly is enforced on this OS.
const PeerCheckSupported = false

func peerAllowed(net.Conn) (bool, error) { return true, nil }
