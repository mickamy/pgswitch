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
	goneDB    = "shop_test__fix_tax_708dc013"
	stagingDB = "pgswitch_staging_0123456789abcdef"
)

var sampleCandidates = []control.PruneCandidate{
	{
		Database: goneDB, Repo: filepath.FromSlash("/src/shop/.git"), Branch: "fix-tax", Requested: "shop_test",
		Reason: "branch gone",
	},
	{Database: stagingDB, Reason: "leftover staging"},
}

type fakePruner struct {
	candidates []control.PruneCandidate
	asked      []string
}

func (p *fakePruner) Candidates(context.Context) ([]control.PruneCandidate, error) {
	return p.candidates, nil
}

func (p *fakePruner) Drop(_ context.Context, databases []string) ([]control.PruneOutcome, error) {
	p.asked = databases
	outcomes := make([]control.PruneOutcome, len(databases))
	for i, db := range databases {
		outcomes[i] = control.PruneOutcome{Database: db, Dropped: db != stagingDB}
		if db == stagingDB {
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
	"REPO       BRANCH   DATABASE                           REASON\n" +
	filepath.FromSlash("/src/shop") + "  fix-tax  shop_test__fix_tax_708dc013        branch gone\n" +
	"-          -        pgswitch_staging_0123456789abcdef  leftover staging\n"

const wantOutcomes = "" +
	"dropped shop_test__fix_tax_708dc013\n" +
	"kept pgswitch_staging_0123456789abcdef (in use)\n"

func TestPrune(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd and StubTerminal a global
	tests := map[string]struct {
		args       []string
		terminal   bool
		input      string
		wantCode   int
		wantOut    string
		wantErr    string
		wantAsked  []string
		candidates []control.PruneCandidate
	}{
		"nothing to prune": {
			args:     []string{"prune"},
			wantCode: exit.OK,
			wantOut:  "nothing to prune\n",
		},
		"-yes drops without asking": {
			args:       []string{"prune", "-yes"},
			candidates: sampleCandidates,
			wantCode:   exit.OK,
			wantOut:    wantTable + wantOutcomes,
			wantAsked:  []string{goneDB, stagingDB},
		},
		"confirmed": {
			args:       []string{"prune"},
			terminal:   true,
			input:      "y\n",
			candidates: sampleCandidates,
			wantCode:   exit.OK,
			wantOut:    wantTable + wantOutcomes,
			wantErr:    "Drop 2 database(s)? [y/N] ",
			wantAsked:  []string{goneDB, stagingDB},
		},
		"declined": {
			args:       []string{"prune"},
			terminal:   true,
			input:      "\n",
			candidates: sampleCandidates,
			wantCode:   exit.OK,
			wantOut:    wantTable + "nothing dropped\n",
			wantErr:    "Drop 2 database(s)? [y/N] ",
		},
		"not a terminal and no -yes": {
			args:       []string{"prune"},
			candidates: sampleCandidates,
			wantCode:   exit.Error,
			wantErr:    "pgswitch prune: stdin is not a terminal; pass -yes to drop without asking\n",
		},
	}

	for name, tt := range tests { //nolint:paralleltest // t.Chdir changes the process cwd and StubTerminal a global
		t.Run(name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if tt.terminal {
				cli.StubTerminal(t)
			}
			p := &fakePruner{candidates: tt.candidates}
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
	servePrune(t, &fakePruner{candidates: sampleCandidates})

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
		{Database: goneDB, Dropped: true},
		{Database: stagingDB, Kept: "in use"},
	}, got.Outcomes)
}
