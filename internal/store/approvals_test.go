package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func openApprovalStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestApprovalLifecycle(t *testing.T) {
	st := openApprovalStore(t)
	ctx := context.Background()

	reply := "msg-9"
	err := st.CreateApproval(ctx, Approval{
		ID:             "ap-1",
		ConversationID: "conv-1",
		Body:           "hello there",
		ReplyToID:      &reply,
		RequestedBy:    "mcp",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := st.GetApproval(ctx, "ap-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != ApprovalPending || got.Body != "hello there" || got.RequestedBy != "mcp" {
		t.Fatalf("unexpected row: %+v", got)
	}
	if got.ReplyToID == nil || *got.ReplyToID != "msg-9" {
		t.Fatalf("reply_to_id not persisted: %+v", got)
	}

	pending, err := st.ListApprovals(ctx, ApprovalPending, 0)
	if err != nil || len(pending) != 1 {
		t.Fatalf("list pending: got %d err=%v, want 1", len(pending), err)
	}

	msgID := "sent-msg-1"
	if err := st.ResolveApproval(ctx, "ap-1", ApprovalSent, nil, &msgID); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got, err = st.GetApproval(ctx, "ap-1")
	if err != nil {
		t.Fatalf("get after resolve: %v", err)
	}
	if got.Status != ApprovalSent || got.MessageID == nil || *got.MessageID != "sent-msg-1" {
		t.Fatalf("unexpected resolved row: %+v", got)
	}

	// Double-resolve must fail with ErrApprovalResolved.
	err = st.ResolveApproval(ctx, "ap-1", ApprovalDenied, nil, nil)
	if !errors.Is(err, ErrApprovalResolved) {
		t.Fatalf("double resolve: got %v, want ErrApprovalResolved", err)
	}

	// Unknown ID must report ErrNotFound.
	err = st.ResolveApproval(ctx, "nope", ApprovalDenied, nil, nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve missing: got %v, want ErrNotFound", err)
	}
}

func TestResolveApprovalRejectsNonTerminalStatus(t *testing.T) {
	st := openApprovalStore(t)
	ctx := context.Background()
	if err := st.CreateApproval(ctx, Approval{ID: "ap-2", ConversationID: "c", Body: "b"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.ResolveApproval(ctx, "ap-2", ApprovalPending, nil, nil); err == nil {
		t.Fatal("expected error resolving to pending")
	}
}
