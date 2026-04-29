package store

import (
	"context"
	"fmt"
	"time"
)

// Message is the storage shape for a single message. Body is the plaintext
// content if any (nil for media-only). MediaID/MimeType/DecryptionKey
// describe the attachment if present; the bytes themselves are not stored
// here — Phase 3 will add an explicit `media download` command.
type Message struct {
	ID             string
	ConversationID string
	SourcePlatform string
	SenderID       string
	Body           *string
	TimestampMS    int64
	Status         int64
	IsFromMe       bool
	MediaID        *string
	MimeType       *string
	DecryptionKey  []byte
	ReactionsJSON  *string
	ReplyToID      *string
	RawProto       []byte
}

// UpsertMessage inserts or updates a message row by ID.
func (s *Store) UpsertMessage(ctx context.Context, m Message) error {
	if m.ID == "" {
		return fmt.Errorf("message id is required")
	}
	if m.ConversationID == "" {
		return fmt.Errorf("conversation id is required")
	}
	platform := m.SourcePlatform
	if platform == "" {
		platform = "gm"
	}
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO messages (
			message_id, conversation_id, source_platform, sender_id,
			body, timestamp_ms, status, is_from_me,
			media_id, mime_type, decryption_key,
			reactions_json, reply_to_id, raw_proto, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(message_id) DO UPDATE SET
			conversation_id = excluded.conversation_id,
			source_platform = excluded.source_platform,
			sender_id       = excluded.sender_id,
			body            = excluded.body,
			timestamp_ms    = excluded.timestamp_ms,
			status          = excluded.status,
			is_from_me      = excluded.is_from_me,
			media_id        = excluded.media_id,
			mime_type       = excluded.mime_type,
			decryption_key  = excluded.decryption_key,
			reactions_json  = excluded.reactions_json,
			reply_to_id     = excluded.reply_to_id,
			raw_proto       = excluded.raw_proto,
			updated_at      = excluded.updated_at
	`,
		m.ID, m.ConversationID, platform, m.SenderID,
		m.Body, m.TimestampMS, m.Status, boolToInt(m.IsFromMe),
		m.MediaID, m.MimeType, nullBytes(m.DecryptionKey),
		m.ReactionsJSON, m.ReplyToID, nullBytes(m.RawProto), now,
	)
	if err != nil {
		return fmt.Errorf("upsert message %s: %w", m.ID, err)
	}
	return nil
}

// CountMessages returns the total number of stored messages.
func (s *Store) CountMessages(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&n)
	return n, err
}

// SearchHit is one FTS result. Snippet is the FTS5-generated highlighted
// excerpt around the match, suitable for display in --json output.
type SearchHit struct {
	MessageID      string
	ConversationID string
	Body           string
	Snippet        string
	TimestampMS    int64
	IsFromMe       bool
}

// SearchMessages runs an FTS5 MATCH against messages_fts. limit caps the
// result count. The query string is passed to FTS5 verbatim, so callers can
// use the standard syntax (phrase quotes, NEAR(), AND/OR/NOT).
func (s *Store) SearchMessages(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.message_id, m.conversation_id,
		       COALESCE(m.body, ''),
		       snippet(messages_fts, 1, '[', ']', ' … ', 12),
		       m.timestamp_ms, m.is_from_me
		  FROM messages_fts
		  JOIN messages m ON m.message_id = messages_fts.message_id
		 WHERE messages_fts MATCH ?
		 ORDER BY m.timestamp_ms DESC
		 LIMIT ?
	`, query, limit)
	if err != nil {
		return nil, fmt.Errorf("fts search %q: %w", query, err)
	}
	defer rows.Close()
	var out []SearchHit
	for rows.Next() {
		var h SearchHit
		var fromMe int64
		if err := rows.Scan(&h.MessageID, &h.ConversationID, &h.Body, &h.Snippet, &h.TimestampMS, &fromMe); err != nil {
			return nil, fmt.Errorf("scan fts row: %w", err)
		}
		h.IsFromMe = fromMe != 0
		out = append(out, h)
	}
	return out, rows.Err()
}

func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
