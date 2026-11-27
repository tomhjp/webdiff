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
	"net/netip"
	"net/url"
	"strings"
	"time"

	"tailscale.com/client/local"
)

// workstationsURL is the JSON listing of corp VMs. The shape matches
// tailscale.io/misc/vmwrangler.VM; we decode only the fields we need.
const workstationsURL = "https://workstations.corp.ts.net/json"

// clientHeader is the request header that the desktop client adds to
// each request that it sends to the sandbox. When it is there, the
// sandbox shows the controls that only the desktop client can do, for
// example "send to github". It is not a security control: the
// endpoints of those controls are only on the client.
const clientHeader = "X-Webdiff-Client"

// runClient is the desktop mode. It is a reverse proxy in front of a
// remote webdiff sandbox, so the browser talks to the desktop, and the
// desktop can add endpoints under /_local/ that use the credentials of
// the desktop, for example to push to GitHub.
//
// By default it listens on port 80 of the Tailscale IPs of the
// desktop. Tailscale IPs are not the wildcard address, so on macOS a
// process with no privileges can use port 80. Add an /etc/hosts entry
// that maps the name in -hostname to the Tailscale IPv4 address to
// open http://webdiff/.
func runClient(args []string) error {
	fs := flag.NewFlagSet("client", flag.ContinueOnError)
	listenFlag := fs.String("listen", "", "comma-separated addresses to listen on; defaults to port 80 of each Tailscale IP of this device")
	hostnameFlag := fs.String("hostname", "webdiff", "host name that the browser uses for the client, from an /etc/hosts entry")
	githubURLFlag := fs.String("github-url", "git@github.com:{repo}.git", "URL to push to; {repo} is replaced with owner/name of the GitHub repo")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: webdiff client [flags] [<sandbox-host[:port]>]")
		fmt.Fprintln(fs.Output(), "  with no host, finds the LLM sandbox VM of the user")
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

	selfIPs, err := tailscaleIPs(context.Background())
	if err != nil && *listenFlag == "" {
		return fmt.Errorf("find the Tailscale IPs of this device (or use -listen): %w", err)
	}
	var addrs []string
	if *listenFlag != "" {
		for _, a := range strings.Split(*listenFlag, ",") {
			if a = strings.TrimSpace(a); a != "" {
				addrs = append(addrs, a)
			}
		}
	} else {
		for _, ip := range selfIPs {
			addrs = append(addrs, net.JoinHostPort(ip.String(), "80"))
		}
	}
	if len(addrs) == 0 {
		return errors.New("no addresses to listen on")
	}

	sender := &githubSender{
		upstream:  upstream,
		urlFormat: *githubURLFlag,
		cacheDir:  defaultSendCacheDir(),
	}

	proxy := &httputil.ReverseProxy{
		// SSE and chunked responses flush on their own, but -1 also
		// covers an endpoint that streams with another Content-Type.
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			// Keep the Host of the browser, so that it agrees with
			// the Origin header for the CSRF checks of the sandbox.
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Set(clientHeader, "1")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("proxy %s %s: %v", r.Method, r.URL.Path, err)
			http.Error(w, "upstream unreachable: "+err.Error(), http.StatusBadGateway)
		},
	}

	mux := http.NewServeMux()
	// The sandbox uses /, /_/, and /api/, so /_local/ cannot be the
	// same as a path of the sandbox.
	mux.HandleFunc("/_local/github/send", sender.handleSend)
	mux.Handle("/", proxy)

	g := &clientGuard{
		hosts:   allowedHosts(*hostnameFlag, addrs),
		selfIPs: selfIPs,
		next:    mux,
	}
	srv := &http.Server{
		Handler:           g,
		ReadHeaderTimeout: 10 * time.Second,
		// No ReadTimeout or WriteTimeout: they would stop SSE and
		// WebSocket connections.
	}
	errCh := make(chan error, len(addrs))
	for _, addr := range addrs {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", addr, err)
		}
		go func() { errCh <- srv.Serve(ln) }()
	}
	fmt.Printf("webdiff client on %s → %s\n", strings.Join(addrs, ", "), upstream)
	fmt.Printf("Open http://%s/ in your browser\n", *hostnameFlag)
	return <-errCh
}

// tailscaleIPs returns the Tailscale IPs of this device, from the
// local tailscaled.
func tailscaleIPs(ctx context.Context) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var lc local.Client
	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		return nil, err
	}
	if st.Self == nil || len(st.Self.TailscaleIPs) == 0 {
		return nil, errors.New("tailscale status has no IPs")
	}
	return st.Self.TailscaleIPs, nil
}

// allowedHosts returns the host names that the client accepts in the
// Host header: hostname and the hosts of addrs.
func allowedHosts(hostname string, addrs []string) map[string]bool {
	hosts := map[string]bool{
		strings.ToLower(hostname): true,
	}
	for _, a := range addrs {
		if h, _, err := net.SplitHostPort(a); err == nil && h != "" {
			hosts[strings.ToLower(h)] = true
		}
	}
	return hosts
}

// clientGuard accepts only requests that come from this device. The
// client listens on the Tailscale IPs, so other devices of the tailnet
// can connect to it, but the client acts with the credentials of this
// device and the identity of its user.
//
// It also accepts only the Host names of the client, to stop DNS
// rebinding, and for /_local/ it accepts only same-origin JSON POST
// requests, to stop cross-site requests from other pages in the
// browser.
type clientGuard struct {
	hosts   map[string]bool
	selfIPs []netip.Addr
	next    http.Handler
}

func (g *clientGuard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !g.fromSelf(r.RemoteAddr) {
		log.Printf("client: denied %s %s from %s: not this device", r.Method, r.URL.Path, r.RemoteAddr)
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if !g.hosts[host] {
		http.Error(w, "unknown host "+r.Host, http.StatusMisdirectedRequest)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/_local/") {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed", nil)
			return
		}
		if r.Header.Get("Origin") != "http://"+r.Host {
			writeJSONError(w, http.StatusForbidden, "cross-origin request", nil)
			return
		}
		if !isJSONRequest(r) {
			writeJSONError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json", nil)
			return
		}
	}
	g.next.ServeHTTP(w, r)
}

// fromSelf reports whether remoteAddr is an address of this device: a
// loopback address or one of its Tailscale IPs.
func (g *clientGuard) fromSelf(remoteAddr string) bool {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := ap.Addr().Unmap()
	if ip.IsLoopback() {
		return true
	}
	for _, self := range g.selfIPs {
		if ip == self {
			return true
		}
	}
	return false
}

// parseSandboxURL accepts host, host:port, [ipv6]:port, or http://...
// and returns a URL of the sandbox. With no port, it uses port 80.
// It does not accept https://, because the sandbox serves plain HTTP
// on Tailscale.
func parseSandboxURL(s string) (*url.URL, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("empty endpoint")
	}
	if strings.HasPrefix(s, "https://") {
		return nil, errors.New("https not supported (sandbox is plain HTTP on Tailscale)")
	}
	if !strings.HasPrefix(s, "http://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	if u.Hostname() == "" {
		return nil, errors.New("missing host")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
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
