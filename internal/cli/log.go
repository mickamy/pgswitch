package cli

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/mickamy/pgswitch/internal/control"
	"github.com/mickamy/pgswitch/internal/events"
	"github.com/mickamy/pgswitch/internal/exit"
)

const logTimeFormat = "15:04:05"

func runLog(ctx context.Context, args []string, std IO) int {
	fs := newFlagSet("log", std.Err, printLogUsage)
	controlPath := fs.String("control", defaultControl(), "control socket of the running serve")
	follow := fs.Bool("f", false, "keep printing new events until interrupted")
	asJSON := fs.Bool("json", false, "print JSON lines")
	if err := fs.Parse(args); err != nil {
		return exit.Usage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(std.Err, "%s log: unexpected argument %q\n", Name, fs.Arg(0))

		return exit.Usage
	}

	// A failed write ends the stream at once rather than after the next
	// event, or never with -f; it is told apart from an interrupt by writeErr.
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	enc := jsontext.NewEncoder(std.Out, jsontext.AllowInvalidUTF8(true))
	var writeErr error
	emit := func(e events.Event) {
		if writeErr != nil {
			return
		}
		var err error
		if *asJSON {
			err = json.MarshalEncode(enc, e)
		} else {
			_, err = fmt.Fprintln(std.Out, formatEvent(e))
		}
		if err != nil {
			writeErr = err
			stop()
		}
	}
	err := control.NewClient(*controlPath).Log(ctx, *follow, emit)
	if writeErr != nil {
		return fail(std, writeErr)
	}
	if err != nil {
		return fail(std, err)
	}

	return exit.OK
}

func printLogUsage(w io.Writer) {
	fmt.Fprintf(w, "Usage: %s log [flags]\n\n", Name)
	fmt.Fprint(w, "Show the latest events of a running serve: connections routed, databases\n")
	fmt.Fprint(w, "cloned and renamed, worktrees switched.\n\n")
	fmt.Fprint(w, "Flags:\n")
	fmt.Fprint(w, "  -control PATH  control socket of the running serve (default: in the state directory)\n")
	fmt.Fprint(w, "  -f             keep printing new events until interrupted\n")
	fmt.Fprint(w, "  -json          print JSON lines\n")
}

// formatEvent prints an event on one line: its time, its level unless INFO,
// its message and its attributes by key.
func formatEvent(e events.Event) string {
	parts := []string{e.Time.Format(logTimeFormat)}
	if e.Level != "INFO" {
		parts = append(parts, e.Level)
	}
	parts = append(parts, e.Message)
	for _, k := range slices.Sorted(maps.Keys(e.Attrs)) {
		parts = append(parts, k+"="+quoteIfNeeded(e.Attrs[k]))
	}

	return strings.Join(parts, "  ")
}

// quoteIfNeeded quotes a value that would otherwise blur the line: empty, or
// with spaces, control characters such as newlines, quotes or equal signs.
func quoteIfNeeded(v string) string {
	needs := v == "" || strings.ContainsFunc(v, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || r == '"' || r == '='
	})
	if needs {
		return strconv.Quote(v)
	}

	return v
}
