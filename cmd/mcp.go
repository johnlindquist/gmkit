package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/fdsouvenir/gmcli/internal/daemonctl"
	"github.com/fdsouvenir/gmcli/internal/history"
	"github.com/fdsouvenir/gmcli/internal/paths"
	"github.com/fdsouvenir/gmcli/internal/rpc"
	"github.com/fdsouvenir/gmcli/internal/store"
)

// mcpLiveTimeout bounds one daemon-proxied call (send, backfill).
const mcpLiveTimeout = 5 * time.Minute

// mcpCmd runs an MCP (Model Context Protocol) server over stdio. Read tools
// query the SQLite archive directly, so they work even when the daemon is
// down. Phone-touching tools (send_text, backfill) proxy to a running
// `gmcli serve` socket — the daemon owns the single libgm session, and its
// send policy (approval queue by default) is enforced there, not here.
func mcpCmd() *cobra.Command {
	var noAutostart bool
	c := &cobra.Command{
		Use:   "mcp",
		Short: "Run an MCP server over stdio for AI agent integrations",
		Long: "Expose the local Google Messages archive to MCP clients (Claude, " +
			"editors, agent runtimes) over stdio. Read tools work standalone; " +
			"send and backfill tools require a running `gmcli serve`, which " +
			"enforces the send policy. With the default policy, send_text only " +
			"queues a proposal that a human approves in gmtui or via " +
			"`gmcli approvals approve`.",
		RunE: func(cmd *cobra.Command, args []string) error {
			layout, err := resolveLayout()
			if err != nil {
				return err
			}
			ctx, cancel := signalContext(context.Background())
			defer cancel()

			st, err := store.Open(ctx, layout.Database)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer st.Close()

			if !noAutostart {
				// Best-effort: bring the daemon up so the archive syncs and
				// send/backfill tools work out of the box. Reads work
				// regardless, so a failure here is a warning, not fatal.
				// stderr is safe — MCP stdio protocol owns stdout only.
				if err := daemonctl.EnsureRunning(ctx, layout, daemonctl.Options{LogLevel: "warn"}); err != nil {
					fmt.Fprintf(os.Stderr, "gmcli mcp: daemon autostart failed (read tools still work): %v\n", err)
				}
			}

			srv := newMCPServer(st, layout)
			return srv.Run(ctx, &mcp.StdioTransport{})
		},
	}
	c.Flags().BoolVar(&noAutostart, "no-autostart", false, "do not auto-start the gmcli daemon")
	return c
}

// dialServe connects to the daemon socket for live operations,
// auto-starting an on-demand daemon when none is running.
func dialServe(layout paths.Layout) (*rpc.Client, error) {
	if err := daemonctl.EnsureRunning(context.Background(), layout, daemonctl.Options{LogLevel: "warn"}); err != nil {
		return nil, fmt.Errorf("gmcli daemon is not running and could not be started (%w); start it manually with `gmcli serve`", err)
	}
	client, err := rpc.Dial(layout.Socket)
	if err != nil {
		return nil, fmt.Errorf("connect to gmcli daemon: %w", err)
	}
	return client, nil
}

func newMCPServer(st *store.Store, layout paths.Layout) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "gmcli",
		Title:   "Google Messages Local Archive",
		Version: Version,
	}, nil)

	type chatSummary struct {
		ConversationID string   `json:"conversation_id"`
		DisplayName    string   `json:"display_name"`
		IsGroup        bool     `json:"is_group"`
		Participants   []string `json:"participants,omitempty"`
		Unread         bool     `json:"unread"`
		Pinned         bool     `json:"pinned"`
		LastMessageISO string   `json:"last_message_iso,omitempty"`
		LastMessageMS  int64    `json:"last_message_ms"`
	}
	summarize := func(convs []store.Conversation) []chatSummary {
		out := make([]chatSummary, 0, len(convs))
		for _, c := range convs {
			s := chatSummary{
				ConversationID: c.ID,
				DisplayName:    c.DisplayName(),
				IsGroup:        c.IsGroup,
				Participants:   c.ParticipantNames(),
				Unread:         c.Unread,
				Pinned:         c.Pinned,
				LastMessageMS:  c.LastMessageTimeMS,
			}
			if c.LastMessageTimeMS > 0 {
				s.LastMessageISO = time.UnixMilli(c.LastMessageTimeMS).Local().Format(time.RFC3339)
			}
			out = append(out, s)
		}
		return out
	}

	type listChatsIn struct {
		Limit      int  `json:"limit,omitempty"`
		UnreadOnly bool `json:"unread_only,omitempty"`
	}
	type chatsOut struct {
		Chats []chatSummary `json:"chats"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name: "list_chats",
		Description: "List conversations from the local Google Messages archive, most recent " +
			"first, with human-readable display names. Returns conversation_id values used by " +
			"other tools. limit defaults to 25. To find a specific person's conversation, " +
			"prefer find_chats.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in listChatsIn) (*mcp.CallToolResult, chatsOut, error) {
		limit := in.Limit
		if limit <= 0 {
			limit = 25
		}
		chats, err := st.ListConversations(ctx, store.ListConversationOpts{Limit: limit, UnreadOnly: in.UnreadOnly})
		if err != nil {
			return nil, chatsOut{}, err
		}
		return nil, chatsOut{Chats: summarize(chats)}, nil
	})

	type findChatsIn struct {
		Query string `json:"query"`
		Limit int    `json:"limit,omitempty"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name: "find_chats",
		Description: "Resolve a person, group name, or phone number to conversations — the " +
			"fastest way to answer questions like \"what did Alice say\". Matches conversation " +
			"names, contact names, local aliases, and phone numbers (substring, case-insensitive " +
			"for ASCII). Returns candidates newest-activity first; then use list_messages or " +
			"search_messages scoped by conversation_id.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in findChatsIn) (*mcp.CallToolResult, chatsOut, error) {
		if in.Query == "" {
			return nil, chatsOut{}, errors.New("query is required (a name, alias, or phone number fragment)")
		}
		chats, err := st.FindConversations(ctx, in.Query, in.Limit)
		if err != nil {
			return nil, chatsOut{}, err
		}
		return nil, chatsOut{Chats: summarize(chats)}, nil
	})

	type searchMessagesIn struct {
		Query          string `json:"query"`
		ConversationID string `json:"conversation_id,omitempty"`
		Since          string `json:"since,omitempty"`
		Until          string `json:"until,omitempty"`
		Limit          int    `json:"limit,omitempty"`
	}
	type searchMessagesOut struct {
		Hits []store.RichHit `json:"hits"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name: "search_messages",
		Description: "Full-text search across archived message bodies. Natural-language queries " +
			"work — the query is tried as FTS5 syntax (quoted phrases, AND/OR/NOT) and falls back " +
			"to a literal term search automatically, so punctuation is safe. Terms need 3+ " +
			"characters to match (trigram index). Results include sender and conversation names " +
			"plus ISO timestamps. Optional: conversation_id to scope to one chat; since/until as " +
			"ISO dates (2026-07-01) or relative durations (24h, 7d, 3w, 6mo). Only searches synced " +
			"messages — use backfill_history to pull older history first. Follow up with " +
			"get_message_context on interesting hits.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in searchMessagesIn) (*mcp.CallToolResult, searchMessagesOut, error) {
		if in.Query == "" {
			return nil, searchMessagesOut{}, errors.New("query is required")
		}
		since, err := parseTimeArg(in.Since)
		if err != nil {
			return nil, searchMessagesOut{}, fmt.Errorf("since: %w", err)
		}
		until, err := parseTimeArg(in.Until)
		if err != nil {
			return nil, searchMessagesOut{}, fmt.Errorf("until: %w", err)
		}
		hits, err := st.SearchMessagesRich(ctx, store.SearchOpts{
			Query:          in.Query,
			ConversationID: in.ConversationID,
			Since:          since,
			Until:          until,
			Limit:          in.Limit,
		})
		if err != nil {
			return nil, searchMessagesOut{}, err
		}
		return nil, searchMessagesOut{Hits: hits}, nil
	})

	type listMessagesIn struct {
		ConversationID string `json:"conversation_id"`
		Since          string `json:"since,omitempty"`
		Until          string `json:"until,omitempty"`
		Limit          int    `json:"limit,omitempty"`
		Order          string `json:"order,omitempty"`
	}
	type messagesOut struct {
		ConversationName string              `json:"conversation_name,omitempty"`
		Messages         []store.RichMessage `json:"messages"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name: "list_messages",
		Description: "List archived messages for one conversation_id with resolved sender names " +
			"and ISO timestamps, newest first by default (order: asc|desc). Optional since/until " +
			"as ISO dates or relative durations (24h, 7d). limit defaults to 50.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in listMessagesIn) (*mcp.CallToolResult, messagesOut, error) {
		if in.ConversationID == "" {
			return nil, messagesOut{}, errors.New("conversation_id is required (find one with find_chats)")
		}
		since, err := parseTimeArg(in.Since)
		if err != nil {
			return nil, messagesOut{}, fmt.Errorf("since: %w", err)
		}
		until, err := parseTimeArg(in.Until)
		if err != nil {
			return nil, messagesOut{}, fmt.Errorf("until: %w", err)
		}
		msgs, err := st.ListMessages(ctx, store.ListMessageOpts{
			ConversationID: in.ConversationID,
			Since:          since,
			Until:          until,
			Limit:          in.Limit,
			Order:          in.Order,
		})
		if err != nil {
			return nil, messagesOut{}, err
		}
		out := messagesOut{Messages: st.EnrichMessages(ctx, msgs)}
		if conv, err := st.GetConversation(ctx, in.ConversationID); err == nil {
			out.ConversationName = conv.DisplayName()
		}
		return nil, out, nil
	})

	type messageContextIn struct {
		MessageID string `json:"message_id"`
		Before    int    `json:"before,omitempty"`
		After     int    `json:"after,omitempty"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_message_context",
		Description: "Fetch the messages surrounding one message_id in its conversation " +
			"(default 5 before and 5 after), oldest to newest, with resolved sender names. " +
			"Use after search_messages to read a hit in context before answering.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in messageContextIn) (*mcp.CallToolResult, messagesOut, error) {
		if in.MessageID == "" {
			return nil, messagesOut{}, errors.New("message_id is required")
		}
		before, after := in.Before, in.After
		if before == 0 && after == 0 {
			before, after = 5, 5
		}
		msgs, err := st.GetMessageContext(ctx, in.MessageID, before, after)
		if err != nil {
			return nil, messagesOut{}, err
		}
		out := messagesOut{Messages: st.EnrichMessages(ctx, msgs)}
		if len(msgs) > 0 {
			if conv, err := st.GetConversation(ctx, msgs[0].ConversationID); err == nil {
				out.ConversationName = conv.DisplayName()
			}
		}
		return nil, out, nil
	})

	type searchContactsIn struct {
		Query string `json:"query"`
		Limit int    `json:"limit,omitempty"`
	}
	type searchContactsOut struct {
		Contacts []store.Contact `json:"contacts"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name: "search_contacts",
		Description: "Search contacts by name, local alias, or phone number (substring match). " +
			"Use to resolve a person to participant_id values and find their conversations.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in searchContactsIn) (*mcp.CallToolResult, searchContactsOut, error) {
		contacts, err := st.SearchContacts(ctx, in.Query, in.Limit)
		if err != nil {
			return nil, searchContactsOut{}, err
		}
		return nil, searchContactsOut{Contacts: contacts}, nil
	})

	type backfillIn struct {
		ConversationID string `json:"conversation_id,omitempty"`
		Phone          string `json:"phone,omitempty"`
		Requests       int    `json:"requests,omitempty"`
		Count          int64  `json:"count,omitempty"`
	}
	type backfillOut struct {
		ConversationID string                 `json:"conversation_id"`
		Name           string                 `json:"name,omitempty"`
		Backfill       history.BackfillResult `json:"backfill"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name: "backfill_history",
		Description: "Fetch older messages through the paired phone into the local archive. " +
			"Provide conversation_id for a known chat, or phone (E.164, e.g. +13855551234) to look " +
			"a conversation up by number first. Best-effort: Google may return partial history. " +
			"Requires a running `gmcli serve` and an online phone.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in backfillIn) (*mcp.CallToolResult, backfillOut, error) {
		if in.ConversationID == "" && in.Phone == "" {
			return nil, backfillOut{}, errors.New("conversation_id or phone is required")
		}
		client, err := dialServe(layout)
		if err != nil {
			return nil, backfillOut{}, err
		}
		defer client.Close()
		ctx, cancel := context.WithTimeout(ctx, mcpLiveTimeout)
		defer cancel()

		if in.Phone != "" {
			var res struct {
				ConversationID string                 `json:"conversation_id"`
				Name           string                 `json:"name"`
				Backfill       history.BackfillResult `json:"backfill"`
			}
			if err := client.Call(ctx, "history.lookup", map[string]any{
				"phone": in.Phone, "requests": in.Requests, "count": in.Count,
			}, &res); err != nil {
				return nil, backfillOut{}, err
			}
			return nil, backfillOut(res), nil
		}
		var res history.BackfillResult
		if err := client.Call(ctx, "history.backfill", map[string]any{
			"conversation_id": in.ConversationID, "requests": in.Requests, "count": in.Count,
		}, &res); err != nil {
			return nil, backfillOut{}, err
		}
		return nil, backfillOut{ConversationID: in.ConversationID, Backfill: res}, nil
	})

	type sendTextIn struct {
		ConversationID string `json:"conversation_id"`
		Body           string `json:"body"`
		ReplyToID      string `json:"reply_to_id,omitempty"`
	}
	type sendTextOut struct {
		Approval store.Approval `json:"approval"`
		Note     string         `json:"note"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name: "send_text",
		Description: "Propose sending a text message to a conversation_id. Under the default " +
			"daemon policy this does NOT send immediately: it queues an approval that a human " +
			"reviews in gmtui or with `gmcli approvals approve`. Report the returned status to the " +
			"user honestly — 'pending' means queued for human approval, not sent. Only call this " +
			"when the user explicitly asked to send a message.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in sendTextIn) (*mcp.CallToolResult, sendTextOut, error) {
		if in.ConversationID == "" || in.Body == "" {
			return nil, sendTextOut{}, errors.New("conversation_id and body are required")
		}
		client, err := dialServe(layout)
		if err != nil {
			return nil, sendTextOut{}, err
		}
		defer client.Close()
		ctx, cancel := context.WithTimeout(ctx, mcpLiveTimeout)
		defer cancel()

		var approval store.Approval
		err = client.Call(ctx, "send.text", map[string]any{
			"conversation_id": in.ConversationID,
			"body":            in.Body,
			"reply_to_id":     in.ReplyToID,
			"requested_by":    "mcp",
		}, &approval)
		if err != nil {
			return nil, sendTextOut{}, err
		}
		note := "Sent."
		if approval.Status == store.ApprovalPending {
			note = "Queued for human approval. The message has NOT been sent yet; a human must approve it in gmtui or with `gmcli approvals approve`."
		}
		return nil, sendTextOut{Approval: approval, Note: note}, nil
	})

	type statusOut struct {
		DaemonRunning    bool   `json:"daemon_running"`
		Connected        bool   `json:"connected"`
		SendMode         string `json:"send_mode,omitempty"`
		PendingApprovals int    `json:"pending_approvals"`
		Conversations    int    `json:"conversations"`
		Messages         int    `json:"messages"`
		LastEventMS      int64  `json:"last_event_ms"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_status",
		Description: "Report archive freshness and daemon state: whether `gmcli serve` is running, " +
			"phone connectivity, send policy, pending approvals, and counts. Call this first when " +
			"results seem stale or sends fail.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, statusOut, error) {
		out := statusOut{}
		if client, err := rpc.Dial(layout.Socket); err == nil {
			defer client.Close()
			var res struct {
				Connected        bool   `json:"connected"`
				SendMode         string `json:"send_mode"`
				PendingApprovals int    `json:"pending_approvals"`
				Conversations    int    `json:"conversations"`
				Messages         int    `json:"messages"`
				LastEventMS      int64  `json:"last_event_ms"`
			}
			callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if err := client.Call(callCtx, "status", nil, &res); err == nil {
				return nil, statusOut{
					DaemonRunning:    true,
					Connected:        res.Connected,
					SendMode:         res.SendMode,
					PendingApprovals: res.PendingApprovals,
					Conversations:    res.Conversations,
					Messages:         res.Messages,
					LastEventMS:      res.LastEventMS,
				}, nil
			}
		}
		// Daemon down: report what the store alone knows.
		sync, err := st.SyncState(ctx)
		if err != nil {
			return nil, out, err
		}
		out.Conversations, _ = st.CountConversations(ctx)
		out.Messages, _ = st.CountMessages(ctx)
		pending, _ := st.ListApprovals(ctx, store.ApprovalPending, 1000)
		out.PendingApprovals = len(pending)
		out.LastEventMS = sync.LastEventTime.UnixMilli()
		return nil, out, nil
	})

	return srv
}

// parseTimeArg accepts "" (zero time), an ISO date (2026-07-01), an RFC3339
// datetime, or a relative duration back from now: 30m, 24h, 7d, 3w, 6mo, 1y.
func parseTimeArg(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if d, err := parseRelativeDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("unrecognized time %q (use 2026-07-01, RFC3339, or a relative duration like 24h / 7d / 3w / 6mo)", s)
}

// parseRelativeDuration extends time.ParseDuration with d (days), w (weeks),
// mo (months, 30d) and y (years, 365d) suffixes.
func parseRelativeDuration(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	var n float64
	var unit string
	if _, err := fmt.Sscanf(s, "%f%s", &n, &unit); err != nil {
		return 0, fmt.Errorf("not a duration: %q", s)
	}
	day := 24 * time.Hour
	switch unit {
	case "d":
		return time.Duration(n * float64(day)), nil
	case "w":
		return time.Duration(n * float64(7*day)), nil
	case "mo":
		return time.Duration(n * float64(30*day)), nil
	case "y":
		return time.Duration(n * float64(365*day)), nil
	}
	return 0, fmt.Errorf("unknown duration unit %q", unit)
}
