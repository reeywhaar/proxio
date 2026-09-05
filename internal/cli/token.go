package cli

import (
	"encoding/json"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"proxio/internal/app"
)

func tokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage the tokens that may use this proxy",
		Long: "A token is the whole of proxio's access control. Give each service its own,\n" +
			"labelled with the name of that service, so one can be withdrawn without\n" +
			"disturbing the others.",
	}
	cmd.AddCommand(tokenCreateCmd(), tokenListCmd(), tokenDeleteCmd())
	return cmd
}

func tokenCreateCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "create <label>",
		Short:   "Mint a token and print it once",
		Args:    cobra.ExactArgs(1),
		Example: "  docker exec " + app.Name + " " + app.Name + " token create some_service",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, st, err := setup()
			if err != nil {
				return err
			}
			tok, secret, err := st.Create(args[0])
			if err != nil {
				return err
			}

			// The secret alone on stdout, everything else on stderr, so
			// `TOKEN=$(… token create x)` captures the token and nothing else.
			cmd.Println(secret)
			cmd.PrintErrf("created %q at %s\n", tok.Label, tok.Created().Format(time.RFC3339))
			cmd.PrintErrln("This is the only time it is shown. It is stored hashed, so a lost token is deleted and minted again rather than recovered.")
			return nil
		},
	}
}

func tokenListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the tokens, by label",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, st, err := setup()
			if err != nil {
				return err
			}
			list, err := st.List()
			if err != nil {
				return err
			}

			if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
				// An empty list is [] rather than null, so a consumer can iterate it without
				// checking first.
				out := make([]map[string]any, 0, len(list))
				for _, t := range list {
					out = append(out, map[string]any{
						"label":      t.Label,
						"created_at": t.Created().Format(time.RFC3339),
					})
				}
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}

			if len(list) == 0 {
				cmd.PrintErrln("no tokens yet; create one with `" + app.Name + " token create <label>`")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			defer w.Flush()
			if _, err := w.Write([]byte("LABEL\tCREATED\n")); err != nil {
				return err
			}
			for _, t := range list {
				if _, err := w.Write([]byte(t.Label + "\t" + t.Created().Format(time.RFC3339) + "\n")); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "print the list as JSON")
	return cmd
}

func tokenDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <label>",
		Short: "Withdraw a token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, st, err := setup()
			if err != nil {
				return err
			}
			if err := st.Delete(args[0]); err != nil {
				return err
			}
			// The running server notices within one request: it re-reads the file whenever
			// its timestamp changes, so there is nothing to restart.
			cmd.PrintErrf("deleted %q\n", args[0])
			return nil
		},
	}
}
