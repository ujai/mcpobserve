// Command mcpobserve wraps an MCP stdio server and exposes Prometheus metrics
// about the JSON-RPC traffic flowing through it.
//
// Usage:
//
//	mcpobserve [flags] -- <server-command> [server-args...]
//
// Example (wrapping a filesystem MCP server):
//
//	mcpobserve --metrics-addr 127.0.0.1:9464 -- npx -y @modelcontextprotocol/server-filesystem /tmp
//
// In an MCP client config, replace the server's command with mcpobserve and
// move the original command after `--`. The client talks to mcpobserve over
// stdio exactly as before; metrics appear at http://127.0.0.1:9464/metrics.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/ujai/mcpobserve/internal/metrics"
	"github.com/ujai/mcpobserve/internal/proxy"
)

// version is overridden at release time via
// -ldflags "-X main.version=v0.x.y".
var version = "0.1.0"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	cfg, rest, err := parseFlags(argv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcpobserve:", err)
		usage()
		return 2
	}
	if cfg.showVersion {
		fmt.Println("mcpobserve", version)
		return 0
	}
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "mcpobserve: no server command given after `--`")
		usage()
		return 2
	}

	// Structured event log goes to stderr by default, or a file if requested.
	// It must never go to stdout, which carries the JSON-RPC protocol.
	var logDst io.Writer = os.Stderr
	if cfg.logFile != "" {
		f, err := os.OpenFile(cfg.logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mcpobserve: cannot open log file:", err)
			return 1
		}
		defer f.Close()
		logDst = f
	}
	level := slog.LevelInfo
	if cfg.quiet {
		level = slog.LevelWarn
	}
	log := slog.New(slog.NewJSONHandler(logDst, &slog.HandlerOptions{Level: level}))

	reg := metrics.New()

	// Metrics HTTP server. Bound to loopback by default for safety; there is
	// no auth, so a non-loopback bind deserves a loud warning.
	if !isLoopbackAddr(cfg.metricsAddr) {
		log.Warn("metrics endpoint bound to a non-loopback address; it has no authentication — firewall it",
			"addr", cfg.metricsAddr)
	}
	srv := &http.Server{
		Addr:              cfg.metricsAddr,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if err := reg.WritePrometheus(w); err != nil {
			log.Warn("metrics write failed", "err", err)
		}
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	srv.Handler = mux

	go func() {
		log.Info("metrics endpoint listening", "addr", cfg.metricsAddr, "path", "/metrics")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("metrics server stopped", "err", err)
		}
	}()

	// SIGINT/SIGTERM cancel the context, which makes the proxy SIGTERM the
	// wrapped server (SIGKILL after a grace period) and unwind. The metrics
	// server is also stopped so the process can exit cleanly if run directly.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	p := proxy.New(reg, log)
	if cfg.maxMessageBytes > 0 {
		p.MaxObserveBytes = cfg.maxMessageBytes
	}
	code, err := p.Run(ctx, rest[0], rest[1:], os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		log.Error("failed to start server", "err", err)
		return 1
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	return code
}

type config struct {
	metricsAddr     string
	logFile         string
	quiet           bool
	showVersion     bool
	maxMessageBytes int
}

// isLoopbackAddr reports whether the listen address resolves to loopback.
// Unparseable or hostname binds are treated as non-loopback (warn-worthy).
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// parseFlags performs minimal flag parsing so we can cleanly split our flags
// from the wrapped server command at the `--` separator. The standard flag
// package stops at the first non-flag arg, but being explicit keeps the `--`
// contract obvious to users.
func parseFlags(argv []string) (config, []string, error) {
	cfg := config{metricsAddr: "127.0.0.1:9464"}
	i := 0
	for ; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--":
			return cfg, argv[i+1:], nil
		case a == "-h" || a == "--help":
			usage()
			os.Exit(0)
		case a == "--version" || a == "-v":
			cfg.showVersion = true
		case a == "--quiet" || a == "-q":
			cfg.quiet = true
		case a == "--metrics-addr":
			if i+1 >= len(argv) {
				return cfg, nil, fmt.Errorf("--metrics-addr needs a value")
			}
			i++
			cfg.metricsAddr = argv[i]
		case a == "--log-file":
			if i+1 >= len(argv) {
				return cfg, nil, fmt.Errorf("--log-file needs a value")
			}
			i++
			cfg.logFile = argv[i]
		case a == "--max-message-bytes":
			if i+1 >= len(argv) {
				return cfg, nil, fmt.Errorf("--max-message-bytes needs a value")
			}
			i++
			n, err := strconv.Atoi(argv[i])
			if err != nil || n <= 0 {
				return cfg, nil, fmt.Errorf("--max-message-bytes must be a positive integer, got %q", argv[i])
			}
			cfg.maxMessageBytes = n
		case len(a) > 0 && a[0] == '-':
			return cfg, nil, fmt.Errorf("unknown flag %q", a)
		default:
			// First non-flag without `--` is treated as the start of the
			// server command, for convenience.
			return cfg, argv[i:], nil
		}
	}
	return cfg, nil, nil
}

func usage() {
	fmt.Fprint(os.Stderr, `mcpobserve `+version+` — observability proxy for MCP stdio servers

USAGE:
  mcpobserve [flags] -- <server-command> [server-args...]

FLAGS:
  --metrics-addr ADDR        Address for the Prometheus endpoint (default 127.0.0.1:9464)
  --log-file PATH            Write structured JSON event log here (default stderr)
  --max-message-bytes N      Parse messages up to N bytes for metrics; larger ones
                             are forwarded untouched but counted as oversize
                             (default 67108864 = 64 MiB)
  --quiet, -q                Only log warnings and errors
  --version, -v              Print version and exit
  --help, -h                 Show this help

EXAMPLE:
  mcpobserve -- npx -y @modelcontextprotocol/server-filesystem /tmp
  # then scrape http://127.0.0.1:9464/metrics
`)
}
