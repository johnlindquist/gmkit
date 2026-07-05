package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Approval statuses. Lifecycle: pending -> sent | failed | denied | canceled.
// Terminal states never transition again.
const (
	ApprovalPending  = "pending"
	ApprovalSent     = "sent"
	ApprovalFailed   = "failed"
	ApprovalDenied   = "denied"
	ApprovalCanceled = "canceled"
)

// ErrApprovalResolved is returned when a resolve races: the row exists but is
// no longer pending.
var ErrApprovalResolved = errors.New("approval already resolved")

// Approval is one proposed outgoing message awaiting (or past) human review.
type Approval struct {
	ID             string  `json:"approval_id"`
	ConversationID string  `json:"conversation_id"`
	Body           string  `json:"body"`
	ReplyToID      *string `json:"reply_to_id,omitempty"`
	RequestedBy    string  `json:"requested_by"`
	Status         string  `json:"status"`
	Error          *string `json:"error,omitempty"`
	MessageID      *string `json:"message_id,omitempty"`
	CreatedAtMS    int64   `json:"created_at_ms"`
	UpdatedAtMS    int64   `json:"updated_at_ms"`
}

// CreateApproval inserts a new pending approval row.
func (s *Store) CreateApproval(ctx context.Context, a Approval) error {
	if a.ID == "" {
		return fmt.Errorf("approval id is required")
	}
	if a.ConversationID == "" {
		return fmt.Errorf("conversation id is required")
	}
	if a.Body == "" {
		return fmt.Errorf("body is required")
	}
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO approvals (
			approval_id, conversation_id, body, reply_to_id,
			requested_by, status, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, 'pending', ?, ?)
	`, a.ID, a.ConversationID, a.Body, a.ReplyToID, a.RequestedBy, now, now)
	if err != nil {
		return fmt.Errorf("create approval %s: %w", a.ID, err)
	}
	return nil
}

// GetApproval fetches one approval by ID. Returns ErrNotFound on miss.
func (s *Store) GetApproval(ctx context.Context, id string) (Approval, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT approval_id, conversation_id, body, reply_to_id, requested_by,
		       status, error, message_id, created_at, updated_at
		  FROM approvals
		 WHERE approval_id = ?`, id)
	a, err := scanApproval(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, ErrNotFound
	}
	return a, err
}

// ListApprovals returns approvals, newest first. status filters when
// non-empty; limit <=0 means 50.
func (s *Store) ListApprovals(ctx context.Context, status string, limit int) ([]Approval, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `
		SELECT approval_id, conversation_id, body, reply_to_id, requested_by,
		       status, error, message_id, created_at, updated_at
		  FROM approvals`
	var args []any
	if status != "" {
		q += " WHERE status = ?"
		args = append(args, status)
	}
	q += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list approvals: %w", err)
	}
	defer rows.Close()
	out := make([]Approval, 0)
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ResolveApproval transitions a pending approval to a terminal status,
// recording the sent message ID or failure detail. Returns ErrNotFound if the
// row does not exist and ErrApprovalResolved if it is no longer pending —
// guarding against double-approve races between two clients.
func (s *Store) ResolveApproval(ctx context.Context, id, status string, errMsg, messageID *string) error {
	switch status {
	case ApprovalSent, ApprovalFailed, ApprovalDenied, ApprovalCanceled:
	default:
		return fmt.Errorf("invalid terminal approval status %q", status)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE approvals
		   SET status = ?, error = ?, message_id = ?, updated_at = ?
		 WHERE approval_id = ? AND status = 'pending'
	`, status, errMsg, messageID, time.Now().UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("resolve approval %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		if _, err := s.GetApproval(ctx, id); err != nil {
			return err
		}
		return ErrApprovalResolved
	}
	return nil
}

func scanApproval(r interface {
	Scan(...any) error
}) (Approval, error) {
	var a Approval
	var replyTo, errMsg, msgID sql.NullString
	if err := r.Scan(
		&a.ID, &a.ConversationID, &a.Body, &replyTo, &a.RequestedBy,
		&a.Status, &errMsg, &msgID, &a.CreatedAtMS, &a.UpdatedAtMS,
	); err != nil {
		return Approval{}, err
	}
	if replyTo.Valid {
		s := replyTo.String
		a.ReplyToID = &s
	}
	if errMsg.Valid {
		s := errMsg.String
		a.Error = &s
	}
	if msgID.Valid {
		s := msgID.String
		a.MessageID = &s
	}
	return a, nil
}
