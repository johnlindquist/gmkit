package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/johnlindquist/gmkit/internal/gm"
	"github.com/johnlindquist/gmkit/internal/store"
	gmsync "github.com/johnlindquist/gmkit/internal/sync"
)

// maxLineBytes bounds a single request line. Message bodies are small; 4 MiB
// is generous headroom without letting a broken client OOM the daemon.
const maxLineBytes = 4 << 20

// defaultLiveTimeout bounds one phone-touching operation (send, backfill).
const defaultLiveTimeout = 60 * time.Second

// Deps carries everything the server needs. Client and Pump may be nil in
// tests that only exercise store-backed methods; phone-touching methods then
// return CodeUnavailable.
type Deps struct {
	Store       *store.Store
	Client      *gm.Client
	Pump        *gmsync.Pump
	Logger      zerolog.Logger
	Version     string
	SendMode    SendMode
	LiveTimeout time.Duration // 0 means defaultLiveTimeout
	// IdleExit, when > 0, arms the Idle() signal: it fires once the server
	// has had no client connections for this long (including never having
	// had one). Used by auto-started daemons to retire themselves.
	IdleExit time.Duration
}

// Server hosts the RPC surface. Construct with NewServer, run with Serve.
type Server struct {
	deps Deps

	mu        sync.Mutex
	conns     map[*serverConn]struct{}
	idleTimer *time.Timer
	idleFired bool

	idle chan struct{}

	// liveMu serializes operations that talk to the phone (sends,
	// backfills). libgm tolerates some concurrency, but interleaving
	// FetchMessages pagination with sends has no upside.
	liveMu sync.Mutex
}

// NewServer builds a Server around deps.
func NewServer(deps Deps) *Server {
	if deps.SendMode == "" {
		deps.SendMode = SendOff
	}
	if deps.LiveTimeout == 0 {
		deps.LiveTimeout = defaultLiveTimeout
	}
	s := &Server{deps: deps, conns: make(map[*serverConn]struct{})}
	if deps.IdleExit > 0 {
		s.idle = make(chan struct{})
		s.armIdleTimer()
	}
	return s
}

// Idle fires once when the server has been client-free for Deps.IdleExit.
// Returns nil (blocks forever in a select) when idle exit is disabled.
func (s *Server) Idle() <-chan struct{} { return s.idle }

// armIdleTimer (re)starts the countdown. Callers hold no lock; the timer
// callback re-checks emptiness under the lock, so a connection arriving
// between fire and check wins.
func (s *Server) armIdleTimer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
	}
	s.idleTimer = time.AfterFunc(s.deps.IdleExit, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.conns) == 0 && !s.idleFired {
			s.idleFired = true
			close(s.idle)
		}
	})
}

// Listen prepares the unix socket at path. A stale socket file from a
// crashed daemon is removed; a live one (something accepts connections)
// aborts with an error so two daemons never fight over one store.
func Listen(path string) (net.Listener, error) {
	if _, err := os.Stat(path); err == nil {
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("another gmcli daemon is already listening on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	// The archive is private; so is its control socket.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod socket %s: %w", path, err)
	}
	return ln, nil
}

// Serve accepts connections until ctx is cancelled or the listener fails.
// It closes the listener (and removes the socket file) on return.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	defer func() {
		s.mu.Lock()
		for c := range s.conns {
			_ = c.netConn.Close()
		}
		s.mu.Unlock()
	}()
	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		c := &serverConn{s: s, netConn: nc}
		s.mu.Lock()
		s.conns[c] = struct{}{}
		if s.idleTimer != nil {
			s.idleTimer.Stop()
		}
		s.mu.Unlock()
		go c.run(ctx)
	}
}

// HandleGMEvent converts libgm events into subscriber pushes. Register it
// on gm.Client *after* the sync pump so the store row already exists when a
// client reacts to the event.
func (s *Server) HandleGMEvent(evt any) {
	switch e := evt.(type) {
	case *libgm.WrappedMessage:
		if row, ok := gmsync.MessageRow(e, "gm"); ok {
			s.Broadcast(EventMessageNew, map[string]any{"message": row, "is_old": e.IsOld})
		}
	case *gmproto.Conversation:
		if row, ok := gmsync.ConversationRow(e, "gm"); ok {
			s.Broadcast(EventConversationUpdated, map[string]any{"conversation": row})
		}
	case *events.ClientReady:
		s.Broadcast(EventSyncStatus, map[string]any{"state": "ready"})
	case *events.PhoneNotResponding:
		s.Broadcast(EventSyncStatus, map[string]any{"state": "phone_not_responding"})
	case *events.PhoneRespondingAgain:
		s.Broadcast(EventSyncStatus, map[string]any{"state": "phone_responding"})
	case *events.ListenTemporaryError:
		s.Broadcast(EventSyncStatus, map[string]any{"state": "listen_temporary_error"})
	case *events.ListenRecovered:
		s.Broadcast(EventSyncStatus, map[string]any{"state": "listen_recovered"})
	case *events.GaiaLoggedOut:
		s.Broadcast(EventSyncStatus, map[string]any{"state": "logged_out"})
	}
}

// Broadcast pushes an event to every subscribed connection.
func (s *Server) Broadcast(typ string, data any) {
	n := Notification{JSONRPC: "2.0", Method: "event", Params: Event{Type: typ, Data: data}}
	s.mu.Lock()
	conns := make([]*serverConn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		if c.subscribed.Load() {
			c.write(n)
		}
	}
}

func (s *Server) dropConn(c *serverConn) {
	s.mu.Lock()
	delete(s.conns, c)
	empty := len(s.conns) == 0
	s.mu.Unlock()
	_ = c.netConn.Close()
	if empty && s.idle != nil {
		s.armIdleTimer()
	}
}

// serverConn is one accepted client connection.
type serverConn struct {
	s          *Server
	netConn    net.Conn
	writeMu    sync.Mutex
	subscribed atomic.Bool
}

func (c *serverConn) run(ctx context.Context) {
	defer c.s.dropConn(c)
	scanner := bufio.NewScanner(c.netConn)
	scanner.Buffer(make([]byte, 64<<10), maxLineBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			c.write(Response{JSONRPC: "2.0", Error: &Error{Code: CodeParse, Message: "parse error: " + err.Error()}})
			continue
		}
		if req.Method == "" {
			c.write(Response{JSONRPC: "2.0", ID: req.ID, Error: &Error{Code: CodeInvalidRequest, Message: "missing method"}})
			continue
		}
		// Each request runs in its own goroutine so a slow phone operation
		// doesn't starve queries multiplexed over the same connection.
		go func(req Request) {
			result, rpcErr := c.s.dispatch(ctx, c, req.Method, req.Params)
			if req.ID == nil {
				return // notification: no reply expected
			}
			resp := Response{JSONRPC: "2.0", ID: req.ID}
			if rpcErr != nil {
				resp.Error = rpcErr
			} else {
				resp.Result = result
			}
			c.write(resp)
		}(req)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, net.ErrClosed) {
		c.s.deps.Logger.Debug().Err(err).Msg("rpc connection read error")
	}
}

// write serializes one message onto the connection. Errors drop the
// connection; the read side will notice on its next Scan.
func (c *serverConn) write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		c.s.deps.Logger.Error().Err(err).Msg("rpc marshal failed")
		return
	}
	b = append(b, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.netConn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.netConn.Write(b); err != nil {
		_ = c.netConn.Close()
	}
}
