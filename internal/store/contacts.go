package store

import (
	"context"
	"fmt"
	"time"
)

// Contact is the storage shape for an address-book entry. ParticipantID is
// the libgm-stable ID and forms the primary key; ContactID is Google's
// contact-database ID (may be empty for non-saved numbers).
type Contact struct {
	ParticipantID   string
	SourcePlatform  string
	ContactID       string
	Name            string
	E164            string
	FormattedNumber string
	AvatarColor     string
	IsMe            bool
}

// UpsertContact inserts or updates a contact row by ParticipantID.
func (s *Store) UpsertContact(ctx context.Context, c Contact) error {
	if c.ParticipantID == "" {
		return fmt.Errorf("participant id is required")
	}
	platform := c.SourcePlatform
	if platform == "" {
		platform = "gm"
	}
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO contacts (
			participant_id, source_platform, contact_id, name, e164,
			formatted_number, avatar_color, is_me, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(participant_id) DO UPDATE SET
			source_platform  = excluded.source_platform,
			contact_id       = excluded.contact_id,
			name             = excluded.name,
			e164             = excluded.e164,
			formatted_number = excluded.formatted_number,
			avatar_color     = excluded.avatar_color,
			is_me            = excluded.is_me,
			updated_at       = excluded.updated_at
	`,
		c.ParticipantID, platform, c.ContactID, c.Name, c.E164,
		c.FormattedNumber, c.AvatarColor, boolToInt(c.IsMe), now,
	)
	if err != nil {
		return fmt.Errorf("upsert contact %s: %w", c.ParticipantID, err)
	}
	return nil
}

// CountContacts returns the total number of stored contacts.
func (s *Store) CountContacts(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM contacts`).Scan(&n)
	return n, err
}
