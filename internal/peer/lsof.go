package peer

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
)

// lsof -F output is one field per line, each prefixed by its type:
// p<pid>, c<command>, f<fd>, n<name>.

// processFromLsof returns the pid and command of the process whose socket
// connects client to server.
func processFromLsof(out []byte, client, server string) (pid int, command string, ok bool) {
	want := client + "->" + server
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if v, isPID := strings.CutPrefix(line, "p"); isPID {
			pid, _ = strconv.Atoi(v)
			command = ""

			continue
		}
		if v, isCommand := strings.CutPrefix(line, "c"); isCommand {
			command = v

			continue
		}
		if name, isName := strings.CutPrefix(line, "n"); isName && pid > 0 && name == want {
			return pid, command, true
		}
	}

	return 0, "", false
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
