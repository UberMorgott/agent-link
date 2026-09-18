// Command agentlink runs an agent-to-agent messaging node and talks to its
// local control API.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/selfupdate"
)

const usage = `usage:
  agentlink serve --config <path>
  agentlink send  --config <path> [--to <node|area:NAME>] --body <text> [--reply-to <id>]   (no --to: the only peer)
  agentlink wait  --config <path> [--timeout 0]    (seconds or duration; 0 = forever; exit 2 on timeout)
  agentlink inbox --config <path> [--limit 50]
  agentlink update [--check]    (install the latest GitHub release next to this program; --check only reports)
  agentlink version`

// exitTimeout is returned by wait when no message arrived in time.
const exitTimeout = 2

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, usage)
		return 1
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	switch args[0] {
	case "version":
		_, _ = fmt.Fprintln(stdout, selfupdate.Version)
		return 0
	case "update":
		check := fs.Bool("check", false, "only report whether a newer release exists")
		if err := fs.Parse(args[1:]); err != nil {
			return 1
		}
		if err := update(*check, stdout); err != nil {
			_, _ = fmt.Fprintln(stderr, "agentlink:", err)
			return 1
		}
		return 0
	}
	cfgPath := fs.String("config", "", "config file")
	var cmd func(config.Config) (int, error)
	switch args[0] {
	case "serve":
		cmd = func(c config.Config) (int, error) { return 0, serve(c) }
	case "send":
		to := fs.String("to", "", "node name or area:NAME; empty sends to the only known peer")
		body := fs.String("body", "", "message text")
		replyTo := fs.String("reply-to", "", "id of the message being answered")
		cmd = func(c config.Config) (int, error) { return 0, send(c, *to, *body, *replyTo, stdout) }
	case "wait":
		timeout := fs.String("timeout", "0", "seconds or Go duration; 0 waits forever")
		cmd = func(c config.Config) (int, error) { return wait(c, *timeout, stdout) }
	case "inbox":
		limit := fs.Int("limit", 50, "maximum entries")
		cmd = func(c config.Config) (int, error) { return 0, inbox(c, *limit, stdout) }
	default:
		_, _ = fmt.Fprintln(stderr, usage)
		return 1
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}
	if *cfgPath == "" {
		_, _ = fmt.Fprintln(stderr, "--config is required")
		return 1
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	code, err := cmd(cfg)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "agentlink:", err)
		return 1
	}
	return code
}

func serve(cfg config.Config) error {
	secret, err := cfg.Secret()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	n, err := node.New(cfg, secret, log)
	if err != nil {
		return err
	}
	peerLn, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	apiLn, err := net.Listen("tcp", cfg.API)
	if err != nil {
		_ = peerLn.Close()
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return n.Serve(ctx, peerLn, apiLn)
}

func send(cfg config.Config, to, body, replyTo string, stdout io.Writer) error {
	if body == "" {
		return errors.New("--body is required")
	}
	req, err := json.Marshal(node.SendRequest{To: to, Body: body, ReplyTo: replyTo})
	if err != nil {
		return err
	}
	resp, err := http.Post(apiURL(cfg, "/send", nil), "application/json", bytes.NewReader(req))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := checkStatus(resp, http.StatusOK); err != nil {
		return err
	}
	var m node.Message
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, m.ID)
	return err
}

func wait(cfg config.Config, timeout string, stdout io.Writer) (int, error) {
	d, err := parseTimeout(timeout)
	if err != nil {
		return 1, err
	}
	resp, err := http.Get(apiURL(cfg, "/wait", url.Values{"timeout": {d.String()}}))
	if err != nil {
		return 1, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent {
		return exitTimeout, nil
	}
	if err := checkStatus(resp, http.StatusOK); err != nil {
		return 1, err
	}
	return 0, printLines[node.Message](resp.Body, stdout)
}

func inbox(cfg config.Config, limit int, stdout io.Writer) error {
	resp, err := http.Get(apiURL(cfg, "/inbox", url.Values{"limit": {strconv.Itoa(limit)}}))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := checkStatus(resp, http.StatusOK); err != nil {
		return err
	}
	return printLines[node.Entry](resp.Body, stdout)
}

// update reports the latest release and, unless check, installs it over this
// program and agentlink-tray next to it. A running tray keeps the old version
// until it restarts.
func update(check bool, stdout io.Writer) error {
	cur := selfupdate.Version
	if !selfupdate.Valid(cur) {
		return fmt.Errorf("this build has no version (%s): self-update is off; use a release build", cur)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	_ = selfupdate.Cleanup(exe) // leftovers of an earlier update, if no longer locked
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	rel, newer, err := selfupdate.Check(ctx, cur)
	switch {
	case err != nil:
		return err
	case rel == nil:
		_, err = fmt.Fprintf(stdout, "agentlink %s: no release for this platform on github.com/%s\n", cur, selfupdate.Repo)
		return err
	case !newer:
		_, err = fmt.Fprintf(stdout, "agentlink %s is up to date (latest release %s)\n", cur, rel.Version())
		return err
	case check:
		_, err = fmt.Fprintf(stdout, "agentlink %s: update available: %s (run: agentlink update)\n", cur, rel.Version())
		return err
	}
	paths, err := rel.Apply(ctx, exe)
	if err != nil {
		return err
	}
	for _, p := range paths {
		if _, err := fmt.Fprintf(stdout, "updated %s\n", p); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(stdout, "agentlink %s -> %s; restart agentlink-tray if it is running\n", cur, rel.Version())
	return err
}

// parseTimeout accepts whole seconds ("30") or a Go duration ("1m30s").
func parseTimeout(s string) (time.Duration, error) {
	if secs, err := strconv.Atoi(s); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid --timeout %q", s)
	}
	return d, nil
}

func apiURL(cfg config.Config, path string, q url.Values) string {
	u := url.URL{Scheme: "http", Host: cfg.API, Path: path, RawQuery: q.Encode()}
	return u.String()
}

func checkStatus(resp *http.Response, want int) error {
	if resp.StatusCode == want {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("api %s: %s", resp.Status, bytes.TrimSpace(msg))
}

// printLines decodes a JSON array and prints one compact JSON object per line.
func printLines[T any](r io.Reader, w io.Writer) error {
	var items []T
	if err := json.NewDecoder(r).Decode(&items); err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	for _, it := range items {
		if err := enc.Encode(it); err != nil {
			return err
		}
	}
	return nil
}
