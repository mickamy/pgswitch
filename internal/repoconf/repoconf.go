package repoconf

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// File is the name of a repository's pgswitch configuration, committed at
// the root of each worktree.
const File = ".pgswitch.toml"

// Config is what a repository sets for itself. Empty fields fall back to the
// daemon's defaults.
type Config struct {
	// Upstream is the Postgres server the repository's databases live on, a
	// loopback host:port.
	Upstream      string
	AdminUser     string
	AdminPassword string
}

// Load reads the configuration of the worktree at root. ok is false when it
// has none.
func Load(root string) (cfg Config, ok bool, err error) {
	path := filepath.Join(root, File)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, false, nil
	}
	if err != nil {
		return Config{}, false, fmt.Errorf("repoconf: %w", err)
	}

	cfg, err = Parse(b)
	if err != nil {
		return Config{}, false, fmt.Errorf("repoconf: %s:%w", path, err)
	}

	return cfg, true, nil
}

// Parse reads a configuration written in the subset of TOML pgswitch knows:
// key = "string" lines, comments and blank lines. Its errors start with the
// line number.
func Parse(b []byte) (Config, error) {
	var cfg Config
	fields := map[string]*string{
		"upstream":       &cfg.Upstream,
		"admin_user":     &cfg.AdminUser,
		"admin_password": &cfg.AdminPassword,
	}
	seen := make(map[string]bool)

	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		key, value, ok, err := parseLine(sc.Text())
		if err != nil {
			return Config{}, fmt.Errorf("%d: %w", n, err)
		}
		if !ok {
			continue
		}
		field, known := fields[key]
		if !known {
			return Config{}, fmt.Errorf("%d: unknown key %q", n, key)
		}
		if seen[key] {
			return Config{}, fmt.Errorf("%d: %s is set twice", n, key)
		}
		seen[key] = true
		if key == "upstream" {
			if err := checkUpstream(value); err != nil {
				return Config{}, fmt.Errorf("%d: %w", n, err)
			}
		}
		*field = value
	}
	if err := sc.Err(); err != nil {
		return Config{}, fmt.Errorf("%w", err)
	}

	return cfg, nil
}

// parseLine reads one key = "value" line. ok is false for a blank or comment
// line.
func parseLine(line string) (key, value string, ok bool, err error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false, nil
	}
	if strings.HasPrefix(line, "[") {
		return "", "", false, errors.New("tables are not supported")
	}

	key, rest, found := strings.Cut(line, "=")
	if !found {
		return "", "", false, errors.New(`want key = "value"`)
	}
	key = strings.TrimSpace(key)
	if !isBareKey(key) {
		return "", "", false, fmt.Errorf("invalid key %q", key)
	}

	value, rest, err = parseString(strings.TrimSpace(rest))
	if err != nil {
		return "", "", false, fmt.Errorf("%s: %w", key, err)
	}
	if rest = strings.TrimSpace(rest); rest != "" && !strings.HasPrefix(rest, "#") {
		return "", "", false, fmt.Errorf("%s: unexpected %q after the value", key, rest)
	}

	return key, value, true, nil
}

func isBareKey(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		switch {
		case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}

	return true
}

// parseString reads a basic ("...") or literal ('...') string at the start of
// s and returns what follows it.
func parseString(s string) (value, rest string, err error) {
	switch {
	case strings.HasPrefix(s, "'"):
		end := strings.IndexByte(s[1:], '\'')
		if end < 0 {
			return "", "", errors.New("unterminated string")
		}

		return s[1 : 1+end], s[2+end:], nil
	case strings.HasPrefix(s, `"`):
		return parseBasicString(s[1:])
	default:
		return "", "", errors.New("the value must be a quoted string")
	}
}

func parseBasicString(s string) (value, rest string, err error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			return b.String(), s[i+1:], nil
		case '\\':
			if i+1 >= len(s) {
				return "", "", errors.New("unterminated string")
			}
			i++
			switch s[i] {
			case '"', '\\':
				b.WriteByte(s[i])
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'u', 'U':
				width := 4
				if s[i] == 'U' {
					width = 8
				}
				if i+width >= len(s) {
					return "", "", errors.New("short unicode escape")
				}
				code, err := strconv.ParseUint(s[i+1:i+1+width], 16, 32)
				r := rune(code) //nolint:gosec // ParseUint keeps it to 32 bits; ValidRune rejects the rest
				if err != nil || !utf8.ValidRune(r) {
					return "", "", fmt.Errorf("invalid unicode escape %q", s[i-1:i+1+width])
				}
				b.WriteRune(r)
				i += width
			default:
				return "", "", fmt.Errorf("invalid escape \\%c", s[i])
			}
		default:
			b.WriteByte(c)
		}
	}

	return "", "", errors.New("unterminated string")
}

// checkUpstream accepts only a loopback host:port: a cloned repository must
// not point pgswitch at another machine.
func checkUpstream(upstream string) error {
	host, port, err := net.SplitHostPort(upstream)
	if err != nil {
		return fmt.Errorf("upstream: %w", err)
	}
	if p, err := strconv.ParseUint(port, 10, 16); err != nil || p == 0 {
		return fmt.Errorf("upstream: invalid port %q", port)
	}
	if host == "localhost" {
		return nil
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !addr.IsLoopback() {
		return fmt.Errorf("upstream: %q is not a loopback address", host)
	}

	return nil
}
