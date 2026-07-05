// Package history implements best-effort message backfill through the paired
// phone. It is shared by the CLI (`gmcli history`) and the RPC daemon
// (`gmcli serve`), which is why it lives outside cmd.
package history

import (
	"context"
	"fmt"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/johnlindquist/gmkit/internal/gm"
	"github.com/johnlindquist/gmkit/internal/store"
	gmsync "github.com/johnlindquist/gmkit/internal/sync"
)

// BackfillResult reports what a backfill run accomplished. Protocol records
// processed are counted separately from messages added to the target chat —
// Google's FetchMessages can return records for other conversations.
type BackfillResult struct {
	ConversationID       string `json:"conversation_id"`
	Requests             int    `json:"requests"`
	Count                int64  `json:"count"`
	FetchedMessages      int    `json:"fetched_messages"`
	SyncRecordsProcessed int    `json:"sync_records_processed"`
	MessagesBefore       int    `json:"messages_before"`
	MessagesAfter        int    `json:"messages_after"`
	MessagesAddedForChat int    `json:"messages_added_for_chat"`
}

// Backfill pages FetchMessages backwards from the oldest locally-stored
// message in chat, importing records through pump. requests bounds the number
// of FetchMessages calls; count bounds records per call.
func Backfill(ctx context.Context, st *store.Store, client *gm.Client, pump *gmsync.Pump, chat string, requests int, count int64) (BackfillResult, error) {
	if requests <= 0 {
		requests = 10
	}
	if count <= 0 {
		count = 50
	}
	cursor, err := oldestCursor(ctx, st, chat)
	if err != nil {
		return BackfillResult{}, err
	}

	before, err := st.CountMessagesForConversation(ctx, chat)
	if err != nil {
		return BackfillResult{}, fmt.Errorf("count messages before backfill: %w", err)
	}

	res := BackfillResult{ConversationID: chat, Count: count, MessagesBefore: before}
	for i := 0; i < requests; i++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		resp, err := client.Underlying().FetchMessages(chat, count, cursor)
		if err != nil {
			return res, fmt.Errorf("fetch messages: %w", err)
		}
		res.Requests++
		msgs := resp.GetMessages()
		res.FetchedMessages += len(msgs)
		imported := pump.ImportMessages(ctx, msgs)
		res.SyncRecordsProcessed += imported
		next := resp.GetCursor()
		if len(msgs) == 0 || sameCursor(cursor, next) {
			break
		}
		cursor = next
	}
	after, err := st.CountMessagesForConversation(ctx, chat)
	if err != nil {
		return res, fmt.Errorf("count messages after backfill: %w", err)
	}
	res.MessagesAfter = after
	res.MessagesAddedForChat = after - before
	return res, nil
}

// LookupConversation asks Google Messages for the conversation associated
// with an E.164 phone number and imports it through pump. Returns the proto
// so callers can read the conversation ID and name.
func LookupConversation(client *gm.Client, pump *gmsync.Pump, phone string) (*gmproto.Conversation, error) {
	resp, err := client.Underlying().GetOrCreateConversation(&gmproto.GetOrCreateConversationRequest{
		Numbers: []*gmproto.ContactNumber{
			{MysteriousInt: 7, Number: phone, Number2: phone},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("GetOrCreateConversation: %w", err)
	}
	conv := resp.GetConversation()
	if conv == nil {
		return nil, fmt.Errorf("no conversation found for %s", phone)
	}
	pump.Handle(conv)
	return conv, nil
}

func oldestCursor(ctx context.Context, st *store.Store, chat string) (*gmproto.Cursor, error) {
	msgs, err := st.ListMessages(ctx, store.ListMessageOpts{
		ConversationID: chat,
		Limit:          1,
		Order:          "asc",
	})
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, nil
	}
	return &gmproto.Cursor{
		LastItemID:        msgs[0].ID,
		LastItemTimestamp: msgs[0].TimestampMS,
	}, nil
}

func sameCursor(a, b *gmproto.Cursor) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.GetLastItemID() == b.GetLastItemID() &&
		a.GetLastItemTimestamp() == b.GetLastItemTimestamp()
}
