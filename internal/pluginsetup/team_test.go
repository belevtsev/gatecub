package pluginsetup

import (
	"fmt"
	"path/filepath"
	"testing"
)

// teamRelease is a release whose marketplace also holds the role plugin.
func teamRelease(t *testing.T, version, skill string) release {
	t.Helper()
	files := releaseFiles(version, "task skill")
	files[0].contents = `{"name":"kitsu","plugins":[{"name":"kitsu","source":{"source":"local","path":"./"}},{"name":"kitsu-team","source":{"source":"local","path":"./plugins/kitsu-team"}}]}`
	files[1].contents = `{"name":"kitsu","plugins":[{"name":"kitsu","source":"./"},{"name":"kitsu-team","source":"./plugins/kitsu-team"}]}`
	manifest := fmt.Sprintf(`{"name":"kitsu-team","version":%q}`, version)
	for name, contents := range map[string]string{pluginManifest("codex"): manifest, pluginManifest("claude"): manifest, "skills/managed/SKILL.md": skill} {
		files = append(files, zipEntry{name: marketplaceName + "/" + teamDir + "/" + name, contents: contents})
	}
	r, err := loadRelease(writeBundle(t, fixtureZIP(t, files)), "codex")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func teamOf(market string) string { return filepath.Join(market, filepath.FromSlash(teamDir)) }

func codexItems(market string, team map[string]any) hostStep {
	items := []any{map[string]any{"pluginId": pluginID, "version": "1.2.3", "installed": true, "enabled": true,
		"source": map[string]string{"source": "local", "path": market}}}
	if team != nil {
		items = append(items, team)
	}
	return hostStep{args: []string{"list", "--json", "--marketplace", "kitsu"}, receipt: map[string]any{"installed": items}}
}

func codexTeam(source string, enabled bool) map[string]any {
	return map[string]any{"pluginId": teamID, "version": "1.2.3", "installed": true, "enabled": enabled,
		"source": map[string]string{"source": "local", "path": source}}
}

func TestCodexInstallsRemovesOrLeavesTheRolePlugin(t *testing.T) {
	t.Parallel()
	r := teamRelease(t, "1.2.3", "managed skill")
	market := savedRelease(t, r)
	stale := teamOf(savedRelease(t, teamRelease(t, "1.2.3", "older managed skill")))
	kitsu := []hostStep{codexMarketStep(market), codexInstallStep(market), codexItems(market, nil)}
	add := func(installed string) hostStep {
		return hostStep{args: []string{"add", "--json", teamID}, receipt: map[string]string{"pluginId": teamID, "installedPath": installed}}
	}
	both := codexItems(market, codexTeam(teamOf(market), true))
	for name, c := range map[string]struct {
		team  bool
		steps []hostStep
		want  string
		fails string
	}{
		"installs it by default":           {true, []hostStep{codexItems(market, nil), add(teamOf(market)), both}, TeamOn, ""},
		"updates an enabled one":           {true, []hostStep{both, add(teamOf(market)), both}, TeamOn, ""},
		"leaves a disabled one disabled":   {true, []hostStep{codexItems(market, codexTeam(teamOf(market), false))}, TeamDisabled, ""},
		"removes it when turned off":       {false, []hostStep{both, {args: []string{"remove", teamID}}}, TeamOff, ""},
		"removes a disabled one too":       {false, []hostStep{codexItems(market, codexTeam(teamOf(market), false)), {args: []string{"remove", teamID}}}, TeamOff, ""},
		"has nothing to remove":            {false, []hostStep{codexItems(market, nil)}, TeamOff, ""},
		"refuses files of another build":   {true, []hostStep{codexItems(market, nil), add(stale)}, "", "installed files do not match"},
		"refuses another effective source": {true, []hostStep{codexItems(market, nil), add(teamOf(market)), codexItems(market, codexTeam(market, true))}, "", "different Kitsu marketplace"},
		"refuses a disabled result":        {true, []hostStep{codexItems(market, nil), add(teamOf(market)), codexItems(market, codexTeam(teamOf(market), false))}, "", "enabled state"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := installCodex(t.Context(), hostScript(t, append(append([]hostStep{}, kitsu...), c.steps...)...), market, r, c.team)
			if c.fails != "" {
				assertSetupError(t, err, c.fails)
			} else if err != nil || got != c.want {
				t.Fatalf("the role plugin is %q, want %q: %v", got, c.want, err)
			}
		})
	}
}

func claudeItems(entries ...map[string]any) hostStep {
	list := []any{}
	for _, entry := range entries {
		list = append(list, entry)
	}
	return hostStep{args: []string{"list", "--json"}, receipt: list}
}

func claudeEntry(id, installed, effective string, enabled bool) map[string]any {
	return map[string]any{"id": id, "scope": "user", "version": "1.2.3", "enabled": enabled, "installPath": installed, "readFromFolder": effective}
}

func TestClaudeInstallsRemovesOrLeavesTheRolePlugin(t *testing.T) {
	t.Parallel()
	r := teamRelease(t, "1.2.3", "managed skill")
	market := savedRelease(t, r)
	stale := teamOf(savedRelease(t, teamRelease(t, "1.2.3", "older managed skill")))
	registered := claudeMarketStep("directory", market)
	main := claudeEntry(pluginID, market, market, true)
	kitsu := []hostStep{registered, {args: []string{"marketplace", "add", market}}, registered, {args: []string{"install", pluginID}}, claudeItems(main)}
	install := hostStep{args: []string{"install", teamID}}
	remove := hostStep{args: []string{"uninstall", "--scope", "user", teamID}}
	team := func(installed string, enabled bool) map[string]any {
		return claudeEntry(teamID, installed, teamOf(market), enabled)
	}
	both := claudeItems(team(teamOf(market), true), main)
	project := claudeEntry(teamID, teamOf(market), teamOf(market), false)
	project["scope"] = "project"
	for name, c := range map[string]struct {
		team  bool
		steps []hostStep
		want  string
		fails string
	}{
		"installs it by default":            {true, []hostStep{claudeItems(main), install, both}, TeamOn, ""},
		"updates an enabled one":            {true, []hostStep{both, install, both}, TeamOn, ""},
		"ignores another scope":             {true, []hostStep{claudeItems(project, main), install, both}, TeamOn, ""},
		"leaves a disabled one disabled":    {true, []hostStep{claudeItems(team(teamOf(market), false), main)}, TeamDisabled, ""},
		"removes it when turned off":        {false, []hostStep{both, remove}, TeamOff, ""},
		"has nothing to remove":             {false, []hostStep{claudeItems(main)}, TeamOff, ""},
		"refreshes files of another build":  {true, []hostStep{claudeItems(main), install, claudeItems(team(stale, true), main), {args: []string{"uninstall", "--keep-data", "--scope", "user", teamID}}, install, both}, TeamOn, ""},
		"refuses files that stay different": {true, []hostStep{claudeItems(main), install, claudeItems(team(stale, true), main), {args: []string{"uninstall", "--keep-data", "--scope", "user", teamID}}, install, claudeItems(team(stale, true), main)}, "", "installed files do not match"},
		"refuses another effective source":  {true, []hostStep{claudeItems(main), install, claudeItems(claudeEntry(teamID, teamOf(market), market, true), main)}, "", "different marketplace directory"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := installClaude(t.Context(), hostScript(t, append(append([]hostStep{}, kitsu...), c.steps...)...), market, r, c.team)
			if c.fails != "" {
				assertSetupError(t, err, c.fails)
			} else if err != nil || got != c.want {
				t.Fatalf("the role plugin is %q, want %q: %v", got, c.want, err)
			}
		})
	}
}

// An archive of a release before the role plugin holds none, and setup then
// installs the one plugin it has.
func TestSetupWithoutARolePluginInTheArchiveInstallsTheDefaultOne(t *testing.T) {
	t.Parallel()
	r := releaseFixture(t, "1.2.3", "task skill")
	market := savedRelease(t, r)
	run := hostScript(t, codexMarketStep(market), codexInstallStep(market), codexPluginStep(market, "1.2.3"))
	if got, err := installCodex(t.Context(), run, market, r, true); err != nil || got != TeamOff {
		t.Fatalf("the role plugin is %q, want %q: %v", got, TeamOff, err)
	}
}
