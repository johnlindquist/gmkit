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

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

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

// WaitForReady blocks until the libgm client emits *events.ClientReady or
// the context is cancelled. SendMessage and SendReaction need an established
// session before they can round-trip a response; ClientReady is the earliest
// signal that the session is up. The handler is removed before returning.
//
// Subscribe(c.WaitForReady...) is not the right idiom — this method
// installs and removes a single-fire subscriber for you.
func (c *Client) WaitForReady(ctx context.Context) error {
	if c.libgm.IsConnected() {
		// Already ready — but ClientReady has likely already fired and
		// won't repeat. Best effort: return immediately.
		return nil
	}
	ready := make(chan struct{}, 1)
	var fired sync.Once

	c.mu.Lock()
	idx := len(c.subscribers)
	c.subscribers = append(c.subscribers, func(evt any) {
		if _, ok := evt.(*events.ClientReady); ok {
			fired.Do(func() { close(ready) })
		}
	})
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		// Remove only our subscriber; preserve any added concurrently.
		if idx < len(c.subscribers) {
			c.subscribers = append(c.subscribers[:idx], c.subscribers[idx+1:]...)
		}
		c.mu.Unlock()
	}()

	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Underlying returns the wrapped *libgm.Client for callers that need access
// to libgm methods we haven't surfaced yet (ListContacts, FetchMessages,
// etc.). Higher-level operations should prefer the typed wrappers below.
func (c *Client) Underlying() *libgm.Client { return c.libgm }

// SendTextResult describes a successful send.
type SendTextResult struct {
	MessageID      string
	ConversationID string
	TmpID          string
}

// SendText sends a text message into the given conversation. ReplyToID is
// optional; when set, the new message is rendered as a quoted reply by the
// recipient's client. The libgm long-poll must be Connected; call
// WaitForReady first for fresh sessions.
func (c *Client) SendText(conversationID, body, replyToID string) (*SendTextResult, error) {
	if conversationID == "" {
		return nil, fmt.Errorf("conversation id is required")
	}
	if body == "" {
		return nil, fmt.Errorf("message body is required")
	}
	tmpID := uuid.NewString()
	req := &gmproto.SendMessageRequest{
		ConversationID: conversationID,
		TmpID:          tmpID,
		MessagePayload: &gmproto.MessagePayload{
			ConversationID: conversationID,
			TmpID:          tmpID,
			TmpID2:         tmpID,
			MessagePayloadContent: &gmproto.MessagePayloadContent{
				MessageContent: &gmproto.MessageContent{Content: body},
			},
		},
	}
	if replyToID != "" {
		req.Reply = &gmproto.ReplyPayload{MessageID: replyToID}
	}
	resp, err := c.libgm.SendMessage(req)
	if err != nil {
		return nil, fmt.Errorf("libgm send: %w", err)
	}
	if resp.GetStatus() != gmproto.SendMessageResponse_SUCCESS {
		return nil, fmt.Errorf("send rejected by phone: status=%s", resp.GetStatus())
	}
	// libgm doesn't echo the canonical message_id directly in the response;
	// it arrives shortly after via the WrappedMessage event. Return the
	// tmpID so the caller can correlate. Sync-pump-driven upserts will
	// reconcile against the real message_id when it arrives.
	return &SendTextResult{
		MessageID:      tmpID,
		ConversationID: conversationID,
		TmpID:          tmpID,
	}, nil
}

// ReactionAction selects ADD / REMOVE / SWITCH semantics on SendReaction.
type ReactionAction int

const (
	ReactionAdd ReactionAction = iota
	ReactionRemove
	ReactionSwitch
)

// SendReaction adds, removes, or switches a unicode reaction on a message.
func (c *Client) SendReaction(messageID, emoji string, action ReactionAction) error {
	if messageID == "" {
		return fmt.Errorf("message id is required")
	}
	if emoji == "" {
		return fmt.Errorf("emoji is required")
	}
	var act gmproto.SendReactionRequest_Action
	switch action {
	case ReactionAdd:
		act = gmproto.SendReactionRequest_ADD
	case ReactionRemove:
		act = gmproto.SendReactionRequest_REMOVE
	case ReactionSwitch:
		act = gmproto.SendReactionRequest_SWITCH
	default:
		return fmt.Errorf("unknown reaction action %v", action)
	}
	_, err := c.libgm.SendReaction(&gmproto.SendReactionRequest{
		MessageID:    messageID,
		Action:       act,
		ReactionData: &gmproto.ReactionData{Unicode: emoji},
	})
	if err != nil {
		return fmt.Errorf("libgm reaction: %w", err)
	}
	return nil
}

// DownloadMedia retrieves and decrypts the bytes for an attachment.
// The connection does not need to be in long-poll mode — DownloadMedia
// uses authenticated HTTP — but the AuthData's TachyonAuthToken must be
// fresh. Call Connect once before this if the session has been idle.
func (c *Client) DownloadMedia(mediaID string, key []byte) ([]byte, error) {
	if mediaID == "" {
		return nil, fmt.Errorf("media id is required")
	}
	return c.libgm.DownloadMedia(mediaID, key)
}

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
