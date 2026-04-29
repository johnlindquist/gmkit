package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/fdsouvenir/gmcli/internal/output"
	"github.com/fdsouvenir/gmcli/internal/store"
)

func contactsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "contacts",
		Short: "Look up contacts in the local archive",
	}
	c.AddCommand(contactsSearchCmd())
	c.AddCommand(contactsShowCmd())
	return c
}

func contactsSearchCmd() *cobra.Command {
	var limit int
	c := &cobra.Command{
		Use:   "search [query]",
		Short: "Search contacts by name or number (substring, case-insensitive)",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			query := joinArgs(args)
			st, err := openStore()
			if err != nil {
				return err
			}
			defer st.Close()
			hits, err := st.SearchContacts(context.Background(), query, limit)
			if err != nil {
				return err
			}
			if flags.jsonOut {
				return output.JSON(os.Stdout, hits)
			}
			if len(hits) == 0 {
				fmt.Fprintln(os.Stderr, "(no matches)")
				return nil
			}
			rows := make([][]string, 0, len(hits))
			for _, h := range hits {
				rows = append(rows, []string{
					h.Name,
					h.FormattedNumber,
					h.E164,
					h.ParticipantID,
				})
			}
			return output.Table(os.Stdout, []string{"name", "number", "e164", "participant_id"}, rows)
		},
	}
	c.Flags().IntVar(&limit, "limit", 50, "max rows")
	return c
}

func contactsShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <participant-id-or-number>",
		Short: "Show one contact's full record (lookup by participant_id or phone number)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer st.Close()
			ctx := context.Background()

			c, err := st.GetContact(ctx, args[0])
			if errors.Is(err, store.ErrNotFound) {
				c, err = st.GetContactByNumber(ctx, args[0])
			}
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("no contact matching %q", args[0])
				}
				return err
			}
			if flags.jsonOut {
				return output.JSON(os.Stdout, c)
			}
			renderContactDetail(c)
			return nil
		},
	}
}

func renderContactDetail(c store.Contact) {
	fmt.Printf("name:             %s\n", c.Name)
	fmt.Printf("participant_id:   %s\n", c.ParticipantID)
	if c.ContactID != "" {
		fmt.Printf("contact_id:       %s\n", c.ContactID)
	}
	if c.E164 != "" {
		fmt.Printf("e164:             %s\n", c.E164)
	}
	if c.FormattedNumber != "" {
		fmt.Printf("formatted_number: %s\n", c.FormattedNumber)
	}
	if c.AvatarColor != "" {
		fmt.Printf("avatar_color:     %s\n", c.AvatarColor)
	}
	if c.IsMe {
		fmt.Println("self:             true")
	}
}
