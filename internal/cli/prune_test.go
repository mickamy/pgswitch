package cli_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/cli"
	"github.com/mickamy/pgswitch/internal/control"
	"github.com/mickamy/pgswitch/internal/events"
	"github.com/mickamy/pgswitch/internal/exit"
)

const (
	shopUpstream = "127.0.0.1:5433"
	goneDB       = "shop_test__fix_tax_708dc013"
	stagingDB    = "pgswitch_staging_0123456789abcdef"
)

var sampleCandidates = []control.PruneCandidate{
	{
		Upstream: shopUpstream, Database: goneDB, Repo: filepath.FromSlash("/src/shop/.git"), Branch: "fix-tax",
		Requested: "shop_test", Reason: "branch gone",
	},
	{Upstream: "127.0.0.1:5432", Database: stagingDB, Reason: "leftover staging"},
}

var sampleTargets = []control.PruneTarget{
	{Upstream: shopUpstream, Database: goneDB},
	{Upstream: "127.0.0.1:5432", Database: stagingDB},
}

type fakePruner struct {
	listing control.PruneListing
	asked   []control.PruneTarget
}

func (p *fakePruner) Candidates(context.Context) (control.PruneListing, error) {
	return p.listing, nil
}

func (p *fakePruner) Drop(_ context.Context, targets []control.PruneTarget) ([]control.PruneOutcome, error) {
	p.asked = targets
	outcomes := make([]control.PruneOutcome, len(targets))
	for i, t := range targets {
		outcomes[i] = control.PruneOutcome{Upstream: t.Upstream, Database: t.Database, Dropped: t.Database != stagingDB}
		if t.Database == stagingDB {
			outcomes[i].Kept = "in use"
		}
	}

	return outcomes, nil
}

func servePrune(t *testing.T, p *fakePruner) {
	t.Helper()

	ln, err := control.Listen(t.Context(), socket)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	src := control.Source{Status: func() control.Status { return control.Status{} }, Events: events.New(), Pruner: p}
	go func() { done <- control.Serve(ctx, ln, src) }()
	t.Cleanup(func() {
		cancel()
		assert.NoError(t, <-done)
	})
}

func runCLIWithInput(t *testing.T, input string, args ...string) (code int, stdout, stderr string) {
	t.Helper()

	var out, errOut bytes.Buffer
	code = cli.Run(t.Context(), args, cli.IO{In: strings.NewReader(input), Out: &out, Err: &errOut})

	return code, out.String(), errOut.String()
}

var wantTable = "" +
	"UPSTREAM        REPO       BRANCH   DATABASE                           REASON\n" +
	"127.0.0.1:5433  " + filepath.FromSlash("/src/shop") + "  fix-tax  shop_test__fix_tax_708dc013        branch gone\n" +
	"127.0.0.1:5432  -          -        pgswitch_staging_0123456789abcdef  leftover staging\n"

const wantOutcomes = "" +
	"dropped shop_test__fix_tax_708dc013 on 127.0.0.1:5433\n" +
	"kept pgswitch_staging_0123456789abcdef on 127.0.0.1:5432 (in use)\n"

func TestPrune(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd and StubTerminal a global
	tests := map[string]struct {
		args      []string
		terminal  bool
		input     string
		wantCode  int
		wantOut   string
		wantErr   string
		wantAsked []control.PruneTarget
		listing   control.PruneListing
	}{
		"nothing to prune": {
			args:     []string{"prune"},
			wantCode: exit.OK,
			wantOut:  "nothing to prune\n",
		},
		"-yes drops without asking": {
			args:      []string{"prune", "-yes"},
			listing:   control.PruneListing{Candidates: sampleCandidates},
			wantCode:  exit.OK,
			wantOut:   wantTable + wantOutcomes,
			wantAsked: sampleTargets,
		},
		"confirmed": {
			args:      []string{"prune"},
			terminal:  true,
			input:     "y\n",
			listing:   control.PruneListing{Candidates: sampleCandidates},
			wantCode:  exit.OK,
			wantOut:   wantTable + wantOutcomes,
			wantErr:   "Drop 2 database(s)? [y/N] ",
			wantAsked: sampleTargets,
		},
		"declined": {
			args:     []string{"prune"},
			terminal: true,
			input:    "\n",
			listing:  control.PruneListing{Candidates: sampleCandidates},
			wantCode: exit.OK,
			wantOut:  wantTable + "nothing dropped\n",
			wantErr:  "Drop 2 database(s)? [y/N] ",
		},
		"an upstream that could not be looked at": {
			args: []string{"prune"},
			listing: control.PruneListing{
				Skipped: []control.PruneSkipped{{Upstream: "127.0.0.1:5434", Error: "connection refused"}},
			},
			wantCode: exit.OK,
			wantOut:  "nothing to prune\n",
			wantErr:  "pgswitch prune: skipped 127.0.0.1:5434: connection refused\n",
		},
		"not a terminal and no -yes": {
			args:     []string{"prune"},
			listing:  control.PruneListing{Candidates: sampleCandidates},
			wantCode: exit.Error,
			wantErr:  "pgswitch prune: stdin is not a terminal; pass -yes to drop without asking\n",
		},
	}

	for name, tt := range tests { //nolint:paralleltest // t.Chdir changes the process cwd and StubTerminal a global
		t.Run(name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if tt.terminal {
				cli.StubTerminal(t)
			}
			p := &fakePruner{listing: tt.listing}
			servePrune(t, p)

			code, out, stderr := runCLIWithInput(t, tt.input, append(tt.args, "-control", socket)...)

			assert.Equal(t, tt.wantCode, code)
			assert.Equal(t, tt.wantOut, out)
			assert.Equal(t, tt.wantErr, stderr)
			assert.Equal(t, tt.wantAsked, p.asked)
		})
	}
}

func TestPruneJSON(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd and StubTerminal a global
	t.Chdir(t.TempDir())
	cli.StubTerminal(t)
	servePrune(t, &fakePruner{listing: control.PruneListing{Candidates: sampleCandidates}})

	code, out, stderr := runCLIWithInput(t, "yes\n", "prune", "-json", "-control", socket)
	require.Equal(t, exit.OK, code, stderr)

	assert.Equal(t, wantTable+"Drop 2 database(s)? [y/N] ", stderr, "the table goes to stderr, leaving stdout to JSON")
	var got struct {
		Candidates []control.PruneCandidate `json:"candidates"`
		Outcomes   []control.PruneOutcome   `json:"outcomes"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, sampleCandidates, got.Candidates)
	assert.Equal(t, []control.PruneOutcome{
		{Upstream: shopUpstream, Database: goneDB, Dropped: true},
		{Upstream: "127.0.0.1:5432", Database: stagingDB, Kept: "in use"},
	}, got.Outcomes)
}
