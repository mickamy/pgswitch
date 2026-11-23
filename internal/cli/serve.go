package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"

	"github.com/mickamy/pgswitch/internal/exit"
	"github.com/mickamy/pgswitch/internal/proxy"
	"github.com/mickamy/pgswitch/internal/route"
)

const (
	defaultListen        = "127.0.0.1:6432"
	defaultUpstream      = "127.0.0.1:5432"
	defaultDefaultBranch = "main"
)

func runServe(ctx context.Context, args []string, std IO) int {
	fs := newFlagSet("serve", std.Err, printServeUsage)
	listen := fs.String("listen", defaultListen, "address to accept clients on")
	upstream := fs.String("upstream", defaultUpstream, "address of the Postgres server")
	branch := fs.String("branch", defaultDefaultBranch, "branch every connection is routed to")
	defaultBranch := fs.String("default-branch", defaultDefaultBranch, "branch that keeps the original database names")
	if err := fs.Parse(args); err != nil {
		return exit.Usage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(std.Err, "%s serve: unexpected argument %q\n", Name, fs.Arg(0))

		return exit.Usage
	}

	log := slog.New(slog.NewTextHandler(std.Err, nil))

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", *listen)
	if err != nil {
		return fail(std, err)
	}

	router := proxy.RouterFunc(func(_ context.Context, _ net.Addr, database string) (string, error) {
		return route.PhysicalName(database, *branch, *defaultBranch), nil
	})

	log.Info("serving", "listen", ln.Addr().String(), "upstream", *upstream, "branch", *branch)
	if err := proxy.New(*upstream, router, log).Serve(ctx, ln); err != nil {
		return fail(std, err)
	}

	return exit.OK
}

func printServeUsage(w io.Writer) {
	fmt.Fprintf(w, "Usage: %s serve [flags]\n\n", Name)
	fmt.Fprint(w, "Run the proxy in the foreground until interrupted.\n\n")
	fmt.Fprint(w, "Flags:\n")
	fmt.Fprintf(w, "  -listen ADDR           address to accept clients on (default %q)\n", defaultListen)
	fmt.Fprintf(w, "  -upstream ADDR         address of the Postgres server (default %q)\n", defaultUpstream)
	fmt.Fprintf(w, "  -branch NAME           branch every connection is routed to (default %q)\n", defaultDefaultBranch)
	fmt.Fprintf(w, "  -default-branch NAME   branch that keeps the original database names (default %q)\n",
		defaultDefaultBranch)
}
