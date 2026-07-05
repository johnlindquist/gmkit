package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Client is a minimal Go client for the gmcli daemon socket. Safe for
// concurrent Call use; events arrive on the channel returned by Events after
// Subscribe.
type Client struct {
	conn    net.Conn
	writeMu sync.Mutex

	mu      sync.Mutex
	pending map[string]chan Response
	closed  bool

	nextID atomic.Int64
	events chan Event
}

// Dial connects to the daemon socket. Returns a wrapped error mentioning
// `gmcli serve` when nothing is listening, since that is by far the most
// common failure.
func Dial(socketPath string) (*Client, error) {
	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to gmcli daemon at %s (is `gmcli serve` running?): %w", socketPath, err)
	}
	c := &Client{
		conn:    conn,
		pending: make(map[string]chan Response),
		events:  make(chan Event, 256),
	}
	go c.readLoop()
	return c, nil
}

// Close tears down the connection. Pending calls fail once the read loop
// notices the closed socket; the events channel closes with it.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return c.conn.Close()
}

// Events is the stream of server pushes. Call Subscribe first; events are
// dropped (oldest-first pressure on the server side, newest dropped here)
// if the consumer falls more than the buffer behind.
func (c *Client) Events() <-chan Event { return c.events }

// Subscribe asks the server to start pushing events on this connection.
func (c *Client) Subscribe(ctx context.Context) error {
	var res struct {
		Subscribed bool `json:"subscribed"`
	}
	return c.Call(ctx, "subscribe", nil, &res)
}

// Call performs one RPC round trip, decoding the result into result when
// non-nil. Server-side failures come back as *Error.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	id := c.nextID.Add(1)
	idJSON := json.RawMessage(strconv.FormatInt(id, 10))

	req := Request{JSONRPC: "2.0", ID: idJSON, Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("marshal params: %w", err)
		}
		req.Params = b
	}
	b, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	b = append(b, '\n')

	ch := make(chan Response, 1)
	key := string(idJSON)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("client is closed")
	}
	c.pending[key] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
	}()

	c.writeMu.Lock()
	_, err = c.conn.Write(b)
	c.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("write request: %w", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			return fmt.Errorf("connection closed while waiting for %s", method)
		}
		if resp.Error != nil {
			return resp.Error
		}
		if result == nil {
			return nil
		}
		raw, err := json.Marshal(resp.Result)
		if err != nil {
			return fmt.Errorf("re-marshal result: %w", err)
		}
		return json.Unmarshal(raw, result)
	}
}

func (c *Client) readLoop() {
	scanner := bufio.NewScanner(c.conn)
	scanner.Buffer(make([]byte, 64<<10), maxLineBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Distinguish responses (have "id") from event notifications.
		var probe struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			continue
		}
		if probe.Method == "event" {
			var n Notification
			if err := json.Unmarshal(line, &n); err == nil {
				select {
				case c.events <- n.Params:
				default: // consumer is behind; drop rather than stall reads
				}
			}
			continue
		}
		if len(probe.ID) == 0 {
			continue
		}
		var resp Response
		if err := json.Unmarshal(line, &resp); err != nil {
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[string(probe.ID)]
		c.mu.Unlock()
		if ok {
			ch <- resp
		}
	}
	// Reader is done (EOF or error): fail all pending calls and end the
	// event stream.
	c.mu.Lock()
	c.closed = true
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	close(c.events)
}
