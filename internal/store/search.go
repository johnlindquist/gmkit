package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// SearchOpts filters SearchMessagesRich. Query is required; everything else
// narrows the result set.
type SearchOpts struct {
	Query          string
	ConversationID string    // optional: scope to one chat
	Since          time.Time // optional lower bound (inclusive)
	Until          time.Time // optional upper bound (inclusive)
	Limit          int       // <=0 means 50
}

// RichHit is a search result enriched for direct consumption by humans and
// LLMs: display names and an ISO timestamp ride along so callers don't need
// follow-up lookups to make sense of a hit.
type RichHit struct {
	MessageID        string `json:"message_id"`
	ConversationID   string `json:"conversation_id"`
	ConversationName string `json:"conversation_name,omitempty"`
	SenderName       string `json:"sender_name,omitempty"`
	Body             string `json:"body"`
	Snippet          string `json:"snippet"`
	TimestampMS      int64  `json:"timestamp_ms"`
	TimestampISO     string `json:"timestamp_iso,omitempty"`
	IsFromMe         bool   `json:"is_from_me"`
}

// SearchMessagesRich runs an FTS5 search with graceful degradation: the
// query is tried verbatim first (full FTS5 syntax available), and if FTS5
// rejects it — natural-language input with apostrophes, question marks,
// unbalanced quotes — it is retried with every term quoted. Results carry
// conversation and sender display names.
func (s *Store) SearchMessagesRich(ctx context.Context, opts SearchOpts) ([]RichHit, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	hits, err := s.searchOnce(ctx, opts.Query, opts, limit)
	if err != nil {
		terms := searchableTerms(opts.Query)
		if len(terms) == 0 {
			// Nothing searchable survived (all terms too short for the
			// trigram index or pure punctuation): no matches, not an error.
			return []RichHit{}, nil
		}
		// Tier 2: all terms, quoted (implicit AND).
		fallback := joinQuoted(terms, " ")
		if fallback == opts.Query {
			return nil, err
		}
		hits, err = s.searchOnce(ctx, fallback, opts, limit)
		if err != nil {
			return nil, fmt.Errorf("fts search %q: %w", opts.Query, err)
		}
		// Tier 3: natural-language queries often contain words the message
		// doesn't ("who's" vs "who is"). If requiring every term found
		// nothing, take any term — still ordered newest-first, and callers
		// get snippets to judge relevance.
		if len(hits) == 0 && len(terms) > 1 {
			hits, err = s.searchOnce(ctx, joinQuoted(terms, " OR "), opts, limit)
			if err != nil {
				return nil, fmt.Errorf("fts search %q: %w", opts.Query, err)
			}
		}
	}
	s.attachConversationNames(ctx, hits)
	return hits, nil
}

func (s *Store) searchOnce(ctx context.Context, query string, opts SearchOpts, limit int) ([]RichHit, error) {
	q := strings.Builder{}
	q.WriteString(`
		SELECT m.message_id, m.conversation_id,
		       COALESCE(m.body, ''),
		       snippet(messages_fts, 1, '[', ']', ' … ', 12),
		       m.timestamp_ms, m.is_from_me,
		       COALESCE(aa.alias, ct.name, ct.e164, '') AS sender_name
		  FROM messages_fts
		  JOIN messages m ON m.message_id = messages_fts.message_id
		  LEFT JOIN contacts ct ON ct.participant_id = m.sender_id
		  LEFT JOIN aliases aa ON aa.target_type = 'contact' AND aa.target_id = m.sender_id
		 WHERE messages_fts MATCH ?`)
	args := []any{query}
	if opts.ConversationID != "" {
		q.WriteString(" AND m.conversation_id = ?")
		args = append(args, opts.ConversationID)
	}
	if !opts.Since.IsZero() {
		q.WriteString(" AND m.timestamp_ms >= ?")
		args = append(args, opts.Since.UnixMilli())
	}
	if !opts.Until.IsZero() {
		q.WriteString(" AND m.timestamp_ms <= ?")
		args = append(args, opts.Until.UnixMilli())
	}
	q.WriteString(" ORDER BY m.timestamp_ms DESC LIMIT ?")
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RichHit, 0)
	for rows.Next() {
		var h RichHit
		var fromMe int64
		if err := rows.Scan(&h.MessageID, &h.ConversationID, &h.Body, &h.Snippet,
			&h.TimestampMS, &fromMe, &h.SenderName); err != nil {
			return nil, fmt.Errorf("scan fts row: %w", err)
		}
		h.IsFromMe = fromMe != 0
		if h.IsFromMe {
			h.SenderName = "me"
		}
		h.TimestampISO = isoTime(h.TimestampMS)
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) attachConversationNames(ctx context.Context, hits []RichHit) {
	names := make(map[string]string)
	for i := range hits {
		id := hits[i].ConversationID
		name, ok := names[id]
		if !ok {
			if conv, err := s.GetConversation(ctx, id); err == nil {
				name = conv.DisplayName()
			}
			names[id] = name
		}
		hits[i].ConversationName = name
	}
}

// fallbackStopwords are high-frequency function words dropped from fallback
// queries: they match nearly every message, so they only add noise —
// especially in the OR tier. Words a user types in precise FTS5 syntax are
// never filtered (the raw query is always tried first).
var fallbackStopwords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "are": {}, "was": {}, "were": {},
	"you": {}, "your": {}, "has": {}, "have": {}, "had": {}, "this": {},
	"that": {}, "with": {}, "from": {}, "they": {}, "them": {}, "then": {},
	"what": {}, "when": {}, "where": {}, "who": {}, "why": {}, "how": {},
	"did": {}, "does": {}, "will": {}, "would": {}, "could": {}, "should": {},
	"can": {}, "get": {}, "got": {}, "our": {}, "out": {}, "not": {},
	"but": {}, "all": {}, "any": {}, "about": {}, "there": {}, "here": {},
	"just": {}, "into": {}, "some": {}, "been": {},
}

// searchableTerms extracts the FTS-matchable terms from free-form text.
// Each whitespace-separated term is stripped of leading/trailing punctuation
// (which would otherwise have to appear literally in the message), dropped
// if shorter than the trigram minimum (3 runes — such terms can never match
// and would AND the whole query to zero), and dropped if it is a common
// stopword. `who's coming to the party?` -> [who's coming party].
func searchableTerms(q string) []string {
	var terms []string
	for _, f := range strings.Fields(q) {
		f = strings.TrimFunc(f, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		})
		if utf8.RuneCountInString(f) < 3 {
			continue
		}
		if _, stop := fallbackStopwords[strings.ToLower(f)]; stop {
			continue
		}
		terms = append(terms, f)
	}
	return terms
}

// joinQuoted builds an FTS5 query from literal terms: each is quoted (with
// embedded quotes doubled) and joined by sep (" " for AND, " OR " for OR).
func joinQuoted(terms []string, sep string) string {
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	return strings.Join(quoted, sep)
}

// FindConversations resolves a person, group name, or phone number fragment
// to conversations, newest-activity first. It matches the conversation name,
// a local alias, participant names/numbers embedded in participants_json,
// and — via the contacts table — contact names and aliases.
func (s *Store) FindConversations(ctx context.Context, query string, limit int) ([]Conversation, error) {
	if limit <= 0 {
		limit = 25
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return s.ListConversations(ctx, ListConversationOpts{Limit: limit})
	}
	like := "%" + escapeLike(query) + "%"

	clauses := []string{
		`c.name LIKE ? ESCAPE '\'`,
		`a.alias LIKE ? ESCAPE '\'`,
		`c.participants_json LIKE ? ESCAPE '\'`,
	}
	args := []any{like, like, like}

	// Names the user knows may live only in the contacts table (or as a
	// local alias); map matches back through participant IDs.
	if contacts, err := s.SearchContacts(ctx, query, 10); err == nil {
		for _, contact := range contacts {
			if contact.ParticipantID == "" {
				continue
			}
			clauses = append(clauses, `c.participants_json LIKE ? ESCAPE '\'`)
			args = append(args, `%"id":"`+escapeLike(contact.ParticipantID)+`"%`)
		}
	}

	q := `
		SELECT c.conversation_id, c.source_platform, c.name, c.is_group, c.participants_json,
		       c.last_message_ts, c.unread, c.pinned, c.archived, c.updated_at
		  FROM conversations c
		  LEFT JOIN aliases a ON a.target_type = 'conversation' AND a.target_id = c.conversation_id
		 WHERE ` + strings.Join(clauses, " OR ") + `
		 ORDER BY c.last_message_ts DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("find conversations: %w", err)
	}
	defer rows.Close()
	out := make([]Conversation, 0)
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// participantSummary is the subset of participant fields embedded in
// conversations.participants_json that display logic needs.
type participantSummary struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	E164            string `json:"e164"`
	FormattedNumber string `json:"formatted_number"`
	IsMe            bool   `json:"is_me"`
}

// DisplayName returns the best human-readable label for a conversation:
// the explicit name if set, otherwise the other participants' names or
// numbers, otherwise the conversation ID.
func (c Conversation) DisplayName() string {
	if strings.TrimSpace(c.Name) != "" {
		return c.Name
	}
	var parts []participantSummary
	_ = json.Unmarshal([]byte(c.ParticipantsJSON), &parts)
	names := make([]string, 0, len(parts))
	for _, p := range parts {
		if p.IsMe {
			continue
		}
		switch {
		case p.Name != "":
			names = append(names, p.Name)
		case p.E164 != "":
			names = append(names, p.E164)
		case p.FormattedNumber != "":
			names = append(names, p.FormattedNumber)
		}
	}
	if len(names) == 0 {
		return c.ID
	}
	return strings.Join(names, ", ")
}

// ParticipantNames lists the display names (or numbers) of everyone in the
// conversation except the local user.
func (c Conversation) ParticipantNames() []string {
	var parts []participantSummary
	_ = json.Unmarshal([]byte(c.ParticipantsJSON), &parts)
	names := make([]string, 0, len(parts))
	for _, p := range parts {
		if p.IsMe {
			continue
		}
		switch {
		case p.Name != "":
			names = append(names, p.Name)
		case p.E164 != "":
			names = append(names, p.E164)
		case p.FormattedNumber != "":
			names = append(names, p.FormattedNumber)
		}
	}
	return names
}

// RichMessage is a Message plus resolved display fields, for consumers
// (LLMs, humans) that shouldn't need extra lookups per row.
type RichMessage struct {
	Message
	SenderName   string `json:"sender_name,omitempty"`
	TimestampISO string `json:"timestamp_iso,omitempty"`
}

// EnrichMessages resolves sender display names (alias > contact name >
// number > raw participant id) and ISO timestamps for a message slice.
func (s *Store) EnrichMessages(ctx context.Context, msgs []Message) []RichMessage {
	cache := make(map[string]string)
	out := make([]RichMessage, 0, len(msgs))
	for _, m := range msgs {
		rm := RichMessage{Message: m, TimestampISO: isoTime(m.TimestampMS)}
		if m.IsFromMe {
			rm.SenderName = "me"
		} else if m.SenderID != "" {
			name, ok := cache[m.SenderID]
			if !ok {
				if contact, err := s.GetContact(ctx, m.SenderID); err == nil {
					name = contact.DisplayName
					if name == "" {
						name = contact.E164
					}
				}
				if name == "" {
					name = m.SenderID
				}
				cache[m.SenderID] = name
			}
			rm.SenderName = name
		}
		out = append(out, rm)
	}
	return out
}

func isoTime(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).Local().Format(time.RFC3339)
}

// escapeLike escapes LIKE wildcards so user input matches literally.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}
