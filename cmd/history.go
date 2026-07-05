package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/fdsouvenir/gmcli/internal/gm"
	"github.com/fdsouvenir/gmcli/internal/history"
	"github.com/fdsouvenir/gmcli/internal/output"
	"github.com/fdsouvenir/gmcli/internal/store"
	gmsync "github.com/fdsouvenir/gmcli/internal/sync"
)

func historyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "history",
		Short: "Best-effort message history backfill",
		Long: "Fetch older messages for a conversation through the paired phone. " +
			"Like wacli, this is best-effort: Google may return partial history, " +
			"and the phone must be online.",
	}
	c.AddCommand(historyBackfillCmd())
	c.AddCommand(historyLookupCmd())
	return c
}

func historyBackfillCmd() *cobra.Command {
	var chat string
	var requests int
	var count int64
	c := &cobra.Command{
		Use:   "backfill",
		Short: "Fetch older messages for one conversation",
		Long: "Fetch older messages for one conversation. --requests limits how many " +
			"FetchMessages calls gmcli makes, and --count limits how many message " +
			"records each call asks the phone for. JSON output separates protocol " +
			"records processed from messages added to the target conversation.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if chat == "" {
				return fmt.Errorf("--chat is required")
			}
			res, err := runHistoryBackfill(chat, requests, count)
			if err != nil {
				return err
			}
			return printBackfillResult(res)
		},
	}
	c.Flags().StringVar(&chat, "chat", "", "conversation_id to backfill")
	c.Flags().IntVar(&requests, "requests", 10, "max FetchMessages calls to make for the target conversation")
	c.Flags().Int64Var(&count, "count", 50, "max message records to request per FetchMessages call")
	return c
}

func historyLookupCmd() *cobra.Command {
	var phone string
	var requests int
	var count int64
	c := &cobra.Command{
		Use:   "lookup",
		Short: "Find a conversation by phone number and backfill its messages",
		RunE: func(cmd *cobra.Command, args []string) error {
			if phone == "" {
				return fmt.Errorf("--phone is required")
			}
			layout, err := resolveLayout()
			if err != nil {
				return err
			}
			logger := newLogger()
			ctx, cancel := signalContext(context.Background())
			defer cancel()

			st, err := store.Open(ctx, layout.Database)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer st.Close()

			client, err := gm.Open(layout, logger)
			if err != nil {
				return err
			}
			pump := gmsync.New(st, logger)
			client.Subscribe(pump.Handle)
			if err := client.Connect(); err != nil {
				return fmt.Errorf("connect: %w", err)
			}
			defer client.Disconnect()

			conv, err := history.LookupConversation(client, pump, phone)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Found conversation: %s (ID: %s)\n", conv.GetName(), conv.GetConversationID())

			res, err := history.Backfill(ctx, st, client, pump, conv.GetConversationID(), requests, count)
			if err != nil {
				return err
			}
			return printBackfillResult(res)
		},
	}
	c.Flags().StringVar(&phone, "phone", "", "phone number in E.164 format (e.g. +13855551234)")
	c.Flags().IntVar(&requests, "requests", 20, "max FetchMessages calls")
	c.Flags().Int64Var(&count, "count", 50, "max message records per call")
	return c
}

func printBackfillResult(res history.BackfillResult) error {
	if flags.jsonOut {
		return output.JSON(os.Stdout, res)
	}
	fmt.Fprintf(os.Stderr, "Backfill for %s: fetched %d message record(s), chat messages %d -> %d (+%d), using %d request(s)\n",
		res.ConversationID, res.FetchedMessages, res.MessagesBefore, res.MessagesAfter, res.MessagesAddedForChat, res.Requests)
	return nil
}

func runHistoryBackfill(chat string, requests int, count int64) (history.BackfillResult, error) {
	layout, err := resolveLayout()
	if err != nil {
		return history.BackfillResult{}, err
	}
	logger := newLogger()
	ctx, cancel := signalContext(context.Background())
	defer cancel()

	st, err := store.Open(ctx, layout.Database)
	if err != nil {
		return history.BackfillResult{}, fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	client, err := gm.Open(layout, logger)
	if err != nil {
		return history.BackfillResult{}, err
	}

	pump := gmsync.New(st, logger)
	client.Subscribe(pump.Handle)

	if err := client.Connect(); err != nil {
		return history.BackfillResult{}, fmt.Errorf("connect: %w", err)
	}
	defer client.Disconnect()

	if conv, err := client.Underlying().GetConversation(chat); err == nil && conv != nil {
		pump.Handle(conv)
	} else if _, localErr := st.GetConversation(ctx, chat); localErr != nil {
		if err != nil {
			return history.BackfillResult{}, fmt.Errorf("get conversation %s: %w", chat, err)
		}
		return history.BackfillResult{}, fmt.Errorf("conversation %s is not in the local store; run `gmcli sync` first", chat)
	}

	return history.Backfill(ctx, st, client, pump, chat, requests, count)
}
