package peer

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const procRoot = "/proc"

func lookup(ctx context.Context, client, server netip.AddrPort) (Process, error) {
	inode, err := socketInode(client, server)
	if err != nil {
		return Process{}, err
	}
	pid, err := socketOwner(ctx, "socket:["+inode+"]")
	if err != nil {
		return Process{}, err
	}

	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	cwd, err := os.Readlink(filepath.Join(dir, "cwd"))
	if err != nil {
		return Process{}, fmt.Errorf("peer: %w", err)
	}
	comm, err := os.ReadFile(filepath.Join(dir, "comm"))
	if err != nil {
		return Process{}, fmt.Errorf("peer: %w", err)
	}

	return Process{PID: pid, Command: strings.TrimSpace(string(comm)), Cwd: cwd}, nil
}

// socketInode searches both tables: a dual-stack socket connecting over IPv4
// is listed in tcp6 with a mapped address.
func socketInode(client, server netip.AddrPort) (string, error) {
	for _, table := range []string{"tcp", "tcp6"} {
		content, err := os.ReadFile(filepath.Join(procRoot, "net", table))
		if err != nil {
			continue
		}
		if inode, ok := inodeFromProcNetTCP(content, client, server); ok {
			return inode, nil
		}
	}

	return "", ErrNotFound
}

func socketOwner(ctx context.Context, link string) (int, error) {
	procs, err := os.ReadDir(procRoot)
	if err != nil {
		return 0, fmt.Errorf("peer: %w", err)
	}

	for _, p := range procs {
		if err := ctx.Err(); err != nil {
			return 0, fmt.Errorf("peer: %w", err)
		}
		pid, err := strconv.Atoi(p.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join(procRoot, p.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue // another user's process, or gone
		}
		for _, fd := range fds {
			if target, err := os.Readlink(filepath.Join(fdDir, fd.Name())); err == nil && target == link {
				return pid, nil
			}
		}
	}

	return 0, ErrNotFound
}
