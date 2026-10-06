// Command key-policy-home serves the cpa-key-policy management API on top of a
// CLIProxyAPIHome cluster. See README-home.md for configuration.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"cpa-key-policy/internal/homeadapter"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	args := os.Args[1:]
	command := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	var err error
	switch command {
	case "serve":
		err = serve()
	case "migrate":
		err = migrate(args)
	case "version":
		fmt.Println(version)
	default:
		err = fmt.Errorf("unknown command %q (serve, migrate, version)", command)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "key-policy-home:", err)
		os.Exit(1)
	}
}

type settings struct {
	listen   []string
	homes    []string
	homeKey  string
	tokens   []string
	location *time.Location
	prefix   string
	cacheTTL time.Duration
}

func load(requireTokens bool) (settings, error) {
	s := settings{
		listen:  split(env("KP_LISTEN", "127.0.0.1:18340")),
		homes:   split(os.Getenv("KP_HOME_URLS")),
		homeKey: strings.TrimSpace(os.Getenv("KP_HOME_KEY")),
		tokens:  split(os.Getenv("KP_TOKENS")),
		prefix:  env("KP_USER_PREFIX", "kp_"),
	}
	location, err := time.LoadLocation(env("KP_TIMEZONE", "Asia/Shanghai"))
	if err != nil {
		return s, err
	}
	s.location = location
	if s.cacheTTL, err = time.ParseDuration(env("KP_CACHE_TTL", "30s")); err != nil {
		return s, fmt.Errorf("KP_CACHE_TTL: %w", err)
	}
	switch {
	case len(s.homes) == 0:
		return s, errors.New("KP_HOME_URLS is required")
	case s.homeKey == "":
		return s, errors.New("KP_HOME_KEY is required")
	case requireTokens && len(s.tokens) == 0:
		return s, errors.New("KP_TOKENS is required")
	}
	return s, nil
}

func (s settings) service() *homeadapter.Service {
	client := homeadapter.NewClient(s.homes, s.homeKey, 20*time.Second)
	return homeadapter.NewService(client, s.prefix, s.location, s.cacheTTL)
}

func serve() error {
	s, err := load(true)
	if err != nil {
		return err
	}
	logger := log.New(os.Stdout, "", log.LstdFlags)
	server := &http.Server{
		Handler:           homeadapter.NewAPI(s.service(), s.tokens, logger),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errs := make(chan error, len(s.listen))
	for _, address := range s.listen {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			return err
		}
		logger.Printf("key-policy-home %s listening on %s", version, address)
		go func() { errs <- server.Serve(listener) }()
	}
	select {
	case <-ctx.Done():
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}

func migrate(args []string) error {
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	state := flags.String("state", "", "cpa-key-policy state file (cpa-key-policy-state.json)")
	usage := flags.String("usage", "", "cpa-key-policy usage file; keeps each key's weekly reset weekday")
	plaintext := flags.String("plaintext", "", "comma-separated files holding key values (JSON or one key per line)")
	apply := flags.Bool("apply", false, "write to Home; without it only the plan is printed")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *state == "" || *plaintext == "" {
		return errors.New("--state and --plaintext are required")
	}
	s, err := load(false)
	if err != nil {
		return err
	}
	var progress io.Writer
	if *apply {
		progress = os.Stderr
	}
	report, err := homeadapter.Migrate(context.Background(), s.service(), homeadapter.MigrateOptions{
		StatePath: *state, UsagePath: *usage, PlaintextPaths: split(*plaintext), Apply: *apply,
	}, progress)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return err
	}
	if len(report.Failed) > 0 {
		return fmt.Errorf("%d keys failed", len(report.Failed))
	}
	return nil
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func split(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
