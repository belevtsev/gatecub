package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/belevtsev/kitsu/v3/internal/workflowrunner"
)

// The record of a task names its runner, and the record is a file in the
// checkout. Where a runner service is registered, or may be, a task must not
// become a local one.
func TestLocalRunnerIsRefusedUnlessNoRunnerServiceIsRegistered(t *testing.T) {
	saved := registeredRunner
	t.Cleanup(func() { registeredRunner = saved })
	for name, c := range map[string]struct {
		err     error
		allowed bool
	}{
		"nothing is registered":       {err: workflowrunner.ErrUnregistered, allowed: true},
		"a service is registered":     {},
		"the registration is damaged": {err: errors.New("invalid machine runner registration")},
	} {
		registeredRunner = func(string) (*workflowrunner.Client, error) { return nil, c.err }
		if err := localRunnerAllowed(t.TempDir()); (err == nil) != c.allowed {
			t.Errorf("%s: local runner allowed = %t, want %t", name, err == nil, c.allowed)
		}
	}
}

func TestALocalRunWithoutARolesFilePutsTheReviewerOnTheOtherInstalledHost(t *testing.T) {
	for _, c := range []struct {
		preferred           string
		installed           []string
		developer, reviewer string
	}{
		{"", []string{"claude", "codex"}, "claude", "codex"},
		{"codex", []string{"claude", "codex"}, "codex", "claude"},
		{"codex", []string{"claude"}, "claude", "claude"}, // the preferred host is gone
		{"", []string{"codex"}, "codex", "codex"},
		{"claude", nil, "", ""},
	} {
		if developer, reviewer := localHosts(c.preferred, c.installed); developer != c.developer || reviewer != c.reviewer {
			t.Errorf("preferred %q of %v: developer %q and reviewer %q, want %q and %q", c.preferred, c.installed, developer, reviewer, c.developer, c.reviewer)
		}
	}
}

// The flags of a local run that the developer left out. One they gave stays.
func TestALocalRunStartsAtTheDeveloperAndEndsAfterAReview(t *testing.T) {
	for _, c := range []struct {
		args         string
		entry, until string
		review       bool
		reviews      int
	}{
		{args: "--local", entry: "implement", until: "verify", review: true, reviews: 3},
		{args: "--local --commit", entry: "implement", until: "deliver", review: true, reviews: 3},
		{args: "--local --require-review=false --review-limit 5", entry: "implement", until: "verify", reviews: 5},
		{args: "--local --entry verify", entry: "verify", until: "verify", reviews: 3},
		{args: "--local --entry review --until review", entry: "review", until: "review", reviews: 3},
		{args: "--local --entry plan", entry: "plan", until: "verify", reviews: 3},
		{args: "", entry: "plan", until: "verify"},
	} {
		o, err := parseWorkflow(append(strings.Fields(c.args), "Fix the demo"))
		if err != nil {
			t.Errorf("%q: %v", c.args, err)
			continue
		}
		if o.scope.Entry != c.entry || o.scope.Until != c.until || o.scope.RequireReview != c.review || o.reviews != c.reviews {
			t.Errorf("%q: entry %s, until %s, review %t, %d reviews; want %s, %s, %t, %d",
				c.args, o.scope.Entry, o.scope.Until, o.scope.RequireReview, o.reviews, c.entry, c.until, c.review, c.reviews)
		}
	}
}

// A run of the checks alone starts no agent, so it needs no host on PATH: the
// roles then go to the host Kitsu was installed for, as before.
func TestALocalRunWithoutAnInstalledHostKeepsTheRecordedOne(t *testing.T) {
	home := t.TempDir()
	for _, name := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "AppData"} {
		t.Setenv(name, home)
	}
	t.Setenv("PATH", t.TempDir())
	path, err := uxPersonalConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("version = 1\nhost = \"codex\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := workflowRoleConfig(t.TempDir(), true)
	if err != nil || config.ConfiguredHost("developer") != "codex" || config.ConfiguredHost("reviewer") != "codex" {
		t.Fatalf("want the roles on the recorded host, got %v: %v", config, err)
	}
}
