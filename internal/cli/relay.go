package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"

	"github.com/mickamy/pgswitch/internal/exit"
	"github.com/mickamy/pgswitch/internal/relay"
)

const (
	defaultRelayListen = ":5432"
	defaultRelayTo     = "host.docker.internal:6432"
)

func runRelay(ctx context.Context, args []string, std IO) int {
	fs := newFlagSet("relay", std.Err, printRelayUsage)
	listen := fs.String("listen", defaultRelayListen, "address to accept the container's clients on")
	to := fs.String("to", defaultRelayTo, "address of pgswitch serve on the host")
	container := fs.String("container", "", "ID of the container the clients run in (default: this one's)")
	if err := fs.Parse(args); err != nil {
		return exit.Usage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(std.Err, "%s relay: unexpected argument %q\n", Name, fs.Arg(0))

		return exit.Usage
	}

	id := *container
	if id == "" {
		var err error
		if id, err = ownContainerID(); err != nil {
			return fail(std, err)
		}
	}

	logger := slog.New(slog.NewTextHandler(std.Err, nil))
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", *listen)
	if err != nil {
		return fail(std, err)
	}
	logger.Info("relaying", "listen", ln.Addr().String(), "to", *to, "container", id)
	if err := relay.Serve(ctx, ln, *to, id, logger); err != nil {
		return fail(std, err)
	}

	return exit.OK
}

func ownContainerID() (string, error) {
	var mountinfo io.Reader = strings.NewReader("")
	if f, err := os.Open("/proc/self/mountinfo"); err == nil {
		defer func() { _ = f.Close() }()
		mountinfo = f
	}
	hostname, _ := os.Hostname()

	return relay.ContainerID(mountinfo, hostname) //nolint:wrapcheck // relay's errors say where they come from
}

func printRelayUsage(w io.Writer) {
	fmt.Fprintf(w, "Usage: %s relay [flags]\n\n", Name)
	fmt.Fprint(w, "Run inside a container, in place of Postgres for its neighbors: relay each\n")
	fmt.Fprint(w, "connection to pgswitch serve on the host, saying which container it comes from,\n")
	fmt.Fprint(w, "so it is routed by the worktree Compose ran in.\n\n")
	fmt.Fprint(w, "Flags:\n")
	fmt.Fprintf(w, "  -listen ADDR    address to accept the container's clients on (default %q)\n", defaultRelayListen)
	fmt.Fprintf(w, "  -to ADDR        address of pgswitch serve on the host (default %q)\n", defaultRelayTo)
	fmt.Fprint(w, "  -container ID   the container the clients run in (default: this one's)\n")
}
