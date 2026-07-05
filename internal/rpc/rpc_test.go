package rpc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/fdsouvenir/gmcli/internal/store"
)

// shortSocketPath returns a socket path short enough for the 104-byte
// sun_path limit on macOS — t.TempDir() embeds the test name and can blow
// past it.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gmrpc")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

// startTestServer runs a Server with store-only deps (no phone) on a real
// unix socket and returns a connected client.
func startTestServer(t *testing.T, mode SendMode) (*Client, *store.Store, *Server) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv := NewServer(Deps{
		Store:    st,
		Logger:   zerolog.Nop(),
		Version:  "test",
		SendMode: mode,
	})
	sock := shortSocketPath(t)
	ln, err := Listen(sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	client, err := Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, st, srv
}

func callCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestPingAndStatus(t *testing.T) {
	client, _, _ := startTestServer(t, SendOff)

	var ping struct {
		Pong          bool   `json:"pong"`
		Version       string `json:"version"`
		SchemaVersion int    `json:"schema_version"`
	}
	if err := client.Call(callCtx(t), "ping", nil, &ping); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if !ping.Pong || ping.Version != "test" || ping.SchemaVersion < 3 {
		t.Fatalf("unexpected ping result: %+v", ping)
	}

	var status struct {
		Connected bool   `json:"connected"`
		SendMode  string `json:"send_mode"`
	}
	if err := client.Call(callCtx(t), "status", nil, &status); err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Connected || status.SendMode != "off" {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestUnknownMethodAndBadParams(t *testing.T) {
	client, _, _ := startTestServer(t, SendOff)

	err := client.Call(callCtx(t), "no.such.method", nil, nil)
	var rpcErr *Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeMethodNotFound {
		t.Fatalf("unknown method: got %v, want CodeMethodNotFound", err)
	}

	err = client.Call(callCtx(t), "messages.search", map[string]any{"limit": 5}, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("missing query: got %v, want CodeInvalidParams", err)
	}
}

func TestQueryMethodsAgainstSeededStore(t *testing.T) {
	client, st, _ := startTestServer(t, SendOff)
	ctx := context.Background()

	if err := st.UpsertConversation(ctx, store.Conversation{ID: "conv-1", Name: "Alice", LastMessageTimeMS: 1000}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	body := "pizza tonight?"
	if err := st.UpsertMessage(ctx, store.Message{ID: "m-1", ConversationID: "conv-1", Body: &body, TimestampMS: 1000}); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	if err := st.UpsertContact(ctx, store.Contact{ParticipantID: "p-1", Name: "Alice", E164: "+15551234567"}); err != nil {
		t.Fatalf("seed contact: %v", err)
	}

	var convs []store.Conversation
	if err := client.Call(callCtx(t), "chats.list", map[string]any{"limit": 10}, &convs); err != nil {
		t.Fatalf("chats.list: %v", err)
	}
	if len(convs) != 1 || convs[0].ID != "conv-1" {
		t.Fatalf("chats.list: %+v", convs)
	}

	var show struct {
		Conversation store.Conversation `json:"conversation"`
		Messages     []store.Message    `json:"messages"`
	}
	if err := client.Call(callCtx(t), "chats.show", map[string]any{"conversation_id": "conv-1"}, &show); err != nil {
		t.Fatalf("chats.show: %v", err)
	}
	if show.Conversation.Name != "Alice" || len(show.Messages) != 1 {
		t.Fatalf("chats.show: %+v", show)
	}

	var hits []store.SearchHit
	if err := client.Call(callCtx(t), "messages.search", map[string]any{"query": "pizza"}, &hits); err != nil {
		t.Fatalf("messages.search: %v", err)
	}
	if len(hits) != 1 || hits[0].MessageID != "m-1" {
		t.Fatalf("messages.search: %+v", hits)
	}

	var contacts []store.Contact
	if err := client.Call(callCtx(t), "contacts.search", map[string]any{"query": "alice"}, &contacts); err != nil {
		t.Fatalf("contacts.search: %v", err)
	}
	if len(contacts) != 1 || contacts[0].ParticipantID != "p-1" {
		t.Fatalf("contacts.search: %+v", contacts)
	}

	// Not-found paths.
	var rpcErr *Error
	err := client.Call(callCtx(t), "chats.show", map[string]any{"conversation_id": "missing"}, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeNotFound {
		t.Fatalf("chats.show missing: got %v, want CodeNotFound", err)
	}
}

func TestSendTextBlockedWhenOff(t *testing.T) {
	client, _, _ := startTestServer(t, SendOff)
	err := client.Call(callCtx(t), "send.text", map[string]any{"conversation_id": "c", "body": "hi"}, nil)
	var rpcErr *Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeSendsDisabled {
		t.Fatalf("send.text in off mode: got %v, want CodeSendsDisabled", err)
	}
}

func TestApprovalQueueFlow(t *testing.T) {
	client, _, _ := startTestServer(t, SendApprove)

	// Subscribe on a second connection so we can assert events fire.
	if err := client.Subscribe(callCtx(t)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	var approval store.Approval
	if err := client.Call(callCtx(t), "send.text", map[string]any{
		"conversation_id": "conv-1",
		"body":            "agent says hi",
		"requested_by":    "test-agent",
	}, &approval); err != nil {
		t.Fatalf("send.text: %v", err)
	}
	if approval.Status != store.ApprovalPending || approval.ID == "" {
		t.Fatalf("expected pending approval, got %+v", approval)
	}

	waitEvent := func(wantType string) Event {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case ev, ok := <-client.Events():
				if !ok {
					t.Fatal("event channel closed")
				}
				if ev.Type == wantType {
					return ev
				}
			case <-deadline:
				t.Fatalf("timed out waiting for %s", wantType)
			}
		}
	}
	waitEvent(EventApprovalRequested)

	var listed []store.Approval
	if err := client.Call(callCtx(t), "approvals.list", map[string]any{"status": "pending"}, &listed); err != nil {
		t.Fatalf("approvals.list: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != approval.ID {
		t.Fatalf("approvals.list: %+v", listed)
	}

	// Approving without a phone connection must fail with unavailable and
	// leave the approval pending (so it can be retried when the phone is up).
	var rpcErr *Error
	err := client.Call(callCtx(t), "approvals.approve", map[string]any{"approval_id": approval.ID}, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeUnavailable {
		t.Fatalf("approve without phone: got %v, want CodeUnavailable", err)
	}
	var after store.Approval
	if err := client.Call(callCtx(t), "approvals.list", map[string]any{"status": "pending"}, &listed); err != nil || len(listed) != 1 {
		t.Fatalf("approval should still be pending: %+v err=%v", listed, err)
	}

	// Deny resolves it and emits approval.resolved.
	if err := client.Call(callCtx(t), "approvals.deny", map[string]any{"approval_id": approval.ID, "reason": "not now"}, &after); err != nil {
		t.Fatalf("approvals.deny: %v", err)
	}
	if after.Status != store.ApprovalDenied || after.Error == nil || *after.Error != "not now" {
		t.Fatalf("deny result: %+v", after)
	}
	waitEvent(EventApprovalResolved)

	// Second deny reports already-resolved.
	err = client.Call(callCtx(t), "approvals.deny", map[string]any{"approval_id": approval.ID}, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeAlreadyResolved {
		t.Fatalf("double deny: got %v, want CodeAlreadyResolved", err)
	}
}

func TestBroadcastReachesOnlySubscribers(t *testing.T) {
	client, _, srv := startTestServer(t, SendOff)

	// Not subscribed yet: a broadcast must not arrive.
	srv.Broadcast(EventSyncStatus, map[string]any{"state": "ready"})
	select {
	case ev := <-client.Events():
		t.Fatalf("unsubscribed client received %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}

	if err := client.Subscribe(callCtx(t)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	srv.Broadcast(EventSyncStatus, map[string]any{"state": "ready"})
	select {
	case ev := <-client.Events():
		if ev.Type != EventSyncStatus {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subscribed client never received the event")
	}
}

func TestListenRefusesLiveDaemonAndClearsStaleSocket(t *testing.T) {
	sock := shortSocketPath(t)

	ln, err := Listen(sock)
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}
	// Something is accepting: a second daemon must refuse to start.
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()
	if _, err := Listen(sock); err == nil {
		t.Fatal("second listen should fail while the first is alive")
	}
	_ = ln.Close()

	// Simulate a crash leaving a stale socket file behind: a path that
	// exists but where nothing accepts connections.
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatalf("plant stale socket: %v", err)
	}
	ln2, err := Listen(sock)
	if err != nil {
		t.Fatalf("listen after stale socket: %v", err)
	}
	_ = ln2.Close()
}

func TestIdleExitFiresAfterLastClientLeaves(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv := NewServer(Deps{
		Store:    st,
		Logger:   zerolog.Nop(),
		SendMode: SendOff,
		IdleExit: 200 * time.Millisecond,
	})
	sock := shortSocketPath(t)
	ln, err := Listen(sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx, ln) }()

	// A connected client holds off the idle exit past the initial window.
	client, err := Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := client.Call(callCtx(t), "ping", nil, nil); err != nil {
		t.Fatalf("ping: %v", err)
	}
	select {
	case <-srv.Idle():
		t.Fatal("idle fired while a client was connected")
	case <-time.After(400 * time.Millisecond):
	}

	// Disconnect: idle must fire within the window (plus slack).
	_ = client.Close()
	select {
	case <-srv.Idle():
	case <-time.After(2 * time.Second):
		t.Fatal("idle never fired after last client left")
	}
}

func TestIdleDisabledNeverFires(t *testing.T) {
	client, _, srv := startTestServer(t, SendOff)
	_ = client
	select {
	case <-srv.Idle():
		t.Fatal("idle fired with IdleExit disabled")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestChatsFindAndRichSearch(t *testing.T) {
	client, st, _ := startTestServer(t, SendOff)
	ctx := context.Background()

	if err := st.UpsertConversation(ctx, store.Conversation{
		ID:                "conv-9",
		ParticipantsJSON:  `[{"id":"p9","name":"Zelda","e164":"+15550009","is_me":false}]`,
		LastMessageTimeMS: 5,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.UpsertContact(ctx, store.Contact{ParticipantID: "p9", Name: "Zelda", E164: "+15550009"}); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	body := "let's meet at the library?"
	if err := st.UpsertMessage(ctx, store.Message{ID: "m9", ConversationID: "conv-9", SenderID: "p9", Body: &body, TimestampMS: 5}); err != nil {
		t.Fatalf("seed msg: %v", err)
	}

	var convs []store.Conversation
	if err := client.Call(callCtx(t), "chats.find", map[string]any{"query": "zelda"}, &convs); err != nil {
		t.Fatalf("chats.find: %v", err)
	}
	if len(convs) != 1 || convs[0].ID != "conv-9" {
		t.Fatalf("chats.find: %+v", convs)
	}

	// Natural-language query with punctuation goes through the fallback and
	// comes back enriched.
	var hits []store.RichHit
	if err := client.Call(callCtx(t), "messages.search", map[string]any{"query": "library?"}, &hits); err != nil {
		t.Fatalf("messages.search: %v", err)
	}
	if len(hits) != 1 || hits[0].SenderName != "Zelda" || hits[0].ConversationName != "Zelda" {
		t.Fatalf("rich hits: %+v", hits)
	}
}
