package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/johnlindquist/gmkit/internal/history"
	"github.com/johnlindquist/gmkit/internal/store"
)

func (s *Server) dispatch(ctx context.Context, c *serverConn, method string, params json.RawMessage) (any, *Error) {
	switch method {
	case "ping":
		return s.handlePing(ctx)
	case "status":
		return s.handleStatus(ctx)
	case "subscribe":
		c.subscribed.Store(true)
		return map[string]any{"subscribed": true}, nil
	case "chats.list":
		return s.handleChatsList(ctx, params)
	case "chats.find":
		return s.handleChatsFind(ctx, params)
	case "chats.show":
		return s.handleChatsShow(ctx, params)
	case "messages.list":
		return s.handleMessagesList(ctx, params)
	case "messages.search":
		return s.handleMessagesSearch(ctx, params)
	case "messages.show":
		return s.handleMessagesShow(ctx, params)
	case "messages.context":
		return s.handleMessagesContext(ctx, params)
	case "contacts.search":
		return s.handleContactsSearch(ctx, params)
	case "contacts.show":
		return s.handleContactsShow(ctx, params)
	case "sync.refresh":
		return s.handleSyncRefresh(ctx)
	case "history.backfill":
		return s.handleHistoryBackfill(ctx, params)
	case "history.lookup":
		return s.handleHistoryLookup(ctx, params)
	case "send.text":
		return s.handleSendText(ctx, params)
	case "approvals.list":
		return s.handleApprovalsList(ctx, params)
	case "approvals.approve":
		return s.handleApprovalsApprove(ctx, params)
	case "approvals.deny":
		return s.handleApprovalsDeny(ctx, params)
	default:
		return nil, &Error{Code: CodeMethodNotFound, Message: "unknown method " + method}
	}
}

func decode[T any](params json.RawMessage) (T, *Error) {
	var v T
	if len(params) == 0 {
		return v, nil
	}
	if err := json.Unmarshal(params, &v); err != nil {
		return v, &Error{Code: CodeInvalidParams, Message: "invalid params: " + err.Error()}
	}
	return v, nil
}

func internalErr(err error) *Error {
	return &Error{Code: CodeInternal, Message: err.Error()}
}

func (s *Server) handlePing(ctx context.Context) (any, *Error) {
	version, err := s.deps.Store.SchemaVersion(ctx)
	if err != nil {
		return nil, internalErr(err)
	}
	return map[string]any{
		"pong":           true,
		"version":        s.deps.Version,
		"schema_version": version,
	}, nil
}

func (s *Server) handleStatus(ctx context.Context) (any, *Error) {
	sync, err := s.deps.Store.SyncState(ctx)
	if err != nil {
		return nil, internalErr(err)
	}
	pending, err := s.deps.Store.ListApprovals(ctx, store.ApprovalPending, 1000)
	if err != nil {
		return nil, internalErr(err)
	}
	conversations, _ := s.deps.Store.CountConversations(ctx)
	messages, _ := s.deps.Store.CountMessages(ctx)
	connected := false
	if s.deps.Client != nil {
		connected = s.deps.Client.IsConnected()
	}
	return map[string]any{
		"connected":         connected,
		"send_mode":         s.deps.SendMode,
		"pending_approvals": len(pending),
		"conversations":     conversations,
		"messages":          messages,
		"last_event_ms":     sync.LastEventTime.UnixMilli(),
		"last_connect_ms":   sync.LastConnectTime.UnixMilli(),
		"updated_at_ms":     sync.UpdatedAt.UnixMilli(),
	}, nil
}

type chatsListParams struct {
	Limit      int  `json:"limit"`
	UnreadOnly bool `json:"unread_only"`
	Pinned     bool `json:"pinned"`
}

func (s *Server) handleChatsList(ctx context.Context, params json.RawMessage) (any, *Error) {
	p, perr := decode[chatsListParams](params)
	if perr != nil {
		return nil, perr
	}
	convs, err := s.deps.Store.ListConversations(ctx, store.ListConversationOpts{
		Limit:      p.Limit,
		UnreadOnly: p.UnreadOnly,
		Pinned:     p.Pinned,
	})
	if err != nil {
		return nil, internalErr(err)
	}
	return convs, nil
}

type chatsFindParams struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

func (s *Server) handleChatsFind(ctx context.Context, params json.RawMessage) (any, *Error) {
	p, perr := decode[chatsFindParams](params)
	if perr != nil {
		return nil, perr
	}
	if p.Query == "" {
		return nil, &Error{Code: CodeInvalidParams, Message: "query is required"}
	}
	convs, err := s.deps.Store.FindConversations(ctx, p.Query, p.Limit)
	if err != nil {
		return nil, internalErr(err)
	}
	return convs, nil
}

type chatsShowParams struct {
	ConversationID string `json:"conversation_id"`
	Limit          int    `json:"limit"`
}

func (s *Server) handleChatsShow(ctx context.Context, params json.RawMessage) (any, *Error) {
	p, perr := decode[chatsShowParams](params)
	if perr != nil {
		return nil, perr
	}
	if p.ConversationID == "" {
		return nil, &Error{Code: CodeInvalidParams, Message: "conversation_id is required"}
	}
	conv, err := s.deps.Store.GetConversation(ctx, p.ConversationID)
	if err != nil {
		return nil, &Error{Code: CodeNotFound, Message: "conversation not found: " + p.ConversationID}
	}
	msgs, err := s.deps.Store.ListMessages(ctx, store.ListMessageOpts{
		ConversationID: p.ConversationID,
		Limit:          p.Limit,
	})
	if err != nil {
		return nil, internalErr(err)
	}
	return map[string]any{"conversation": conv, "messages": s.deps.Store.EnrichMessages(ctx, msgs)}, nil
}

type messagesListParams struct {
	ConversationID string `json:"conversation_id"`
	SenderID       string `json:"sender_id"`
	SinceMS        int64  `json:"since_ms"`
	UntilMS        int64  `json:"until_ms"`
	Limit          int    `json:"limit"`
	Order          string `json:"order"`
}

func (s *Server) handleMessagesList(ctx context.Context, params json.RawMessage) (any, *Error) {
	p, perr := decode[messagesListParams](params)
	if perr != nil {
		return nil, perr
	}
	opts := store.ListMessageOpts{
		ConversationID: p.ConversationID,
		SenderID:       p.SenderID,
		Limit:          p.Limit,
		Order:          p.Order,
	}
	if p.SinceMS > 0 {
		opts.Since = time.UnixMilli(p.SinceMS)
	}
	if p.UntilMS > 0 {
		opts.Until = time.UnixMilli(p.UntilMS)
	}
	msgs, err := s.deps.Store.ListMessages(ctx, opts)
	if err != nil {
		return nil, internalErr(err)
	}
	return s.deps.Store.EnrichMessages(ctx, msgs), nil
}

type messagesSearchParams struct {
	Query          string `json:"query"`
	ConversationID string `json:"conversation_id"`
	SinceMS        int64  `json:"since_ms"`
	UntilMS        int64  `json:"until_ms"`
	Limit          int    `json:"limit"`
}

func (s *Server) handleMessagesSearch(ctx context.Context, params json.RawMessage) (any, *Error) {
	p, perr := decode[messagesSearchParams](params)
	if perr != nil {
		return nil, perr
	}
	if p.Query == "" {
		return nil, &Error{Code: CodeInvalidParams, Message: "query is required"}
	}
	opts := store.SearchOpts{
		Query:          p.Query,
		ConversationID: p.ConversationID,
		Limit:          p.Limit,
	}
	if p.SinceMS > 0 {
		opts.Since = time.UnixMilli(p.SinceMS)
	}
	if p.UntilMS > 0 {
		opts.Until = time.UnixMilli(p.UntilMS)
	}
	hits, err := s.deps.Store.SearchMessagesRich(ctx, opts)
	if err != nil {
		return nil, internalErr(err)
	}
	return hits, nil
}

type messagesShowParams struct {
	MessageID string `json:"message_id"`
}

func (s *Server) handleMessagesShow(ctx context.Context, params json.RawMessage) (any, *Error) {
	p, perr := decode[messagesShowParams](params)
	if perr != nil {
		return nil, perr
	}
	if p.MessageID == "" {
		return nil, &Error{Code: CodeInvalidParams, Message: "message_id is required"}
	}
	msg, err := s.deps.Store.GetMessage(ctx, p.MessageID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, &Error{Code: CodeNotFound, Message: "message not found: " + p.MessageID}
	}
	if err != nil {
		return nil, internalErr(err)
	}
	return msg, nil
}

type messagesContextParams struct {
	MessageID string `json:"message_id"`
	Before    int    `json:"before"`
	After     int    `json:"after"`
}

func (s *Server) handleMessagesContext(ctx context.Context, params json.RawMessage) (any, *Error) {
	p, perr := decode[messagesContextParams](params)
	if perr != nil {
		return nil, perr
	}
	if p.MessageID == "" {
		return nil, &Error{Code: CodeInvalidParams, Message: "message_id is required"}
	}
	if p.Before == 0 && p.After == 0 {
		p.Before, p.After = 5, 5
	}
	msgs, err := s.deps.Store.GetMessageContext(ctx, p.MessageID, p.Before, p.After)
	if errors.Is(err, store.ErrNotFound) {
		return nil, &Error{Code: CodeNotFound, Message: "message not found: " + p.MessageID}
	}
	if err != nil {
		return nil, internalErr(err)
	}
	return s.deps.Store.EnrichMessages(ctx, msgs), nil
}

type contactsSearchParams struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

func (s *Server) handleContactsSearch(ctx context.Context, params json.RawMessage) (any, *Error) {
	p, perr := decode[contactsSearchParams](params)
	if perr != nil {
		return nil, perr
	}
	contacts, err := s.deps.Store.SearchContacts(ctx, p.Query, p.Limit)
	if err != nil {
		return nil, internalErr(err)
	}
	return contacts, nil
}

type contactsShowParams struct {
	ID string `json:"id"` // participant_id or phone number
}

func (s *Server) handleContactsShow(ctx context.Context, params json.RawMessage) (any, *Error) {
	p, perr := decode[contactsShowParams](params)
	if perr != nil {
		return nil, perr
	}
	if p.ID == "" {
		return nil, &Error{Code: CodeInvalidParams, Message: "id is required"}
	}
	contact, err := s.deps.Store.GetContact(ctx, p.ID)
	if errors.Is(err, store.ErrNotFound) {
		contact, err = s.deps.Store.GetContactByNumber(ctx, p.ID)
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil, &Error{Code: CodeNotFound, Message: "contact not found: " + p.ID}
	}
	if err != nil {
		return nil, internalErr(err)
	}
	return contact, nil
}

// handleSyncRefresh pulls a light snapshot from the phone: the most recent
// inbox conversations plus their latest messages. Clients call it on
// connect so the archive catches up on anything the long-poll missed while
// no daemon (or an idle one) was running. Rate-limited; runs in the
// background and announces completion with a sync.status "refreshed" event.
func (s *Server) handleSyncRefresh(ctx context.Context) (any, *Error) {
	if s.deps.Client == nil || s.deps.Pump == nil {
		return nil, &Error{Code: CodeUnavailable, Message: "offline daemon: no phone connection to refresh from"}
	}
	s.refreshMu.Lock()
	if time.Since(s.lastRefresh) < 30*time.Second {
		s.refreshMu.Unlock()
		return map[string]any{"started": false, "reason": "refreshed recently"}, nil
	}
	s.lastRefresh = time.Now()
	s.refreshMu.Unlock()

	go s.runRefresh()
	return map[string]any{"started": true}, nil
}

// refreshConversations bounds one sync.refresh pass: how many recent inbox
// conversations to re-pull, and for how many of those to fetch messages.
const (
	refreshConversations = 50
	refreshMessageConvs  = 15
	refreshMessagesEach  = 20
)

func (s *Server) runRefresh() {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	ctx := context.Background()

	resp, err := s.deps.Client.Underlying().ListConversations(refreshConversations, gmproto.ListConversationsRequest_INBOX)
	if err != nil {
		s.deps.Logger.Warn().Err(err).Msg("sync.refresh: list conversations failed")
		s.Broadcast(EventSyncStatus, map[string]any{"state": "refresh_failed"})
		return
	}
	fetched := 0
	for _, conv := range resp.GetConversations() {
		if conv == nil || conv.GetConversationID() == "" {
			continue
		}
		s.deps.Pump.Handle(conv)
		if fetched < refreshMessageConvs {
			if history, err := s.deps.Client.Underlying().FetchMessages(conv.GetConversationID(), refreshMessagesEach, nil); err == nil {
				s.deps.Pump.ImportMessages(ctx, history.GetMessages())
			}
			fetched++
		}
	}
	_ = s.deps.Store.MarkSync(ctx, time.Time{}, time.Now())
	// Direct pump writes bypass the gm-event broadcaster, so tell clients
	// to refetch wholesale.
	s.Broadcast(EventSyncStatus, map[string]any{"state": "refreshed"})
}

type historyBackfillParams struct {
	ConversationID string `json:"conversation_id"`
	Requests       int    `json:"requests"`
	Count          int64  `json:"count"`
}

func (s *Server) handleHistoryBackfill(ctx context.Context, params json.RawMessage) (any, *Error) {
	p, perr := decode[historyBackfillParams](params)
	if perr != nil {
		return nil, perr
	}
	if p.ConversationID == "" {
		return nil, &Error{Code: CodeInvalidParams, Message: "conversation_id is required"}
	}
	if s.deps.Client == nil || s.deps.Pump == nil {
		return nil, &Error{Code: CodeUnavailable, Message: "phone connection unavailable"}
	}
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	// Backfills page through many requests; give them more room than one send.
	ctx, cancel := context.WithTimeout(ctx, 5*s.deps.LiveTimeout)
	defer cancel()
	res, err := history.Backfill(ctx, s.deps.Store, s.deps.Client, s.deps.Pump, p.ConversationID, p.Requests, p.Count)
	if err != nil {
		return nil, &Error{Code: CodeUnavailable, Message: err.Error()}
	}
	return res, nil
}

type historyLookupParams struct {
	Phone    string `json:"phone"`
	Requests int    `json:"requests"`
	Count    int64  `json:"count"`
}

func (s *Server) handleHistoryLookup(ctx context.Context, params json.RawMessage) (any, *Error) {
	p, perr := decode[historyLookupParams](params)
	if perr != nil {
		return nil, perr
	}
	if p.Phone == "" {
		return nil, &Error{Code: CodeInvalidParams, Message: "phone is required (E.164, e.g. +13855551234)"}
	}
	if s.deps.Client == nil || s.deps.Pump == nil {
		return nil, &Error{Code: CodeUnavailable, Message: "phone connection unavailable"}
	}
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 5*s.deps.LiveTimeout)
	defer cancel()
	conv, err := history.LookupConversation(s.deps.Client, s.deps.Pump, p.Phone)
	if err != nil {
		return nil, &Error{Code: CodeUnavailable, Message: err.Error()}
	}
	res, err := history.Backfill(ctx, s.deps.Store, s.deps.Client, s.deps.Pump, conv.GetConversationID(), p.Requests, p.Count)
	if err != nil {
		return nil, &Error{Code: CodeUnavailable, Message: err.Error()}
	}
	return map[string]any{
		"conversation_id": conv.GetConversationID(),
		"name":            conv.GetName(),
		"backfill":        res,
	}, nil
}

type sendTextParams struct {
	ConversationID string `json:"conversation_id"`
	Body           string `json:"body"`
	ReplyToID      string `json:"reply_to_id"`
	RequestedBy    string `json:"requested_by"`
}

func (s *Server) handleSendText(ctx context.Context, params json.RawMessage) (any, *Error) {
	p, perr := decode[sendTextParams](params)
	if perr != nil {
		return nil, perr
	}
	if p.ConversationID == "" || p.Body == "" {
		return nil, &Error{Code: CodeInvalidParams, Message: "conversation_id and body are required"}
	}
	if s.deps.SendMode == SendOff {
		return nil, &Error{
			Code:    CodeSendsDisabled,
			Message: "daemon is read-only; restart with `gmcli --read-only=false serve` to enable the approval queue",
		}
	}
	requestedBy := p.RequestedBy
	if requestedBy == "" {
		requestedBy = "rpc"
	}
	approval := store.Approval{
		ID:             uuid.NewString(),
		ConversationID: p.ConversationID,
		Body:           p.Body,
		RequestedBy:    requestedBy,
	}
	if p.ReplyToID != "" {
		r := p.ReplyToID
		approval.ReplyToID = &r
	}
	if err := s.deps.Store.CreateApproval(ctx, approval); err != nil {
		return nil, internalErr(err)
	}
	created, err := s.deps.Store.GetApproval(ctx, approval.ID)
	if err != nil {
		return nil, internalErr(err)
	}

	if s.deps.SendMode == SendApprove {
		s.Broadcast(EventApprovalRequested, created)
		return created, nil
	}

	// SendDirect: perform the send now; the approval row becomes an audit
	// record with a terminal status.
	resolved, rpcErr := s.performApprovedSend(ctx, created)
	if rpcErr != nil {
		return nil, rpcErr
	}
	return resolved, nil
}

type approvalsListParams struct {
	Status string `json:"status"`
	Limit  int    `json:"limit"`
}

func (s *Server) handleApprovalsList(ctx context.Context, params json.RawMessage) (any, *Error) {
	p, perr := decode[approvalsListParams](params)
	if perr != nil {
		return nil, perr
	}
	approvals, err := s.deps.Store.ListApprovals(ctx, p.Status, p.Limit)
	if err != nil {
		return nil, internalErr(err)
	}
	return approvals, nil
}

type approvalResolveParams struct {
	ApprovalID string `json:"approval_id"`
	Reason     string `json:"reason"`
}

func (s *Server) handleApprovalsApprove(ctx context.Context, params json.RawMessage) (any, *Error) {
	p, perr := decode[approvalResolveParams](params)
	if perr != nil {
		return nil, perr
	}
	if p.ApprovalID == "" {
		return nil, &Error{Code: CodeInvalidParams, Message: "approval_id is required"}
	}
	if s.deps.SendMode == SendOff {
		return nil, &Error{
			Code:    CodeSendsDisabled,
			Message: "daemon is read-only; restart with `gmcli --read-only=false serve` to enable sends",
		}
	}
	approval, err := s.deps.Store.GetApproval(ctx, p.ApprovalID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, &Error{Code: CodeNotFound, Message: "approval not found: " + p.ApprovalID}
	}
	if err != nil {
		return nil, internalErr(err)
	}
	if approval.Status != store.ApprovalPending {
		return nil, &Error{Code: CodeAlreadyResolved, Message: "approval is already " + approval.Status}
	}
	return s.performApprovedSend(ctx, approval)
}

// performApprovedSend sends one approved message and records the outcome.
// liveMu makes double-approve races lose cleanly: the second caller finds the
// row no longer pending.
func (s *Server) performApprovedSend(ctx context.Context, approval store.Approval) (any, *Error) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()

	current, err := s.deps.Store.GetApproval(ctx, approval.ID)
	if err != nil {
		return nil, internalErr(err)
	}
	if current.Status != store.ApprovalPending {
		return nil, &Error{Code: CodeAlreadyResolved, Message: "approval is already " + current.Status}
	}
	if s.deps.Client == nil {
		return nil, &Error{Code: CodeUnavailable, Message: "phone connection unavailable"}
	}

	sendCtx, cancel := context.WithTimeout(ctx, s.deps.LiveTimeout)
	defer cancel()
	replyTo := ""
	if approval.ReplyToID != nil {
		replyTo = *approval.ReplyToID
	}
	res, sendErr := s.deps.Client.SendText(sendCtx, approval.ConversationID, approval.Body, replyTo)

	status := store.ApprovalSent
	var errMsg, messageID *string
	if sendErr != nil {
		status = store.ApprovalFailed
		m := sendErr.Error()
		errMsg = &m
	} else {
		messageID = &res.MessageID
	}
	if err := s.deps.Store.ResolveApproval(ctx, approval.ID, status, errMsg, messageID); err != nil {
		return nil, internalErr(err)
	}
	resolved, err := s.deps.Store.GetApproval(ctx, approval.ID)
	if err != nil {
		return nil, internalErr(err)
	}
	s.Broadcast(EventApprovalResolved, resolved)
	if sendErr != nil {
		return nil, &Error{Code: CodeUnavailable, Message: "send failed: " + sendErr.Error(), Data: resolved}
	}
	return resolved, nil
}

func (s *Server) handleApprovalsDeny(ctx context.Context, params json.RawMessage) (any, *Error) {
	p, perr := decode[approvalResolveParams](params)
	if perr != nil {
		return nil, perr
	}
	if p.ApprovalID == "" {
		return nil, &Error{Code: CodeInvalidParams, Message: "approval_id is required"}
	}
	var reason *string
	if p.Reason != "" {
		reason = &p.Reason
	}
	err := s.deps.Store.ResolveApproval(ctx, p.ApprovalID, store.ApprovalDenied, reason, nil)
	if errors.Is(err, store.ErrNotFound) {
		return nil, &Error{Code: CodeNotFound, Message: "approval not found: " + p.ApprovalID}
	}
	if errors.Is(err, store.ErrApprovalResolved) {
		return nil, &Error{Code: CodeAlreadyResolved, Message: "approval already resolved"}
	}
	if err != nil {
		return nil, internalErr(err)
	}
	resolved, err := s.deps.Store.GetApproval(ctx, p.ApprovalID)
	if err != nil {
		return nil, internalErr(err)
	}
	s.Broadcast(EventApprovalResolved, resolved)
	return resolved, nil
}
