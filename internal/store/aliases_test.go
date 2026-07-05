package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/johnlindquist/gmkit/internal/store"
)

func TestAliasSetGetRemove(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	if err := st.SetAlias(ctx, store.AliasContact, "p1", "Mom"); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := st.GetAlias(ctx, store.AliasContact, "p1")
	if err != nil || got != "Mom" {
		t.Fatalf("get: got=%q err=%v", got, err)
	}

	// Update.
	if err := st.SetAlias(ctx, store.AliasContact, "p1", "Mama"); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetAlias(ctx, store.AliasContact, "p1")
	if got != "Mama" {
		t.Fatalf("update: got %q", got)
	}

	// Remove.
	if err := st.RemoveAlias(ctx, store.AliasContact, "p1"); err != nil {
		t.Fatal(err)
	}
	_, err = st.GetAlias(ctx, store.AliasContact, "p1")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after remove, got %v", err)
	}
	if err := st.RemoveAlias(ctx, store.AliasContact, "p1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("double-remove should be ErrNotFound: %v", err)
	}
}

func TestAliasOverlaysContactDisplayName(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	if err := st.UpsertContact(ctx, store.Contact{
		ParticipantID: "p1", Name: "Alice Example", E164: "+15555550100",
	}); err != nil {
		t.Fatal(err)
	}

	got, _ := st.GetContact(ctx, "p1")
	if got.DisplayName != "Alice Example" || got.Alias != "" {
		t.Fatalf("pre-alias: %+v", got)
	}

	if err := st.SetAlias(ctx, store.AliasContact, "p1", "Mom"); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetContact(ctx, "p1")
	if got.DisplayName != "Mom" || got.Alias != "Mom" {
		t.Fatalf("post-alias: %+v", got)
	}

	// SearchContacts must also surface the alias and rank by it.
	hits, err := st.SearchContacts(ctx, "mom", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Alias != "Mom" {
		t.Fatalf("alias search: got %+v", hits)
	}
}

func TestAliasSetRejectsEmptyInputs(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	if err := st.SetAlias(ctx, store.AliasContact, "", "Mom"); err == nil {
		t.Fatal("expected error on empty id")
	}
	if err := st.SetAlias(ctx, store.AliasContact, "p1", "  "); err == nil {
		t.Fatal("expected error on whitespace alias")
	}
	if err := st.SetAlias(ctx, "bogus", "p1", "Mom"); err == nil {
		t.Fatal("expected error on unknown target type")
	}
}

func TestListAliases(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	_ = st.SetAlias(ctx, store.AliasContact, "p1", "Mom")
	_ = st.SetAlias(ctx, store.AliasContact, "p2", "Dad")
	_ = st.SetAlias(ctx, store.AliasConversation, "c1", "Family")

	all, err := st.ListAliases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d, want 3", len(all))
	}
}
