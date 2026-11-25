package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"

	"github.com/mickamy/pgswitch/internal/clone"
	"github.com/mickamy/pgswitch/internal/clone/pgclone"
	"github.com/mickamy/pgswitch/internal/control"
	"github.com/mickamy/pgswitch/internal/docker"
	"github.com/mickamy/pgswitch/internal/events"
	"github.com/mickamy/pgswitch/internal/exit"
	"github.com/mickamy/pgswitch/internal/listen"
	"github.com/mickamy/pgswitch/internal/pgadmin"
	"github.com/mickamy/pgswitch/internal/proxy"
	"github.com/mickamy/pgswitch/internal/prune"
	"github.com/mickamy/pgswitch/internal/router"
	"github.com/mickamy/pgswitch/internal/statedir"
	"github.com/mickamy/pgswitch/internal/upstream"
	"github.com/mickamy/pgswitch/internal/watch"
)

const (
	defaultListen   = "127.0.0.1:6432"
	defaultUpstream = "127.0.0.1:5432"
)

func runServe(ctx context.Context, args []string, std IO) int {
	fs := newFlagSet("serve", std.Err, printServeUsage)
	var explicit addrList
	fs.Var(&explicit, "listen", "address to accept clients on; repeatable")
	upstreamAddr := fs.String("upstream", defaultUpstream, "address of the Postgres server")
	adminUser := fs.String("admin-user", currentUser(), "role that creates branch databases; password from PGPASSWORD")
	passwordFile := fs.String("password-file", defaultPasswordFile(), "admin password when PGPASSWORD is unset")
	debug := fs.Bool("debug", false, "log how each connection is attributed")
	controlPath := fs.String("control", defaultControl(), "control socket for status, log and prune; empty to disable")
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

	dockerAPI, hasDocker := dockerClient(logger)
	var gateways, local []netip.Addr
	if len(explicit) == 0 && hasDocker {
		gateways, local = bridgeOnHost(ctx, dockerAPI, logger)
	}
	addrs, err := listen.Addresses(explicit, defaultListen, gateways, local)
	if err != nil {
		return fail(std, err)
	}
	lns, err := listen.Open(ctx, addrs, logger)
	if err != nil {
		return fail(std, err)
	}
	defer closeAll(lns)

	password := os.Getenv("PGPASSWORD")
	if password == "" {
		if password, err = readPassword(*passwordFile); err != nil {
			return fail(std, err)
		}
	}
	byDefault := upstream.Upstream{Addr: *upstreamAddr, AdminUser: *adminUser, AdminPassword: password}
	servers := upstream.NewPool(func(u upstream.Upstream) server {
		admin := pgclone.New(pgadmin.Config{Addr: u.Addr, User: u.AdminUser, Password: u.AdminPassword})

		return server{admin: admin, cloner: clone.New(admin, logger.With("upstream", u.Addr))}
	})

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
		p.Sever(c.Worktree, c.To)
	})
	servers.Get(byDefault) // known to prune from the start
	var finder router.Docker
	if hasDocker {
		finder = dockerAPI
	}
	r = router.New(logger, byDefault, func(u upstream.Upstream) router.Cloner { return servers.Get(u).cloner },
		finder, w)
	p = proxy.New(*upstreamAddr, r, logger)
	go w.Run(ctx)

	if *controlPath != "" {
		src := control.Source{
			Status: func() control.Status { return statusOf(p.Conns()) },
			Events: recent,
			Pruner: pruneSource{p: prune.New(logger, func() map[string]prune.Server {
				out := make(map[string]prune.Server)
				for addr, s := range servers.ByAddr() {
					out[addr] = prune.Server{Admin: s.admin, Dropper: s.cloner}
				}

				return out
			})},
		}
		stopped, err := serveControl(ctx, *controlPath, src, logger)
		if err != nil {
			return fail(std, err)
		}
		// Returning before the socket is closed would leave it to be removed
		// later, by whatever path it names by then.
		defer stopped()
	}

	listening := make([]string, len(lns))
	for i, ln := range lns {
		listening[i] = ln.Addr().String()
	}
	logger.Info("serving", "listen", listening, "upstream", *upstreamAddr, "admin_user", *adminUser)
	if err := serveAll(ctx, p, lns); err != nil {
		return fail(std, err)
	}

	return exit.OK
}

func printServeUsage(w io.Writer) {
	fmt.Fprintf(w, "Usage: %s serve [flags]\n\n", Name)
	fmt.Fprint(w, "Run the proxy in the foreground until interrupted.\n\n")
	fmt.Fprint(w, "Flags:\n")
	fmt.Fprint(w, "  -listen ADDR         address to accept clients on; repeatable\n")
	fmt.Fprintf(w, "                       (default %q, and the Docker bridge gateway when it is on this host)\n",
		defaultListen)
	fmt.Fprintf(w, "  -upstream ADDR       address of the Postgres server (default %q)\n", defaultUpstream)
	fmt.Fprint(w, "  -admin-user NAME     role that creates branch databases (default: the OS user)\n")
	fmt.Fprint(w, "                       its password is read from PGPASSWORD, else from -password-file\n")
	fmt.Fprint(w, "  -password-file PATH  the admin password, as service install keeps it; empty to disable\n")
	fmt.Fprint(w, "                       (default: in the state directory)\n")
	fmt.Fprint(w, "  -debug               log how each connection is attributed\n")
	fmt.Fprint(w, "  -control PATH        control socket for status, log and prune; empty to disable\n")
	fmt.Fprint(w, "                       (default: in the state directory)\n")
}

func defaultPasswordFile() string {
	path, err := statedir.PasswordFile()
	if err != nil {
		return ""
	}

	return path
}

func defaultControl() string {
	path, err := statedir.Socket()
	if err != nil {
		return ""
	}

	return path
}

// serveControl answers status, log and prune on the socket at path until ctx
// ends. The func it returns waits until the socket is closed.
func serveControl(ctx context.Context, path string, src control.Source, logger *slog.Logger) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}
	ln, err := control.Listen(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := control.Serve(ctx, ln, src); err != nil {
			logger.Warn("control socket", "err", err)
		}
	}()

	return func() { <-done }, nil
}

// dockerClient returns a client for the Docker API, unless there is no
// Docker to ask.
func dockerClient(logger *slog.Logger) (docker.Client, bool) {
	home, _ := os.UserHomeDir()
	socket, err := docker.Socket(docker.Env{Home: home, Getenv: os.Getenv})
	if err != nil {
		logger.Debug("not using Docker", "err", err)

		return docker.Client{}, false
	}

	return docker.NewClient(socket), true
}

// bridgeOnHost returns the Docker bridge's gateways and the host's addresses,
// so that serve also listens where host-gateway leads on native Docker on
// Linux. Both are empty when either cannot be read.
func bridgeOnHost(ctx context.Context, api docker.Client, logger *slog.Logger) (gateways, local []netip.Addr) {
	gateways, err := api.BridgeGateways(ctx)
	if err != nil {
		logger.Debug("not listening on the Docker bridge", "err", err)

		return nil, nil
	}
	local, err = listen.LocalAddrs()
	if err != nil {
		logger.Warn("not listening on the Docker bridge", "err", err)

		return nil, nil
	}

	return gateways, local
}

// serveAll runs p on every listener until ctx ends or one of them fails.
func serveAll(ctx context.Context, p proxy.Proxy, lns []net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make(chan error, len(lns))
	for _, ln := range lns {
		go func() {
			err := p.Serve(ctx, ln)
			cancel()
			errs <- err
		}()
	}
	var first error
	for range lns {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}

	return first
}

func closeAll(lns []net.Listener) {
	for _, ln := range lns {
		_ = ln.Close()
	}
}

// server is what pgswitch keeps per upstream: one cloner, so clones and drops
// there wait on one another.
type server struct {
	admin  pgclone.Admin
	cloner *clone.Cloner
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}

	return os.Getenv("USER")
}
