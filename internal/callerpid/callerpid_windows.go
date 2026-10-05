package callerpid

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"unsafe"

	"golang.org/x/sys/windows"
)

// GetExtendedTcpTable is not wrapped by golang.org/x/sys/windows.
var procGetExtendedTCPTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

const tcpTableOwnerPIDConnections = 4 // TCP_TABLE_OWNER_PID_CONNECTIONS

// Row sizes: MIB_TCPROW_OWNER_PID (6 DWORDs) and MIB_TCP6ROW_OWNER_PID
// (16-byte address, scope, port twice, then state and pid).
const (
	row4Size = 24
	row6Size = 56
)

// LoopbackPID returns the process that owns the client end of the loopback
// connection a server sees as remoteAddr -> localAddr (http.Request.RemoteAddr
// and the http.LocalAddrContextKey address): the row whose local endpoint is
// the client's and whose remote endpoint is ours. IPv4 and IPv6 tables are
// searched (a dual-stack socket lists an IPv4 peer as ::ffff:a.b.c.d).
func LoopbackPID(remoteAddr, localAddr string) (uint32, error) {
	client, server, err := endpoints(remoteAddr, localAddr)
	if err != nil {
		return 0, err
	}
	for _, af := range []uint32{windows.AF_INET, windows.AF_INET6} {
		buf, err := tcpTable(af)
		if err != nil {
			return 0, err
		}
		if pid, ok := match(buf, af, client, server); ok {
			return pid, nil
		}
	}
	return 0, ErrNotFound
}

// tcpTable reads the TCP connection table with owning pids for family af.
func tcpTable(af uint32) ([]byte, error) {
	size := uint32(16 << 10)
	for range 8 {
		buf := make([]byte, size)
		r, _, _ := procGetExtendedTCPTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, //nolint:gosec // G103: documented out buffer
			uintptr(af), tcpTableOwnerPIDConnections, 0)
		switch windows.Errno(r) {
		case windows.ERROR_SUCCESS:
			return buf[:min(int(size), len(buf))], nil
		case windows.ERROR_INSUFFICIENT_BUFFER:
			size += 4 << 10 // connections may appear between the calls
		default:
			return nil, fmt.Errorf("callerpid: GetExtendedTcpTable(af %d): %w", af, windows.Errno(r))
		}
	}
	return nil, errors.New("callerpid: GetExtendedTcpTable: table keeps growing")
}

// port reads a DWORD port field (network byte order in its low 16 bits).
func port(b []byte) uint16 { return binary.BigEndian.Uint16(b[:2]) }

// match finds the row local == client, remote == server in a table buffer.
func match(buf []byte, af uint32, client, server netip.AddrPort) (uint32, bool) {
	if len(buf) < 4 {
		return 0, false
	}
	n := int(binary.LittleEndian.Uint32(buf))
	rows := buf[4:]
	size := row4Size
	if af == windows.AF_INET6 {
		size = row6Size
	}
	for i := 0; i < n && (i+1)*size <= len(rows); i++ {
		r := rows[i*size : (i+1)*size]
		var local, remote netip.AddrPort
		var pid uint32
		if af == windows.AF_INET {
			local = netip.AddrPortFrom(netip.AddrFrom4([4]byte(r[4:8])), port(r[8:12]))
			remote = netip.AddrPortFrom(netip.AddrFrom4([4]byte(r[12:16])), port(r[16:20]))
			pid = binary.LittleEndian.Uint32(r[20:24])
		} else {
			local = netip.AddrPortFrom(netip.AddrFrom16([16]byte(r[0:16])), port(r[20:24]))
			remote = netip.AddrPortFrom(netip.AddrFrom16([16]byte(r[24:40])), port(r[44:48]))
			pid = binary.LittleEndian.Uint32(r[52:56])
		}
		if unmap(local) == client && unmap(remote) == server {
			return pid, true
		}
	}
	return 0, false
}
