package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fdsouvenir/gmcli/internal/store"
)

// seedThread inserts a conversation and `n` messages 1ms apart, alternating
// sender between "alice" and "me", returning their IDs in chronological order.
func seedThread(t *testing.T, st *store.Store, convID string, n int) []string {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertConversation(ctx, store.Conversation{
		ID:                convID,
		Name:              "Alice",
		LastMessageTimeMS: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed conv: %v", err)
	}
	ids := make([]string, n)
	base := time.Now().UnixMilli()
	for i := 0; i < n; i++ {
		body := "msg-" + itoa(i)
		fromMe := i%2 == 1
		sender := "alice"
		if fromMe {
			sender = "me"
		}
		id := convID + "-m" + itoa(i)
		if err := st.UpsertMessage(ctx, store.Message{
			ID:             id,
			ConversationID: convID,
			SenderID:       sender,
			Body:           &body,
			TimestampMS:    base + int64(i),
			IsFromMe:       fromMe,
		}); err != nil {
			t.Fatalf("seed msg %d: %v", i, err)
		}
		ids[i] = id
	}
	return ids
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

func TestListConversationsOrdersByLastMessage(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	mustUC := func(c store.Conversation) {
		t.Helper()
		if err := st.UpsertConversation(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	mustUC(store.Conversation{ID: "old", Name: "Old", LastMessageTimeMS: 100})
	mustUC(store.Conversation{ID: "new", Name: "New", LastMessageTimeMS: 200})
	mustUC(store.Conversation{ID: "mid", Name: "Mid", LastMessageTimeMS: 150, Unread: true})

	got, err := st.ListConversations(ctx, store.ListConversationOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
	if got[0].ID != "new" || got[1].ID != "mid" || got[2].ID != "old" {
		t.Fatalf("wrong order: %s, %s, %s", got[0].ID, got[1].ID, got[2].ID)
	}

	// Unread-only filter.
	unread, err := st.ListConversations(ctx, store.ListConversationOpts{UnreadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(unread) != 1 || unread[0].ID != "mid" {
		t.Fatalf("unread filter: got %+v", unread)
	}
}

func TestListMessagesFilters(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	ids := seedThread(t, st, "c1", 6)

	all, err := st.ListMessages(ctx, store.ListMessageOpts{ConversationID: "c1", Order: "asc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 6 {
		t.Fatalf("expected 6 messages, got %d", len(all))
	}
	if all[0].ID != ids[0] || all[5].ID != ids[5] {
		t.Fatalf("asc ordering broken: %s..%s", all[0].ID, all[5].ID)
	}

	desc, _ := st.ListMessages(ctx, store.ListMessageOpts{ConversationID: "c1", Limit: 3})
	if len(desc) != 3 || desc[0].ID != ids[5] {
		t.Fatalf("desc/limit broken: %d hits, head=%s", len(desc), desc[0].ID)
	}

	mine, _ := st.ListMessages(ctx, store.ListMessageOpts{ConversationID: "c1", SenderID: "me"})
	if len(mine) != 3 {
		t.Fatalf("sender filter: got %d, want 3", len(mine))
	}
	for _, m := range mine {
		if !m.IsFromMe {
			t.Errorf("expected is_from_me on %s", m.ID)
		}
	}
}

func TestGetMessageMissingReturnsErrNotFound(t *testing.T) {
	st := openTempStore(t)
	_, err := st.GetMessage(context.Background(), "ghost")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestGetMessageContext(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	ids := seedThread(t, st, "c1", 10)

	out, err := st.GetMessageContext(ctx, ids[5], 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 5 {
		t.Fatalf("expected 5 messages, got %d", len(out))
	}
	want := []string{ids[3], ids[4], ids[5], ids[6], ids[7]}
	for i, m := range out {
		if m.ID != want[i] {
			t.Errorf("position %d: got %s, want %s", i, m.ID, want[i])
		}
	}

	// Context at the head clips to available messages.
	out, _ = st.GetMessageContext(ctx, ids[0], 5, 2)
	if len(out) != 3 {
		t.Fatalf("head context: got %d, want 3", len(out))
	}
	if out[0].ID != ids[0] {
		t.Fatalf("head context anchor: %s", out[0].ID)
	}
}

func TestSearchContactsLikeAndExact(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	must := func(c store.Contact) {
		t.Helper()
		if err := st.UpsertContact(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	must(store.Contact{ParticipantID: "p1", Name: "Alice Example", E164: "+15555550100"})
	must(store.Contact{ParticipantID: "p2", Name: "Bob Sample", E164: "+15555550200"})
	must(store.Contact{ParticipantID: "p3", Name: "Carol Alicethorpe", E164: "+15555550300"})

	hits, err := st.SearchContacts(ctx, "alice", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 alice-matches, got %d", len(hits))
	}

	byNum, err := st.GetContactByNumber(ctx, "+15555550200")
	if err != nil {
		t.Fatal(err)
	}
	if byNum.ParticipantID != "p2" {
		t.Errorf("got %s", byNum.ParticipantID)
	}

	_, err = st.GetContact(ctx, "ghost")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
