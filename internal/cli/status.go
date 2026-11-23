package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/mickamy/pgswitch/internal/control"
	"github.com/mickamy/pgswitch/internal/exit"
	"github.com/mickamy/pgswitch/internal/proxy"
)

func runStatus(ctx context.Context, args []string, std IO) int {
	fs := newFlagSet("status", std.Err, printStatusUsage)
	controlPath := fs.String("control", defaultControl(), "control socket of the running serve")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return exit.Usage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(std.Err, "%s status: unexpected argument %q\n", Name, fs.Arg(0))

		return exit.Usage
	}

	s, err := control.NewClient(*controlPath).Status(ctx)
	if err != nil {
		return fail(std, err)
	}

	if *asJSON {
		enc := json.NewEncoder(std.Out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(s); err != nil {
			return fail(std, err)
		}

		return exit.OK
	}
	printStatus(std.Out, s)

	return exit.OK
}

func printStatusUsage(w io.Writer) {
	fmt.Fprintf(w, "Usage: %s status [flags]\n\n", Name)
	fmt.Fprint(w, "Show the connections a running serve is relaying: which worktree and branch\n")
	fmt.Fprint(w, "each came from and which database it landed on.\n\n")
	fmt.Fprint(w, "Flags:\n")
	fmt.Fprint(w, "  -control PATH  control socket of the running serve (default: in the state directory)\n")
	fmt.Fprint(w, "  -json          print JSON\n")
}

// statusOf turns the proxy's connections into what status reports.
func statusOf(conns []proxy.Conn) control.Status {
	s := control.Status{Connections: make([]control.Connection, 0, len(conns))}
	for _, c := range conns {
		s.Connections = append(s.Connections, control.Connection{
			Repo:      c.Repo,
			Worktree:  c.Owner,
			Branch:    c.Branch,
			Database:  c.Database,
			Requested: c.Requested,
			PID:       c.PID,
			Command:   c.Command,
			Client:    c.Client,
			Since:     c.Since,
		})
	}

	return s
}

type statusRow struct {
	repo, worktree, branch, database string
}

// printStatus prints one line per repository, worktree, branch and database,
// with how many connections share it.
func printStatus(w io.Writer, s control.Status) {
	if len(s.Connections) == 0 {
		fmt.Fprintln(w, "no connections")

		return
	}

	counts := make(map[statusRow]int)
	for _, c := range s.Connections {
		counts[rowOf(c)]++
	}
	rows := make([]statusRow, 0, len(counts))
	for r := range counts {
		rows = append(rows, r)
	}
	slices.SortFunc(rows, func(a, b statusRow) int {
		return cmp.Or(
			cmp.Compare(a.repo, b.repo),
			cmp.Compare(a.worktree, b.worktree),
			cmp.Compare(a.database, b.database),
		)
	})

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REPO\tWORKTREE\tBRANCH\tDATABASE\tCONNS")
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join([]string{r.repo, r.worktree, r.branch, r.database, strconv.Itoa(counts[r])}, "\t"))
	}
	_ = tw.Flush() // status output: nothing useful to do with a write error
}

func rowOf(c control.Connection) statusRow {
	r := statusRow{repo: c.Repo, worktree: shortenHome(c.Worktree), branch: c.Branch, database: c.Database}
	switch {
	case c.Worktree == "":
		r.repo, r.worktree, r.branch = "-", "(no worktree)", "-"
	case c.Branch == "":
		r.branch = "(detached)"
	}

	return r
}

func shortenHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.Join("~", rel)
	}

	return path
}
