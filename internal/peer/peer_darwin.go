package peer

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"strconv"
)

// lsof exits with 1 when nothing matches.
const lsofNoMatch = 1

func lookup(ctx context.Context, client, server netip.AddrPort) (Process, error) {
	out, err := runLsof(ctx, "-nP", "-iTCP@"+client.String(), "-sTCP:ESTABLISHED", "-Fpn")
	if err != nil {
		return Process{}, err
	}
	pid, ok := pidFromLsof(out, client.String(), server.String())
	if !ok {
		return Process{}, ErrNotFound
	}

	out, err = runLsof(ctx, "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn")
	if err != nil {
		return Process{}, err
	}
	cwd, ok := cwdFromLsof(out)
	if !ok {
		return Process{}, fmt.Errorf("peer: no cwd for pid %d", pid)
	}

	return Process{PID: pid, Cwd: cwd}, nil
}

func runLsof(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "lsof", args...).Output()
	if exitErr := (*exec.ExitError)(nil); errors.As(err, &exitErr) && exitErr.ExitCode() == lsofNoMatch {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("peer: lsof: %w", err)
	}

	return out, nil
}
