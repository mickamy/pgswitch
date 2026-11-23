package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"

	"github.com/mickamy/pgswitch/internal/exit"
	"github.com/mickamy/pgswitch/internal/proxy"
	"github.com/mickamy/pgswitch/internal/router"
)

const (
	defaultListen   = "127.0.0.1:6432"
	defaultUpstream = "127.0.0.1:5432"
)

func runServe(ctx context.Context, args []string, std IO) int {
	fs := newFlagSet("serve", std.Err, printServeUsage)
	listen := fs.String("listen", defaultListen, "address to accept clients on")
	upstream := fs.String("upstream", defaultUpstream, "address of the Postgres server")
	debug := fs.Bool("debug", false, "log how each connection is attributed")
	if err := fs.Parse(args); err != nil {
		return exit.Usage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(std.Err, "%s serve: unexpected argument %q\n", Name, fs.Arg(0))

		return exit.Usage
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(std.Err, &slog.HandlerOptions{Level: level}))

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", *listen)
	if err != nil {
		return fail(std, err)
	}

	logger.Info("serving", "listen", ln.Addr().String(), "upstream", *upstream)
	if err := proxy.New(*upstream, router.New(logger), logger).Serve(ctx, ln); err != nil {
		return fail(std, err)
	}

	return exit.OK
}

func printServeUsage(w io.Writer) {
	fmt.Fprintf(w, "Usage: %s serve [flags]\n\n", Name)
	fmt.Fprint(w, "Run the proxy in the foreground until interrupted.\n\n")
	fmt.Fprint(w, "Flags:\n")
	fmt.Fprintf(w, "  -listen ADDR    address to accept clients on (default %q)\n", defaultListen)
	fmt.Fprintf(w, "  -upstream ADDR  address of the Postgres server (default %q)\n", defaultUpstream)
	fmt.Fprint(w, "  -debug          log how each connection is attributed\n")
}
