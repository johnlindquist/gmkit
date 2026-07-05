package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/fdsouvenir/gmcli/internal/daemonctl"
	"github.com/fdsouvenir/gmcli/internal/output"
	"github.com/fdsouvenir/gmcli/internal/rpc"
	"github.com/fdsouvenir/gmcli/internal/store"
)

// approvalCallTimeout bounds one approvals RPC. Approve performs a live send
// through the phone, so it gets the same headroom the daemon allows itself.
const approvalCallTimeout = 90 * time.Second

func approvalsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "approvals",
		Short: "Review queued outgoing messages (requires a running `gmcli serve`)",
		Long: "Agents and RPC clients propose outgoing messages into an approval " +
			"queue instead of sending directly. These commands let a human list, " +
			"approve, or deny those proposals. Approving performs the actual send " +
			"through the daemon, so `gmcli serve` must be running with " +
			"--read-only=false.",
	}
	c.AddCommand(approvalsListCmd())
	c.AddCommand(approvalsApproveCmd())
	c.AddCommand(approvalsDenyCmd())
	return c
}

// dialDaemon connects to the serve socket for the resolved store layout,
// auto-starting an on-demand daemon when none is running.
func dialDaemon() (*rpc.Client, error) {
	layout, err := resolveLayout()
	if err != nil {
		return nil, err
	}
	if err := daemonctl.EnsureRunning(context.Background(), layout, daemonctl.Options{LogLevel: flags.logLevel}); err != nil {
		return nil, err
	}
	return rpc.Dial(layout.Socket)
}

func approvalsListCmd() *cobra.Command {
	var status string
	var limit int
	c := &cobra.Command{
		Use:   "list",
		Short: "List queued and resolved send approvals",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialDaemon()
			if err != nil {
				return err
			}
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), approvalCallTimeout)
			defer cancel()

			var approvals []store.Approval
			if err := client.Call(ctx, "approvals.list", map[string]any{"status": status, "limit": limit}, &approvals); err != nil {
				return err
			}
			if flags.jsonOut {
				return output.JSON(os.Stdout, approvals)
			}
			if len(approvals) == 0 {
				fmt.Fprintln(os.Stderr, "No approvals.")
				return nil
			}
			rows := make([][]string, 0, len(approvals))
			for _, a := range approvals {
				created := time.UnixMilli(a.CreatedAtMS).Format("2006-01-02 15:04")
				rows = append(rows, []string{
					a.ID, a.Status, a.ConversationID, a.RequestedBy, created, truncate(a.Body, 60),
				})
			}
			return output.Table(os.Stdout, []string{"APPROVAL", "STATUS", "CONV", "REQUESTED BY", "CREATED", "BODY"}, rows)
		},
	}
	c.Flags().StringVar(&status, "status", "pending", "filter by status (pending, sent, failed, denied, canceled; empty for all)")
	c.Flags().IntVar(&limit, "limit", 50, "max rows")
	return c
}

func approvalsApproveCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "approve <approval-id>",
		Short: "Approve a queued message — this sends it through the phone",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireWritable(); err != nil {
				return err
			}
			client, err := dialDaemon()
			if err != nil {
				return err
			}
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), approvalCallTimeout)
			defer cancel()

			var approval store.Approval
			if err := client.Call(ctx, "approvals.approve", map[string]any{"approval_id": args[0]}, &approval); err != nil {
				return err
			}
			if flags.jsonOut {
				return output.JSON(os.Stdout, approval)
			}
			msgID := ""
			if approval.MessageID != nil {
				msgID = *approval.MessageID
			}
			fmt.Fprintf(os.Stderr, "Approved and sent to %s (message_id %s)\n", approval.ConversationID, msgID)
			return nil
		},
	}
	return c
}

func approvalsDenyCmd() *cobra.Command {
	var reason string
	c := &cobra.Command{
		Use:   "deny <approval-id>",
		Short: "Deny a queued message — nothing is sent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialDaemon()
			if err != nil {
				return err
			}
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), approvalCallTimeout)
			defer cancel()

			var approval store.Approval
			if err := client.Call(ctx, "approvals.deny", map[string]any{"approval_id": args[0], "reason": reason}, &approval); err != nil {
				return err
			}
			if flags.jsonOut {
				return output.JSON(os.Stdout, approval)
			}
			fmt.Fprintf(os.Stderr, "Denied %s\n", approval.ID)
			return nil
		},
	}
	c.Flags().StringVar(&reason, "reason", "", "optional reason recorded on the approval")
	return c
}
