package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/user"
	"path/filepath"

	"github.com/mickamy/pgswitch/internal/clone"
	"github.com/mickamy/pgswitch/internal/clone/pgclone"
	"github.com/mickamy/pgswitch/internal/control"
	"github.com/mickamy/pgswitch/internal/events"
	"github.com/mickamy/pgswitch/internal/exit"
	"github.com/mickamy/pgswitch/internal/pgadmin"
	"github.com/mickamy/pgswitch/internal/proxy"
	"github.com/mickamy/pgswitch/internal/router"
	"github.com/mickamy/pgswitch/internal/statedir"
	"github.com/mickamy/pgswitch/internal/watch"
)

const (
	defaultListen   = "127.0.0.1:6432"
	defaultUpstream = "127.0.0.1:5432"
)

func runServe(ctx context.Context, args []string, std IO) int {
	fs := newFlagSet("serve", std.Err, printServeUsage)
	listen := fs.String("listen", defaultListen, "address to accept clients on")
	upstream := fs.String("upstream", defaultUpstream, "address of the Postgres server")
	adminUser := fs.String("admin-user", currentUser(), "role that creates branch databases; password from PGPASSWORD")
	debug := fs.Bool("debug", false, "log how each connection is attributed")
	controlPath := fs.String("control", defaultControl(), "control socket for status; empty to disable")
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
	// What serve logs is also kept for pgswitch log.
	recent := events.New()
	logger := slog.New(slog.NewMultiHandler(
		slog.NewTextHandler(std.Err, &slog.HandlerOptions{Level: level}),
		events.NewHandler(recent),
	))

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", *listen)
	if err != nil {
		return fail(std, err)
	}

	admin := pgclone.New(pgadmin.Config{Addr: *upstream, User: *adminUser, Password: os.Getenv("PGPASSWORD")})

	// The watcher reports to the router and severs through the proxy, both of
	// which need the watcher; they are set before the watcher runs. A switch
	// is recorded before severing, so reconnections see where it came from.
	var (
		r router.Router
		p proxy.Proxy
	)
	w := watch.New(logger, func(c watch.Change) {
		logger.Info("switched", "worktree", c.Worktree, "from", c.From, "to", c.To)
		r.Switched(ctx, c.CommonDir, c.From, c.To)
		p.Sever(c.Worktree)
	})
	r = router.New(logger, clone.New(admin, logger), w)
	p = proxy.New(*upstream, r, logger)
	go w.Run(ctx)

	if *controlPath != "" {
		if err := serveControl(ctx, *controlPath, p, recent, logger); err != nil {
			return fail(std, err)
		}
	}

	logger.Info("serving", "listen", ln.Addr().String(), "upstream", *upstream, "admin_user", *adminUser)
	if err := p.Serve(ctx, ln); err != nil {
		return fail(std, err)
	}

	return exit.OK
}

func printServeUsage(w io.Writer) {
	fmt.Fprintf(w, "Usage: %s serve [flags]\n\n", Name)
	fmt.Fprint(w, "Run the proxy in the foreground until interrupted.\n\n")
	fmt.Fprint(w, "Flags:\n")
	fmt.Fprintf(w, "  -listen ADDR      address to accept clients on (default %q)\n", defaultListen)
	fmt.Fprintf(w, "  -upstream ADDR    address of the Postgres server (default %q)\n", defaultUpstream)
	fmt.Fprint(w, "  -admin-user NAME  role that creates branch databases (default: the OS user)\n")
	fmt.Fprint(w, "                    its password is read from PGPASSWORD\n")
	fmt.Fprint(w, "  -debug            log how each connection is attributed\n")
	fmt.Fprint(w, "  -control PATH     control socket for status; empty to disable (default: in the state directory)\n")
}

func defaultControl() string {
	path, err := statedir.Socket()
	if err != nil {
		return ""
	}

	return path
}

// serveControl answers pgswitch status on the socket at path until ctx ends.
func serveControl(ctx context.Context, path string, p proxy.Proxy, log *events.Log, logger *slog.Logger) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("control socket: %w", err)
	}
	ln, err := control.Listen(ctx, path)
	if err != nil {
		return fmt.Errorf("control socket: %w", err)
	}
	go func() {
		src := control.Source{Status: func() control.Status { return statusOf(p.Conns()) }, Events: log}
		if err := control.Serve(ctx, ln, src); err != nil {
			logger.Warn("control socket", "err", err)
		}
	}()

	return nil
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}

	return os.Getenv("USER")
}
