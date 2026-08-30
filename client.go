package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// workstationsURL is the JSON listing of corp VMs. The shape matches
// tailscale.io/misc/vmwrangler.VM; we decode only the fields we need.
const workstationsURL = "https://workstations.corp.ts.net/json"

// runClient is the desktop-side mode. It binds a localhost reverse proxy in
// front of a remote webdiff sandbox so the browser sits same-origin with the
// desktop, leaving room for /_local/* trusted-action endpoints in follow-ups
// without touching the sandbox.
func runClient(args []string) error {
	fs := flag.NewFlagSet("client", flag.ContinueOnError)
	listenPort := fs.String("port", "9418", "local port to listen on")
	open := fs.Bool("open", false, "open the URL in the default browser on startup")
	rootFlag := fs.String("root", "", "local directory containing the same repos as the sandbox root; defaults to cwd")
	worktreesFlag := fs.String("worktrees-root", "", "local directory mirroring the sandbox's managed worktrees; defaults to <root>/wt")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: webdiff client [-port N] [-open] [-root DIR] [-worktrees-root DIR] [<sandbox-host[:port]>]")
		fmt.Fprintln(fs.Output(), "  with no host, auto-discovers the user's LLM sandbox VM")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	var endpoint string
	switch len(rest) {
	case 0:
		fmt.Println("looking up LLM sandbox VM...")
		name, err := discoverSandbox(context.Background())
		if err != nil {
			return fmt.Errorf("auto-discover sandbox: %w (try: webdiff client <host>)", err)
		}
		fmt.Printf("discovered sandbox: %s\n", name)
		endpoint = name
	case 1:
		endpoint = rest[0]
	default:
		fs.Usage()
		return errors.New("at most one sandbox endpoint argument allowed")
	}
	upstream, err := parseSandboxURL(endpoint)
	if err != nil {
		return fmt.Errorf("parse sandbox endpoint: %w", err)
	}

	localRoot, err := resolveLocalRoot(*rootFlag)
	if err != nil {
		return err
	}
	localWorktreesRoot, err := resolveLocalWorktreesRoot(*worktreesFlag, localRoot)
	if err != nil {
		return err
	}
	engine := newSyncEngine(upstream, localRoot, localWorktreesRoot)
	fmt.Printf("local root: %s\n", localRoot)
	fmt.Printf("local worktrees root: %s\n", localWorktreesRoot)
	startTurnEventSubscriber(context.Background(), upstream, engine)
	startLocalWatcher(context.Background(), engine)

	proxy := &httputil.ReverseProxy{
		// Defensive; SSE/chunked already auto-flush via Content-Type
		// detection, but -1 covers any future endpoint that streams
		// without setting a streaming Content-Type.
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			// Preserve inbound Host so the sandbox sees
			// Host: 127.0.0.1:PORT, matching the browser's Origin
			// header. Today every POST goes to apiMux (no CSRF),
			// so this is future-proofing for a browser-mux POST
			// landing on the sandbox later.
			pr.Out.Host = pr.In.Host
			// Marks the request as coming through the desktop client
			// so the sandbox renders /_local/sync-dependent UI (the
			// push/pull buttons). Not a trust boundary — the sync
			// endpoints only exist here on the client, never on the
			// sandbox — just keeps dead buttons out of direct access.
			pr.Out.Header.Set("X-Webdiff-Client", "1")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("proxy %s %s: %v", r.Method, r.URL.Path, err)
			http.Error(w, "upstream unreachable: "+err.Error(), http.StatusBadGateway)
		},
	}

	mux := http.NewServeMux()
	// Desktop-only endpoints live under /_local/. The sandbox uses
	// /, /_/, and /api/, so /_local/ is collision-free with the
	// reverse-proxy passthrough below.
	mux.HandleFunc("/_local/sync/push", engine.handleSyncPush)
	mux.HandleFunc("/_local/sync/pull", engine.handleSyncPull)
	mux.Handle("/", proxy)

	addr := "127.0.0.1:" + *listenPort
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}

	browserURL := "http://" + addr + "/"
	fmt.Printf("webdiff client → %s\n", upstream)
	fmt.Printf("Open %s in your browser\n", browserURL)
	if *open {
		if err := openBrowser(browserURL); err != nil {
			log.Printf("open browser: %v", err)
		}
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No ReadTimeout / WriteTimeout: would cap SSE/WS lifetime.
	}
	return srv.Serve(ln)
}

// parseSandboxURL accepts host, host:port, [ipv6]:port, or http://... and
// returns an *url.URL pointing at the sandbox. Defaults the port to 9418.
// Rejects https:// (sandbox runs plain HTTP on Tailscale).
func parseSandboxURL(s string) (*url.URL, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("empty endpoint")
	}
	if strings.HasPrefix(s, "https://") {
		return nil, errors.New("https not supported (sandbox is plain HTTP on Tailscale)")
	}
	if strings.HasPrefix(s, "http://") {
		u, err := url.Parse(s)
		if err != nil {
			return nil, err
		}
		if u.Host == "" {
			return nil, errors.New("missing host")
		}
		if u.Port() == "" {
			u.Host = net.JoinHostPort(u.Hostname(), "9418")
		}
		u.Path = strings.TrimRight(u.Path, "/")
		return u, nil
	}

	host, port := s, "9418"
	if h, p, err := net.SplitHostPort(s); err == nil {
		host, port = h, p
	}
	return &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(host, port),
	}, nil
}

// discoverSandbox queries the corp workstations service for the
// caller's LLM sandbox VM, mirroring the lookup that misc/aif uses.
// Returns the VM's name (a Tailscale MagicDNS-resolvable hostname).
func discoverSandbox(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", workstationsURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching %s: %w", workstationsURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching %s: %s", workstationsURL, resp.Status)
	}
	// Minimal subset of vmwrangler.VM — we only read these fields.
	var vms []struct {
		Name       string
		State      string
		ClientTags map[string]string
	}
	if err := json.NewDecoder(resp.Body).Decode(&vms); err != nil {
		return "", fmt.Errorf("decoding response: %w", err)
	}
	var matches []string
	for _, vm := range vms {
		if vm.ClientTags["purpose"] == "llm-sandbox" && vm.State == "running" {
			matches = append(matches, vm.Name)
		}
	}
	switch len(matches) {
	case 0:
		return "", errors.New("no running LLM sandbox VM found")
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("multiple running LLM sandbox VMs found: %s", strings.Join(matches, ", "))
	}
}

func resolveLocalRoot(flagVal string) (string, error) {
	if flagVal == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("getwd: %w", err)
		}
		return wd, nil
	}
	abs, err := filepath.Abs(flagVal)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return "", fmt.Errorf("local root %s is not a directory", abs)
	}
	return abs, nil
}

// resolveLocalWorktreesRoot defaults to `<localRoot>/wt`, mirroring the
// sandbox layout so a worktree's local path reads the same as its remote
// one.
func resolveLocalWorktreesRoot(flagVal, localRoot string) (string, error) {
	dir := filepath.Join(localRoot, "wt")
	if flagVal != "" {
		abs, err := filepath.Abs(flagVal)
		if err != nil {
			return "", err
		}
		dir = abs
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return dir, nil
}

func openBrowser(rawURL string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", rawURL)
	case "linux":
		cmd = exec.Command("xdg-open", rawURL)
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
	return cmd.Start()
}
