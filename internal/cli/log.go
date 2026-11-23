package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

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

	enc := json.NewEncoder(std.Out)
	var writeErr error
	emit := func(e events.Event) {
		if !*asJSON {
			fmt.Fprintln(std.Out, formatEvent(e))

			return
		}
		if err := enc.Encode(e); err != nil && writeErr == nil {
			writeErr = err
		}
	}
	if err := control.NewClient(*controlPath).Log(ctx, *follow, emit); err != nil {
		return fail(std, err)
	}
	if writeErr != nil {
		return fail(std, writeErr)
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

func quoteIfNeeded(v string) string {
	if v == "" || strings.ContainsAny(v, " \t\"=") {
		return fmt.Sprintf("%q", v)
	}

	return v
}
