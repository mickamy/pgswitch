package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/user"

	"github.com/mickamy/pgswitch/internal/clone"
	"github.com/mickamy/pgswitch/internal/clone/pgclone"
	"github.com/mickamy/pgswitch/internal/exit"
	"github.com/mickamy/pgswitch/internal/pgadmin"
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
	adminUser := fs.String("admin-user", currentUser(), "role that creates branch databases; password from PGPASSWORD")
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

	admin := pgclone.New(pgadmin.Config{Addr: *upstream, User: *adminUser, Password: os.Getenv("PGPASSWORD")})
	r := router.New(logger, clone.New(admin, logger))

	logger.Info("serving", "listen", ln.Addr().String(), "upstream", *upstream, "admin_user", *adminUser)
	if err := proxy.New(*upstream, r, logger).Serve(ctx, ln); err != nil {
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
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}

	return os.Getenv("USER")
}
