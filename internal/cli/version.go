package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

func newVersionCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the wayglass version",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.output(cmd).Emit(a.info, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "wayglass %s (commit %s, %s)\n", a.info.Version, a.info.Commit, a.info.Go)
				return err
			})
		},
	}
}
