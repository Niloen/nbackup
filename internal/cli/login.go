package cli

import (
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Niloen/nbackup/internal/media"
)

// newLoginCmd implements `nb login <medium>`: run a medium's interactive credential
// bootstrap. Most media need none (disk/tape have no credentials; cloud and a
// service-account gdrive authenticate straight from the ambient environment); it exists
// for the OAuth flow a personal Google Drive needs, where a one-time browser consent must
// mint a reusable token. Verb-based, like the other actions — the medium type decides what
// "login" means (media.Spec.Login), keeping this command medium-neutral.
func newLoginCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "login <medium> [options]",
		Short:   "Authenticate a medium that needs an interactive credential bootstrap (e.g. Google Drive OAuth)",
		Long:    "Run a medium's one-time credential bootstrap. Most media need no login — disk and tape have no credentials, and a cloud bucket or a service-account Google Drive authenticate from the ambient environment. It exists for a personal Google Drive, whose OAuth consent must be granted once to mint a reusable token. The token is written to a default per-medium path the medium then reads automatically — no environment variable to set.\n\nAny options AFTER the medium name are specific to that medium's type (the neutral `nb login` command names none of them); see `nb login <medium> -h`. The gdrive flow adapts to your OAuth client: a \"TVs and Limited Input devices\" client uses a headless device code (no browser or open port here); a \"Desktop app\" client opens a browser on this machine and captures the redirect itself.",
		Example: "  nb login gdrive\n  nb login gdrive --client ~/client_secret.json",
		Args:    cobra.MinimumNArgs(1),
		// Stop flag parsing at the first positional (the medium name), so a type's own
		// options (gdrive's --client/--out) pass through as args for its login hook to
		// parse. Global flags still work before the medium. This keeps the CLI
		// medium-neutral: `nb login` hardcodes no type's flags.
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadForWrite()
			if err != nil {
				return err
			}
			m, ok := cfg.Media[args[0]]
			if !ok {
				names := make([]string, 0, len(cfg.Media))
				for n := range cfg.Media {
					names = append(names, n)
				}
				sort.Strings(names)
				return fmt.Errorf("unknown medium %q (configured: %s)", args[0], strings.Join(names, ", "))
			}
			loginFn, ok := media.LoginFor(m.Type)
			if !ok {
				return fmt.Errorf("medium %q (type %s) needs no login — it authenticates from its configuration or the ambient environment", args[0], m.Type)
			}
			opts := media.Options(maps.Clone(m.Params))
			if opts == nil {
				opts = media.Options{}
			}
			// Everything after the medium name belongs to the type's login hook.
			return loginFn(cmd.Context(), opts, cfg.SecretsPath(), args[1:], cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
	cmd.Flags().SetInterspersed(false)
	return cmd
}
