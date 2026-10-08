package auth

import "github.com/spf13/cobra"

func newLogoutCommand() *cobra.Command {
	var provider, profile string

	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Remove stored credentials",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return authLogoutCmd(provider, profile)
		},
	}

	cmd.Flags().StringVarP(&provider, "provider", "p", "", "Provider to logout from (openai, anthropic); empty = all")
	cmd.Flags().StringVar(&profile, "profile", "", "Remove only this named profile of the provider")

	return cmd
}
