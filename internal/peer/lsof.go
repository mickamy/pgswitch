package peer

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
)

// lsof -F output is one field per line, each prefixed by its type:
// p<pid>, f<fd>, n<name>.

// pidFromLsof returns the process whose socket name starts at client.
func pidFromLsof(out []byte, client string) (int, bool) {
	prefix := client + "->"
	pid := 0
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "p"); ok {
			pid, _ = strconv.Atoi(v)

			continue
		}
		if name, ok := strings.CutPrefix(line, "n"); ok && pid > 0 && strings.HasPrefix(name, prefix) {
			return pid, true
		}
	}

	return 0, false
}

// cwdFromLsof returns the name of the cwd file descriptor.
func cwdFromLsof(out []byte) (string, bool) {
	inCwd := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if fd, ok := strings.CutPrefix(line, "f"); ok {
			inCwd = fd == "cwd"

			continue
		}
		if name, ok := strings.CutPrefix(line, "n"); ok && inCwd {
			return name, true
		}
	}

	return "", false
}
