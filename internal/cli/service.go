package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/mickamy/pgswitch/internal/control"
	"github.com/mickamy/pgswitch/internal/exit"
	"github.com/mickamy/pgswitch/internal/service"
	"github.com/mickamy/pgswitch/internal/statedir"
)

const (
	serviceStartTimeout = 10 * time.Second
	serviceStartPoll    = 200 * time.Millisecond
)

// serviceDeps is what service install and uninstall reach outside the
// process for; tests replace it.
type serviceDeps struct {
	manager      func() (service.Manager, error)
	binary       func() (string, error)
	socket       func() (string, error)
	passwordFile func() (string, error)
	logFile      func() (string, error)
	startTimeout time.Duration
}

var services = serviceDeps{
	manager: func() (service.Manager, error) {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("service: %w", err)
		}

		return service.New(service.Env{GOOS: runtime.GOOS, Home: home, UID: os.Getuid(), Getenv: os.Getenv}, service.Exec)
	},
	binary: func() (string, error) {
		self, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("service: %w", err)
		}

		return service.Binary(self, exec.LookPath, os.TempDir())
	},
	socket:       statedir.Socket,
	passwordFile: statedir.PasswordFile,
	logFile:      statedir.LogFile,
	startTimeout: serviceStartTimeout,
}

func runService(ctx context.Context, args []string, std IO) int {
	if len(args) == 0 {
		printServiceUsage(std.Err)

		return exit.Usage
	}

	switch args[0] {
	case "install":
		return runServiceInstall(ctx, args[1:], std)
	case "uninstall":
		return runServiceUninstall(ctx, args[1:], std)
	default:
		fmt.Fprintf(std.Err, "%s service: unknown subcommand %q\n\n", Name, args[0])
		printServiceUsage(std.Err)

		return exit.Usage
	}
}

func runServiceInstall(ctx context.Context, args []string, std IO) int {
	fs := newFlagSet("service install", std.Err, printServiceUsage)
	listen := fs.String("listen", defaultListen, "address to accept clients on")
	upstreamAddr := fs.String("upstream", defaultUpstream, "address of the default Postgres server")
	adminUser := fs.String("admin-user", currentUser(), "role that creates branch databases there")
	debug := fs.Bool("debug", false, "log how each connection is attributed")
	if err := fs.Parse(args); err != nil {
		return exit.Usage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(std.Err, "%s service install: unexpected argument %q\n", Name, fs.Arg(0))

		return exit.Usage
	}

	m, err := services.manager()
	if err != nil {
		return fail(std, err)
	}
	binary, err := services.binary()
	if err != nil {
		return fail(std, err)
	}
	// Resolved here and handed to serve: the service manager's environment,
	// XDG_STATE_HOME included, need not be this shell's.
	logFile, err := services.logFile()
	if err != nil {
		return fail(std, err)
	}
	socket, err := services.socket()
	if err != nil {
		return fail(std, err)
	}
	passwordFile, err := services.passwordFile()
	if err != nil {
		return fail(std, err)
	}
	// launchd opens the log before pgswitch runs, so its directory must exist.
	if err := os.MkdirAll(filepath.Dir(logFile), 0o700); err != nil {
		return fail(std, fmt.Errorf("service: %w", err))
	}

	if password := os.Getenv("PGPASSWORD"); password != "" {
		if err := writePassword(passwordFile, password); err != nil {
			return fail(std, err)
		}
		fmt.Fprintf(std.Out, "  kept the admin password from PGPASSWORD in %s\n", passwordFile)
	}

	serveArgs := []string{
		"serve", "-listen", *listen, "-upstream", *upstreamAddr, "-admin-user", *adminUser,
		"-control", socket, "-password-file", passwordFile,
	}
	if *debug {
		serveArgs = append(serveArgs, "-debug")
	}
	if err := m.Install(ctx, service.Spec{Binary: binary, Args: serveArgs, LogPath: logFile}); err != nil {
		return fail(std, err)
	}

	if err := waitForServe(ctx, socket); err != nil {
		return fail(std, fmt.Errorf("%w; %s", err, whereLogsAre(logFile)))
	}
	fmt.Fprintf(std.Out, "✔ pgswitch is running on %s, forwarding to %s\n", *listen, *upstreamAddr)

	return exit.OK
}

func runServiceUninstall(ctx context.Context, args []string, std IO) int {
	fs := newFlagSet("service uninstall", std.Err, printServiceUsage)
	if err := fs.Parse(args); err != nil {
		return exit.Usage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(std.Err, "%s service uninstall: unexpected argument %q\n", Name, fs.Arg(0))

		return exit.Usage
	}

	m, err := services.manager()
	if err != nil {
		return fail(std, err)
	}
	removed, err := m.Uninstall(ctx)
	if err != nil {
		return fail(std, err)
	}
	if removed {
		fmt.Fprintf(std.Out, "✔ pgswitch is stopped and %s is removed\n", m.UnitPath())
	} else {
		fmt.Fprintf(std.Out, "pgswitch was not installed as a service (%s)\n", m.UnitPath())
	}
	if path, err := services.passwordFile(); err == nil {
		if _, err := os.Stat(path); err == nil {
			fmt.Fprintf(std.Out, "  the admin password stays in %s; delete it if serve no longer needs it\n", path)
		}
	}

	return exit.OK
}

// waitForServe waits until a serve answers on socket.
func waitForServe(ctx context.Context, socket string) error {
	ctx, cancel := context.WithTimeout(ctx, services.startTimeout)
	defer cancel()

	client := control.NewClient(socket)
	for {
		if _, err := client.Status(ctx); err == nil {
			return nil
		}
		select {
		case <-time.After(serviceStartPoll):
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("service: pgswitch did not answer on %s within %s", socket, services.startTimeout)
			}

			return fmt.Errorf("service: %w", ctx.Err())
		}
	}
}

func whereLogsAre(logFile string) string {
	if runtime.GOOS == "linux" {
		return "see journalctl --user -u " + service.UnitName
	}

	return "see " + logFile
}

func printServiceUsage(w io.Writer) {
	fmt.Fprintf(w, "Usage: %s service install [flags]\n", Name)
	fmt.Fprintf(w, "       %s service uninstall\n\n", Name)
	fmt.Fprint(w, "Run serve at login: a launchd agent on macOS, a systemd user unit on Linux.\n")
	fmt.Fprint(w, "Installing again rewrites and restarts it. PGPASSWORD, when set, is kept in a\n")
	fmt.Fprint(w, "file only you can read, in the state directory.\n\n")
	fmt.Fprint(w, "Flags for install:\n")
	fmt.Fprintf(w, "  -listen ADDR      address to accept clients on (default %q)\n", defaultListen)
	fmt.Fprintf(w, "  -upstream ADDR    address of the default Postgres server (default %q)\n", defaultUpstream)
	fmt.Fprint(w, "  -admin-user NAME  role that creates branch databases there (default: the OS user)\n")
	fmt.Fprint(w, "  -debug            log how each connection is attributed\n")
}
