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
	// +c 0 keeps command names whole instead of cutting them at 9 characters.
	out, err := runLsof(ctx, "-nP", "+c", "0", "-iTCP@"+client.String(), "-sTCP:ESTABLISHED", "-Fpcn")
	if err != nil {
		return Process{}, err
	}
	pid, command, ok := processFromLsof(out, client.String(), server.String())
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

	return Process{PID: pid, Command: command, Cwd: cwd}, nil
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
