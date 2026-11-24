package cli

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/mickamy/pgswitch/internal/control"
	"github.com/mickamy/pgswitch/internal/exit"
	"github.com/mickamy/pgswitch/internal/prune"
)

func runPrune(ctx context.Context, args []string, std IO) int {
	fs := newFlagSet("prune", std.Err, printPruneUsage)
	controlPath := fs.String("control", defaultControl(), "control socket of the running serve")
	yes := fs.Bool("yes", false, "drop without asking")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return exit.Usage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(std.Err, "%s prune: unexpected argument %q\n", Name, fs.Arg(0))

		return exit.Usage
	}

	client := control.NewClient(*controlPath)
	report := pruneReport{}
	var err error
	if report.Candidates, err = client.PruneCandidates(ctx); err != nil {
		return fail(std, err)
	}
	if len(report.Candidates) == 0 {
		return finishPrune(std, report, *asJSON, "nothing to prune")
	}

	if !*yes {
		if !isTerminal(std.In) {
			fmt.Fprintf(std.Err, "%s prune: stdin is not a terminal; pass -yes to drop without asking\n", Name)

			return exit.Error
		}
		// With -json, stdout is kept for the JSON alone.
		table := std.Out
		if *asJSON {
			table = std.Err
		}
		printCandidates(table, report.Candidates)
		if !confirm(std, len(report.Candidates)) {
			return finishPrune(std, report, *asJSON, "nothing dropped")
		}
	} else if !*asJSON {
		printCandidates(std.Out, report.Candidates)
	}

	names := make([]string, len(report.Candidates))
	for i, c := range report.Candidates {
		names[i] = c.Database
	}
	if report.Outcomes, err = client.Prune(ctx, names); err != nil {
		return fail(std, err)
	}

	return finishPrune(std, report, *asJSON, "")
}

type pruneReport struct {
	Candidates []control.PruneCandidate `json:"candidates"`
	Outcomes   []control.PruneOutcome   `json:"outcomes,omitempty"`
}

// finishPrune prints report as JSON, or else its outcomes, or note when
// nothing was asked to be dropped.
func finishPrune(std IO, report pruneReport, asJSON bool, note string) int {
	if asJSON {
		enc := jsontext.NewEncoder(std.Out, jsontext.WithIndent("  "), jsontext.AllowInvalidUTF8(true))
		if err := json.MarshalEncode(enc, report); err != nil {
			return fail(std, err)
		}

		return exit.OK
	}
	if note != "" {
		fmt.Fprintln(std.Out, note)

		return exit.OK
	}
	for _, o := range report.Outcomes {
		if o.Dropped {
			fmt.Fprintf(std.Out, "dropped %s\n", o.Database)
		} else {
			fmt.Fprintf(std.Out, "kept %s (%s)\n", o.Database, o.Kept)
		}
	}

	return exit.OK
}

func printCandidates(w io.Writer, candidates []control.PruneCandidate) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REPO\tBRANCH\tDATABASE\tREASON")
	for _, c := range candidates {
		repo, branch := "-", "-"
		if c.Repo != "" {
			repo, branch = repoPath(c.Repo), c.Branch
		}
		fmt.Fprintln(tw, strings.Join([]string{repo, branch, c.Database, c.Reason}, "\t"))
	}
	_ = tw.Flush() // prune output: nothing useful to do with a write error
}

// repoPath shows a repository by its main worktree's directory when it has one.
func repoPath(id string) string {
	if filepath.Base(id) == ".git" {
		id = filepath.Dir(id)
	}

	return shortenHome(id)
}

func confirm(std IO, n int) bool {
	fmt.Fprintf(std.Err, "Drop %d database(s)? [y/N] ", n)
	answer, _ := bufio.NewReader(std.In).ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))

	return answer == "y" || answer == "yes"
}

var isTerminal = func(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()

	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func printPruneUsage(w io.Writer) {
	fmt.Fprintf(w, "Usage: %s prune [flags]\n\n", Name)
	fmt.Fprint(w, "Drop the branch databases whose branch or repository is gone, and copies\n")
	fmt.Fprint(w, "that never finished, after asking. A database still in use is kept.\n\n")
	fmt.Fprint(w, "Flags:\n")
	fmt.Fprint(w, "  -control PATH  control socket of the running serve (default: in the state directory)\n")
	fmt.Fprint(w, "  -yes           drop without asking\n")
	fmt.Fprint(w, "  -json          print JSON\n")
}

// pruneSource lets the control socket prune through p.
type pruneSource struct {
	p prune.Pruner
}

var _ control.Pruner = pruneSource{}

func (s pruneSource) Candidates(ctx context.Context) ([]control.PruneCandidate, error) {
	candidates, err := s.p.Candidates(ctx)
	if err != nil {
		return nil, err //nolint:wrapcheck // prune's errors say where they come from
	}
	out := make([]control.PruneCandidate, len(candidates))
	for i, c := range candidates {
		out[i] = control.PruneCandidate{
			Database:  c.Database,
			Repo:      c.Label.Repo,
			Branch:    c.Label.Branch,
			Requested: c.Label.Database,
			Reason:    string(c.Reason),
		}
	}

	return out, nil
}

func (s pruneSource) Drop(ctx context.Context, databases []string) ([]control.PruneOutcome, error) {
	outcomes, err := s.p.Drop(ctx, databases)
	if err != nil {
		return nil, err //nolint:wrapcheck // prune's errors say where they come from
	}
	out := make([]control.PruneOutcome, len(outcomes))
	for i, o := range outcomes {
		out[i] = control.PruneOutcome{Database: o.Database, Dropped: o.Dropped, Kept: o.Kept}
	}

	return out, nil
}
