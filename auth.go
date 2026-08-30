package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"strings"

	"tailscale.com/client/local"
	"tailscale.com/tailcfg"
)

// ownerAuth gates HTTP requests to the Tailscale device owner. It owns
// the local client (so the startup self-check and per-request whois
// share it) and caches the owner's user ID and the local node's
// Tailscale IPs once init succeeds.
type ownerAuth struct {
	lc      *local.Client
	ownerID tailcfg.UserID
	ips     []netip.Addr
	// dnsName is this node's MagicDNS name, carried here only because
	// the startup status read already has it and main wants it for the
	// SSH hand-off links. Not used for auth decisions.
	dnsName string
}

// newOwnerAuth returns an ownerAuth backed by the default Tailscale
// local client (the zero-value client talks to the system tailscaled
// over its UNIX socket) and reads the local node's status to cache the
// owner's user ID and assigned Tailscale IPs. Failing here is fatal —
// without an owner identity the middleware can't make decisions, and
// without IPs `main` has nothing to bind to. Either case we'd rather
// refuse to start than silently serve everyone or fall back to 0.0.0.0.
func newOwnerAuth(ctx context.Context) (*ownerAuth, error) {
	a := &ownerAuth{lc: &local.Client{}}
	st, err := a.lc.StatusWithoutPeers(ctx)
	if err != nil {
		return nil, fmt.Errorf("tailscale status (is tailscaled running?): %w", err)
	}
	if st.Self == nil {
		return nil, errors.New("tailscale status has no Self node")
	}
	if st.Self.UserID == 0 {
		return nil, errors.New("tailscale node is not logged in")
	}
	if len(st.Self.TailscaleIPs) == 0 {
		return nil, errors.New("tailscale node has no IPs assigned")
	}
	a.ownerID = st.Self.UserID
	a.ips = st.Self.TailscaleIPs
	// DNSName is fully qualified with a trailing dot ("host.tail.ts.net.").
	a.dnsName = strings.TrimSuffix(st.Self.DNSName, ".")
	return a, nil
}

// middleware gates an http.Handler so only the device owner can hit it.
// It whoises the request's RemoteAddr against the local tailscaled and
// compares the resulting UserProfile.ID to the cached owner. Any
// failure (no whois, untagged-but-mismatched, or tagged peer) returns
// 403 with a deliberately vague message — we don't want to leak whether
// the rejection was "not on tailscale" or "wrong user".
func (a *ownerAuth) middleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who, err := a.lc.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil || who == nil || who.UserProfile == nil || who.Node == nil {
			a.deny(w, r, "no Tailscale identity")
			return
		}
		if who.Node.IsTagged() {
			a.deny(w, r, "tagged peer")
			return
		}
		if who.UserProfile.ID != a.ownerID {
			a.deny(w, r, fmt.Sprintf("user %q is not the device owner", who.UserProfile.LoginName))
			return
		}
		h.ServeHTTP(w, r)
	})
}

// deny logs the real reason but tells the caller only that they're not
// allowed.
func (a *ownerAuth) deny(w http.ResponseWriter, r *http.Request, reason string) {
	log.Printf("auth: denied %s %s from %s: %s", r.Method, r.URL.Path, r.RemoteAddr, reason)
	http.Error(w, "Forbidden", http.StatusForbidden)
}
