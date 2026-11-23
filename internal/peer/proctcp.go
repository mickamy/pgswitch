package peer

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"net/netip"
	"strconv"
	"strings"
)

const (
	tcpEstablished = "01"
	fieldLocal     = 1
	fieldState     = 3
	fieldInode     = 9
)

// inodeFromProcNetTCP returns the inode of the established socket bound to
// client in the content of /proc/net/tcp or /proc/net/tcp6.
func inodeFromProcNetTCP(content []byte, client netip.AddrPort) (string, bool) {
	sc := bufio.NewScanner(bytes.NewReader(content))
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) <= fieldInode || fields[fieldState] != tcpEstablished {
			continue
		}
		local, ok := parseProcAddr(fields[fieldLocal])
		if ok && local == client {
			return fields[fieldInode], true
		}
	}

	return "", false
}

// parseProcAddr parses "0100007F:1F90": the address in host byte order, 32 bits
// at a time, then the port in hex.
func parseProcAddr(s string) (netip.AddrPort, bool) {
	hexIP, hexPort, ok := strings.Cut(s, ":")
	if !ok {
		return netip.AddrPort{}, false
	}
	raw, err := hex.DecodeString(hexIP)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.AddrPort{}, false
	}
	port, err := strconv.ParseUint(hexPort, 16, 16)
	if err != nil {
		return netip.AddrPort{}, false
	}

	for i := 0; i < len(raw); i += 4 {
		binary.BigEndian.PutUint32(raw[i:], binary.NativeEndian.Uint32(raw[i:]))
	}
	addr, _ := netip.AddrFromSlice(raw)

	return netip.AddrPortFrom(addr.Unmap(), uint16(port)), true
}
