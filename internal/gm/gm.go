// Package gm wraps go.mau.fi/mautrix-gmessages/pkg/libgm with the conventions
// gmcli needs: filesystem-backed AuthData persistence, an event subscriber
// model on top of libgm's single SetEventHandler, and helpers for the QR
// pairing flow.
//
// Two entry points cover the lifecycle:
//
//	Pair(ctx, layout, render)         // first run: produces session.json
//	Open(layout, logger) -> *Client   // subsequent runs: ready to Connect()
//
// The wrapper does not own a goroutine of its own; libgm runs the long-poll.
// Subscribers must not block in their handlers.
package gm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"

	"github.com/fdsouvenir/gmcli/internal/paths"
)

// PairTimeout is the upper bound on how long we wait for a phone to scan the
// QR code. Google's relay drops unfinished pairings after a few minutes.
const PairTimeout = 5 * time.Minute

// EventHandler is invoked for each event delivered by libgm. The argument
// type is one of the concrete types in pkg/libgm/events or pkg/libgm/gmproto.
type EventHandler func(evt any)

// Client is a thin wrapper around *libgm.Client adding fan-out event
// subscription and persistence on AuthTokenRefreshed.
type Client struct {
	libgm  *libgm.Client
	auth   *libgm.AuthData
	layout paths.Layout
	logger zerolog.Logger

	mu          sync.RWMutex
	subscribers []EventHandler
}

// Open loads session.json and returns a connected-but-not-yet-Connect()'d
// Client. Returns an error if no session exists — the caller must run Pair
// first.
func Open(layout paths.Layout, logger zerolog.Logger) (*Client, error) {
	auth, err := loadAuth(layout.Session)
	if err != nil {
		return nil, err
	}
	if auth.Browser == nil {
		return nil, fmt.Errorf("session %s has no paired device; run `gmcli auth` first", layout.Session)
	}
	c := &Client{
		auth:   auth,
		layout: layout,
		logger: logger,
	}
	c.libgm = libgm.NewClient(auth, nil, logger)
	c.libgm.SetEventHandler(c.dispatch)
	return c, nil
}

// Subscribe registers a handler. Multiple subscribers receive each event in
// the order they were registered. Handlers must not block.
func (c *Client) Subscribe(h EventHandler) {
	c.mu.Lock()
	c.subscribers = append(c.subscribers, h)
	c.mu.Unlock()
}

// Connect opens the long-poll connection. Events flow to subscribers
// immediately. Returns when the initial sync completes; the connection
// continues running in a background goroutine inside libgm.
func (c *Client) Connect() error {
	return c.libgm.Connect()
}

// Disconnect closes the long-poll. Safe to call multiple times.
func (c *Client) Disconnect() {
	c.libgm.Disconnect()
}

// IsConnected reports whether the long-poll is currently active.
func (c *Client) IsConnected() bool {
	return c.libgm.IsConnected()
}

// Underlying returns the wrapped *libgm.Client for callers that need access
// to libgm methods we haven't surfaced yet (ListContacts, FetchMessages,
// SendMessage, etc.). Phase 3 will replace direct usage of this with typed
// store-backed helpers.
func (c *Client) Underlying() *libgm.Client { return c.libgm }

// AuthSnapshot returns a deep copy of the current AuthData by JSON
// round-trip. Useful for diagnostics; do not modify.
func (c *Client) AuthSnapshot() (*libgm.AuthData, error) {
	b, err := json.Marshal(c.auth)
	if err != nil {
		return nil, err
	}
	var out libgm.AuthData
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// dispatch is the single libgm callback. It persists on token refresh and
// then fans out to subscribers.
func (c *Client) dispatch(evt any) {
	switch evt.(type) {
	case *events.AuthTokenRefreshed, *events.PairSuccessful:
		if err := saveAuth(c.layout.Session, c.auth); err != nil {
			c.logger.Error().Err(err).Msg("Failed to persist refreshed auth data")
		}
	}
	c.mu.RLock()
	subs := append([]EventHandler(nil), c.subscribers...)
	c.mu.RUnlock()
	for _, h := range subs {
		h(evt)
	}
}

// PairResult is returned by Pair on success. PhoneID identifies the paired
// device; SessionPath is where the persisted AuthData lives.
type PairResult struct {
	PhoneID     string
	SessionPath string
}

// QRRenderer is invoked once Pair has the QR URL ready. The implementation
// is responsible for displaying it (terminal QR, plain URL, etc.).
type QRRenderer func(qrURL string)

// Pair runs the QR pairing flow. It writes session.json on success and
// returns the paired phone ID. Cancellable via ctx; otherwise bounded by
// PairTimeout. Existing session.json (if any) is overwritten on success.
func Pair(ctx context.Context, layout paths.Layout, logger zerolog.Logger, render QRRenderer) (*PairResult, error) {
	if err := layout.EnsureDirs(); err != nil {
		return nil, err
	}
	auth := libgm.NewAuthData()
	cli := libgm.NewClient(auth, nil, logger)

	done := make(chan *events.PairSuccessful, 1)
	fatal := make(chan error, 1)
	cli.SetEventHandler(func(evt any) {
		switch e := evt.(type) {
		case *events.PairSuccessful:
			select {
			case done <- e:
			default:
			}
		case *events.ListenFatalError:
			select {
			case fatal <- fmt.Errorf("pairing transport failed: %w", e.Error):
			default:
			}
		}
	})

	qr, err := cli.StartLogin()
	if err != nil {
		return nil, fmt.Errorf("start login: %w", err)
	}
	render(qr)

	timeout := time.NewTimer(PairTimeout)
	defer timeout.Stop()

	select {
	case <-ctx.Done():
		cli.Disconnect()
		return nil, ctx.Err()
	case err := <-fatal:
		cli.Disconnect()
		return nil, err
	case <-timeout.C:
		cli.Disconnect()
		return nil, errors.New("pairing timed out — phone never scanned the QR code")
	case res := <-done:
		// libgm reconnects internally 2s after PairSuccessful; we're not
		// going to keep this client around, so close down cleanly.
		cli.Disconnect()
		if err := saveAuth(layout.Session, auth); err != nil {
			return nil, fmt.Errorf("persist session: %w", err)
		}
		return &PairResult{PhoneID: res.PhoneID, SessionPath: layout.Session}, nil
	}
}

func loadAuth(path string) (*libgm.AuthData, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no session at %s; run `gmcli auth` first", path)
		}
		return nil, fmt.Errorf("open session %s: %w", path, err)
	}
	defer f.Close()
	var auth libgm.AuthData
	if err := json.NewDecoder(f).Decode(&auth); err != nil {
		return nil, fmt.Errorf("decode session %s: %w", path, err)
	}
	return &auth, nil
}

func saveAuth(path string, auth *libgm.AuthData) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(auth); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("encode session: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}
