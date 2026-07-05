package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func seedSearchStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	if err := st.UpsertConversation(ctx, Conversation{
		ID:               "c1",
		ParticipantsJSON: `[{"id":"p1","name":"Alice Chen","e164":"+15551230001","is_me":false},{"id":"me","is_me":true}]`,
	}); err != nil {
		t.Fatalf("seed conv: %v", err)
	}
	if err := st.UpsertConversation(ctx, Conversation{
		ID:               "c2",
		Name:             "Weekend Crew",
		IsGroup:          true,
		ParticipantsJSON: `[{"id":"p2","name":"Bob","e164":"+15551230002","is_me":false}]`,
	}); err != nil {
		t.Fatalf("seed conv: %v", err)
	}
	if err := st.UpsertContact(ctx, Contact{ParticipantID: "p1", Name: "Alice Chen", E164: "+15551230001"}); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	if err := st.UpsertContact(ctx, Contact{ParticipantID: "p2", Name: "Bob", E164: "+15551230002"}); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	msgs := []struct {
		id, conv, sender, body string
		ts                     int64
		fromMe                 bool
	}{
		{"m1", "c1", "p1", "who's coming to the pizza party?", 1_000_000, false},
		{"m2", "c1", "me", "pizza sounds great", 2_000_000, true},
		{"m3", "c2", "p2", "camping tent packed", 3_000_000, false},
	}
	for _, m := range msgs {
		body := m.body
		if err := st.UpsertMessage(ctx, Message{
			ID: m.id, ConversationID: m.conv, SenderID: m.sender,
			Body: &body, TimestampMS: m.ts, IsFromMe: m.fromMe,
		}); err != nil {
			t.Fatalf("seed msg %s: %v", m.id, err)
		}
	}
	return st
}

func TestSearchMessagesRichEnrichment(t *testing.T) {
	st := seedSearchStore(t)
	hits, err := st.SearchMessagesRich(context.Background(), SearchOpts{Query: "pizza"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2: %+v", len(hits), hits)
	}
	// Newest first: m2 (from me) then m1 (Alice).
	if hits[0].SenderName != "me" {
		t.Errorf("hit0 sender = %q, want me", hits[0].SenderName)
	}
	if hits[1].SenderName != "Alice Chen" {
		t.Errorf("hit1 sender = %q, want Alice Chen", hits[1].SenderName)
	}
	if hits[0].ConversationName != "Alice Chen" {
		t.Errorf("conversation name = %q, want Alice Chen (from participants)", hits[0].ConversationName)
	}
	if hits[0].TimestampISO == "" {
		t.Error("timestamp_iso missing")
	}
}

func TestSearchMessagesRichNaturalLanguageFallback(t *testing.T) {
	st := seedSearchStore(t)
	ctx := context.Background()

	// Apostrophe + question mark: invalid FTS5 syntax that must succeed via
	// the quoted-term fallback ("to" is dropped as sub-trigram, trailing
	// "?" is trimmed from "coming?").
	hits, err := st.SearchMessagesRich(ctx, SearchOpts{Query: `who's coming to?`})
	if err != nil {
		t.Fatalf("fallback search: %v", err)
	}
	if len(hits) != 1 || hits[0].MessageID != "m1" {
		t.Fatalf("fallback hits: %+v", hits)
	}

	// Nothing searchable at all: empty result, not an error.
	hits, err = st.SearchMessagesRich(ctx, SearchOpts{Query: `a b ?!`})
	if err != nil || len(hits) != 0 {
		t.Fatalf("unsearchable query: %+v err=%v", hits, err)
	}
}

func TestSearchMessagesRichFilters(t *testing.T) {
	st := seedSearchStore(t)
	ctx := context.Background()

	hits, err := st.SearchMessagesRich(ctx, SearchOpts{Query: "pizza", ConversationID: "c2"})
	if err != nil || len(hits) != 0 {
		t.Fatalf("conv filter: got %d hits err=%v, want 0", len(hits), err)
	}
	hits, err = st.SearchMessagesRich(ctx, SearchOpts{
		Query: "pizza",
		Since: time.UnixMilli(1_500_000),
	})
	if err != nil || len(hits) != 1 || hits[0].MessageID != "m2" {
		t.Fatalf("since filter: %+v err=%v", hits, err)
	}
	hits, err = st.SearchMessagesRich(ctx, SearchOpts{
		Query: "pizza",
		Until: time.UnixMilli(1_500_000),
	})
	if err != nil || len(hits) != 1 || hits[0].MessageID != "m1" {
		t.Fatalf("until filter: %+v err=%v", hits, err)
	}
}

func TestFindConversations(t *testing.T) {
	st := seedSearchStore(t)
	ctx := context.Background()

	// By participant name embedded in participants_json.
	convs, err := st.FindConversations(ctx, "alice", 0)
	if err != nil || len(convs) != 1 || convs[0].ID != "c1" {
		t.Fatalf("by name: %+v err=%v", convs, err)
	}
	// By group conversation name.
	convs, err = st.FindConversations(ctx, "weekend", 0)
	if err != nil || len(convs) != 1 || convs[0].ID != "c2" {
		t.Fatalf("by group name: %+v err=%v", convs, err)
	}
	// By phone number fragment.
	convs, err = st.FindConversations(ctx, "1230002", 0)
	if err != nil || len(convs) != 1 || convs[0].ID != "c2" {
		t.Fatalf("by number: %+v err=%v", convs, err)
	}
	// Via a contact alias that appears nowhere in conversations.
	if err := st.SetAlias(ctx, "contact", "p1", "Mom"); err != nil {
		t.Fatalf("set alias: %v", err)
	}
	convs, err = st.FindConversations(ctx, "mom", 0)
	if err != nil || len(convs) != 1 || convs[0].ID != "c1" {
		t.Fatalf("by alias: %+v err=%v", convs, err)
	}
}

func TestConversationDisplayName(t *testing.T) {
	named := Conversation{ID: "x", Name: "Group"}
	if named.DisplayName() != "Group" {
		t.Errorf("named: %q", named.DisplayName())
	}
	fromParts := Conversation{
		ID:               "y",
		ParticipantsJSON: `[{"id":"a","name":"Ann","is_me":false},{"id":"b","e164":"+1555","is_me":false},{"id":"me","is_me":true}]`,
	}
	if got := fromParts.DisplayName(); got != "Ann, +1555" {
		t.Errorf("from participants: %q", got)
	}
	bare := Conversation{ID: "z", ParticipantsJSON: "[]"}
	if bare.DisplayName() != "z" {
		t.Errorf("bare: %q", bare.DisplayName())
	}
}

func TestEnrichMessages(t *testing.T) {
	st := seedSearchStore(t)
	ctx := context.Background()
	msgs, err := st.ListMessages(ctx, ListMessageOpts{ConversationID: "c1", Order: "asc"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	rich := st.EnrichMessages(ctx, msgs)
	if len(rich) != 2 {
		t.Fatalf("rich len %d", len(rich))
	}
	if rich[0].SenderName != "Alice Chen" || rich[1].SenderName != "me" {
		t.Fatalf("sender names: %q, %q", rich[0].SenderName, rich[1].SenderName)
	}
	if rich[0].TimestampISO == "" {
		t.Error("timestamp_iso missing")
	}
}

func TestSearchMessagesRichORFallbackTier(t *testing.T) {
	st := seedSearchStore(t)
	// "who's" never appears (body says "who is"), so the AND fallback finds
	// nothing; the OR tier still surfaces the tent message via "tent".
	hits, err := st.SearchMessagesRich(context.Background(), SearchOpts{Query: `who's bringing the tent?`})
	if err != nil {
		t.Fatalf("or-tier search: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.MessageID == "m3" {
			found = true
		}
	}
	if !found {
		t.Fatalf("or-tier should surface m3: %+v", hits)
	}
}

func TestSearchMessagesRichORTierOnValidQueryZeroHits(t *testing.T) {
	st := seedSearchStore(t)
	ctx := context.Background()
	// Valid FTS5 (implicit AND) but no single message has both terms; the
	// OR tier should surface per-term matches.
	hits, err := st.SearchMessagesRich(ctx, SearchOpts{Query: "camping pizza"})
	if err != nil || len(hits) == 0 {
		t.Fatalf("or tier on valid query: %+v err=%v", hits, err)
	}
	// Explicit FTS syntax stays exact: a quoted phrase with no hits must
	// NOT degrade to OR.
	hits, err = st.SearchMessagesRich(ctx, SearchOpts{Query: `"camping pizza"`})
	if err != nil || len(hits) != 0 {
		t.Fatalf("phrase should stay exact: %+v err=%v", hits, err)
	}
}
