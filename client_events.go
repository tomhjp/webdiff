package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// startTurnEventSubscriber connects to the sandbox's turn-events SSE
// stream and triggers a pull for each event. Runs for the lifetime of
// the client process; reconnects with exponential backoff (1s→30s
// cap) on any error.
//
// We talk directly to the sandbox (not through our own reverse proxy)
// — the desktop's Tailscale identity is what auth.middleware needs to
// see, and routing the stream through the proxy would just add a hop
// for no auth benefit.
func startTurnEventSubscriber(ctx context.Context, upstream *url.URL, engine *syncEngine) {
	go func() {
		backoff := time.Second
		const maxBackoff = 30 * time.Second
		for {
			err := streamTurnEvents(ctx, upstream, engine)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				log.Printf("turn-events stream: %v (retry in %s)", err, backoff)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if err == nil {
				backoff = time.Second
			} else {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
		}
	}()
}

func streamTurnEvents(ctx context.Context, upstream *url.URL, engine *syncEngine) error {
	u := *upstream
	u.Path = "/api/sessions/turn-events"
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	// No client timeout: the connection is intentionally long-lived.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf := make([]byte, 512)
		n, _ := resp.Body.Read(buf)
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(buf[:n]))
	}

	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var ev turnEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			log.Printf("turn-events decode: %v", err)
			continue
		}
		log.Printf("turn-ended %s/%s — pulling", ev.Kind, ev.Name)
		go func(ev turnEvent) {
			if err := engine.pullRepo(ev.Kind, ev.Name); err != nil {
				log.Printf("auto-pull %s/%s: %v", ev.Kind, ev.Name, err)
			}
		}(ev)
	}
}
