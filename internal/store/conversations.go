package store

import (
	"context"
	"fmt"
	"time"
)

// Conversation is the storage shape for a chat thread. participants_json
// holds the libgm Participant array as JSON to avoid a join table at this
// stage — Phase 3 may normalize if query patterns demand it.
type Conversation struct {
	ID                string
	SourcePlatform    string
	Name              string
	IsGroup           bool
	ParticipantsJSON  string
	LastMessageTimeMS int64
	Unread            bool
	Pinned            bool
	Archived          bool
	UpdatedAt         time.Time
}

// UpsertConversation inserts or updates a conversation row by ID.
func (s *Store) UpsertConversation(ctx context.Context, c Conversation) error {
	if c.ID == "" {
		return fmt.Errorf("conversation id is required")
	}
	platform := c.SourcePlatform
	if platform == "" {
		platform = "gm"
	}
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO conversations (
			conversation_id, source_platform, name, is_group, participants_json,
			last_message_ts, unread, pinned, archived, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(conversation_id) DO UPDATE SET
			source_platform   = excluded.source_platform,
			name              = excluded.name,
			is_group          = excluded.is_group,
			participants_json = excluded.participants_json,
			last_message_ts   = MAX(conversations.last_message_ts, excluded.last_message_ts),
			unread            = excluded.unread,
			pinned            = excluded.pinned,
			archived          = excluded.archived,
			updated_at        = excluded.updated_at
	`,
		c.ID, platform, c.Name, boolToInt(c.IsGroup), nullableJSON(c.ParticipantsJSON),
		c.LastMessageTimeMS, boolToInt(c.Unread), boolToInt(c.Pinned), boolToInt(c.Archived), now,
	)
	if err != nil {
		return fmt.Errorf("upsert conversation %s: %w", c.ID, err)
	}
	return nil
}

// GetConversation fetches a single row. Returns sql.ErrNoRows on miss.
func (s *Store) GetConversation(ctx context.Context, id string) (Conversation, error) {
	var c Conversation
	var lastMsg, updated int64
	var isGroup, unread, pinned, archived int64
	err := s.db.QueryRowContext(ctx, `
		SELECT conversation_id, source_platform, name, is_group, participants_json,
		       last_message_ts, unread, pinned, archived, updated_at
		  FROM conversations
		 WHERE conversation_id = ?
	`, id).Scan(
		&c.ID, &c.SourcePlatform, &c.Name, &isGroup, &c.ParticipantsJSON,
		&lastMsg, &unread, &pinned, &archived, &updated,
	)
	if err != nil {
		return Conversation{}, err
	}
	c.IsGroup = isGroup != 0
	c.Unread = unread != 0
	c.Pinned = pinned != 0
	c.Archived = archived != 0
	c.LastMessageTimeMS = lastMsg
	c.UpdatedAt = time.UnixMilli(updated)
	return c, nil
}

// CountConversations returns the total number of stored conversations.
func (s *Store) CountConversations(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations`).Scan(&n)
	return n, err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullableJSON(s string) any {
	if s == "" {
		return "[]"
	}
	return s
}

