package main

import (
	"github.com/spf13/cobra"
	"github.com/tuna-os/tacklebox/internal/install"
)

// tree-diff is an internal helper, not user-facing (hence Hidden): the
// delta-layout build re-execs the tacklebox binary inside `podman
// unshare` to diff two mounted image rootfs trees into an overlayfs
// delta layer (see install.InstallLiveDelta). It has to be a
// subcommand because the mounted trees only exist inside that user
// namespace — the parent process can't reach them.
var treeDiffCmd = &cobra.Command{
	Use:    "tree-diff BASE_DIR ENV_DIR OUT_DIR",
	Short:  "Diff two rootfs trees into an overlayfs delta layer (internal)",
	Hidden: true,
	Args:   cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		exclude, _ := cmd.Flags().GetStringArray("exclude")
		return install.TreeDiff(args[0], args[1], args[2], exclude)
	},
}

func init() {
	treeDiffCmd.Flags().StringArray("exclude", nil, "top-level-relative path to prune from both trees (repeatable)")
	rootCmd.AddCommand(treeDiffCmd)
}
