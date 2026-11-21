package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/belevtsev/kitsu/v3/internal/decl"
	"github.com/belevtsev/kitsu/v3/internal/pluginsetup"
)

func runUXAgentSetup(ctx context.Context, cfg Config, args []string) error {
	var host, archiveDir string
	var noTeam bool
	flags := newFlags("agent setup")
	bindOnboardingHost(flags, &host)
	flags.BoolVar(&noTeam, "no-team", false, "")
	flags.Func("archive-dir", "", func(value string) error {
		if archiveDir != "" || value == "" {
			return errors.New("supply one nonempty --archive-dir")
		}
		archiveDir = value
		return nil
	})
	p, err := parseArgs(flags, args)
	if err != nil {
		return err
	}
	if len(p.pos) != 0 || p.dash {
		return usagef("agent setup takes no positional arguments")
	}
	if archiveDir == "" {
		return usagef("--archive-dir is required")
	}
	marketplace, team, err := pluginsetup.Install(ctx, host, archiveDir, !noTeam)
	if errors.Is(err, context.Canceled) {
		return exit(130)
	}
	if err != nil {
		return failf("set up %s plugin: %s", host, decl.Plain(err.Error()))
	}
	fmt.Fprintf(cfg.Out, "Installed kitsu@kitsu for %s from %s.\n", host, decl.Plain(marketplace))
	switch {
	case team == pluginsetup.TeamOn:
		fmt.Fprintln(cfg.Out, "Installed kitsu-team@kitsu: the role agents and the managed workflow. Run this again with --no-team to remove it.")
	case team == pluginsetup.TeamDisabled:
		fmt.Fprintf(cfg.Out, "kitsu-team@kitsu is disabled in %s and was left so, without an update. Enable it there and run this again to update it.\n", host)
	case noTeam:
		fmt.Fprintln(cfg.Out, "kitsu-team@kitsu is off: the role agents and the managed workflow are not installed.")
	}
	return nil
}
