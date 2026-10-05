// Package callerpid finds the local process on the other end of a loopback TCP
// connection, so the API can tell which program called it (an agent the runner
// started is refused risky actions).
package callerpid

import (
	"errors"
	"fmt"
	"net/netip"
)

// ErrNotFound means no TCP connection matched the two endpoints (closed in
// the meantime, or not a local connection).
var ErrNotFound = errors.New("callerpid: connection not found")

// endpoints parses the server side's view of a connection: remoteAddr is the
// client's address (its local endpoint), localAddr ours. Both must be loopback.
func endpoints(remoteAddr, localAddr string) (client, server netip.AddrPort, err error) {
	client, err = netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return client, server, fmt.Errorf("callerpid: remote address %q: %w", remoteAddr, err)
	}
	server, err = netip.ParseAddrPort(localAddr)
	if err != nil {
		return client, server, fmt.Errorf("callerpid: local address %q: %w", localAddr, err)
	}
	client, server = unmap(client), unmap(server)
	if !client.Addr().IsLoopback() || !server.Addr().IsLoopback() {
		return client, server, fmt.Errorf("callerpid: %s -> %s is not a loopback connection", remoteAddr, localAddr)
	}
	return client, server, nil
}

func unmap(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(ap.Addr().Unmap().WithZone(""), ap.Port())
}
