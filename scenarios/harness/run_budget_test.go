package harness_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/belevtsev/kitsu/v3/internal/kitsutest"
)

// The numbers in this file are what a local run costs: agent starts for the
// developer's account, and bytes for a chat that started the run. They only
// go down.
const chatBytes = 300 // everything a chat reads from a run: the --json reply and the line that names the task

// budgetRepo is a repository as kitsu init leaves it, with approved checks
// and no roles file. Both hosts are installed, so by default the developer
// works in Claude Code and the reviewer in Codex.
func budgetRepo(t *testing.T) *kitsutest.Repo {
	t.Helper()
	r := kitsutest.Demo(t)
	r.Setenv("LOCALAPPDATA", filepath.Join(r.Home, "cache"))
	approveCIRepo(t, r)
	return r
}

func TestALocalRunStartsNoMoreAgentsThanItsPathNeeds(t *testing.T) {
	greeting := hostTurn{Write: map[string]string{"src/hello.txt": "hello\n"}, Result: completed}
	finding := hostTurn{Diff: true, Result: map[string]any{"decision": "findings", "note": "One defect.", "findings": []string{"code: the greeting has no name"}}}
	for name, path := range map[string]struct {
		developer, reviewer []hostTurn
	}{
		"approved at once": {[]hostTurn{greeting}, []hostTurn{{Diff: true, Result: approved}}},
		"one finding": {
			[]hostTurn{greeting, {Write: map[string]string{"src/hello.txt": "hello, world\n"}, Result: completed}},
			[]hostTurn{finding, {Diff: true, Result: approved}},
		},
		"one failed check": {
			[]hostTurn{{Write: map[string]string{"src/app.txt": "broken\n", "src/hello.txt": "hello\n"}, Result: completed}, {Write: map[string]string{"src/app.txt": "ok\n"}, Result: completed}},
			[]hostTurn{{Diff: true, Result: approved}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := budgetRepo(t)
			calls := scriptHosts(t, r, map[string][]hostTurn{"claude": path.developer, "codex": path.reviewer})
			got := r.Kitsu("task", "run", "--local", "Add a greeting", "--id", "budget", "--json")
			if got.Code != 0 || !strings.Contains(got.Stdout, `"next":"done"`) {
				t.Fatalf("the run did not end verified and reviewed, exit %d:\n%s", got.Code, got.Output())
			}
			// Every turn of the script was played, and a start beyond it fails the run.
			if len(calls("claude")) != len(path.developer) || len(calls("codex")) != len(path.reviewer) {
				t.Errorf("want %d developer and %d reviewer starts, got %d and %d", len(path.developer), len(path.reviewer), len(calls("claude")), len(calls("codex")))
			}
			tokens := fmt.Sprintf(`"tokens":%d`, 110*(len(path.developer)+len(path.reviewer))) // each scripted start reports 100 and 10
			if len(got.Output()) > chatBytes || strings.Count(got.Output(), "\n") != 2 || !strings.Contains(got.Stdout, tokens) {
				t.Errorf("a chat reads %d bytes, want at most %d in two lines, with %s:\n%s", len(got.Output()), chatBytes, tokens, got.Output())
			}
		})
	}
}

// A person at a terminal is told what will start before it starts, and what
// each start took afterwards.
func TestALocalRunNamesItsStagesAndWhatEachTook(t *testing.T) {
	r := budgetRepo(t)
	scriptHosts(t, r, map[string][]hostTurn{
		"claude": {{Write: map[string]string{"src/hello.txt": "hello\n"}, Result: completed}},
		"codex":  {{Diff: true, Result: approved}},
	})
	got := r.Kitsu("task", "run", "--local", "Add a greeting", "--id", "told")
	if got.Code != 0 || !strings.Contains(got.Stdout, "next done") {
		t.Fatalf("the run did not end verified and reviewed, exit %d:\n%s", got.Code, got.Output())
	}
	for _, want := range []string{
		"Stages: developer (claude, sonnet), then checks (unit, lint), then reviewer (codex, gpt-6.1-sol). At most 2 repairs and 3 reviews.",
		"Kitsu task told · implement · developer on claude\n", "Kitsu task told · verify\n", "Kitsu task told · review · reviewer on codex\n",
	} {
		if !strings.Contains(got.Stderr, want) {
			t.Errorf("the run did not say %q:\n%s", want, got.Stderr)
		}
	}
	rows := strings.Split(got.Stdout, "\n")
	if len(rows) < 5 || !strings.HasPrefix(rows[0], "STAGE") || strings.Join(strings.Fields(rows[1])[:4], " ") != "implement developer on claude" ||
		strings.Fields(rows[2])[1] != "checks" || strings.Join(strings.Fields(rows[4])[2:], " ") != "200 0 20" {
		t.Errorf("the run did not list what each stage took, with 100 input and 10 output tokens for each agent:\n%s", got.Stdout)
	}
}

func TestALocalRunThatOnlyVerifiesStartsNoReviewer(t *testing.T) {
	r := budgetRepo(t)
	calls := scriptHosts(t, r, nil)
	got := r.Kitsu("task", "run", "--local", "Verify the demo", "--id", "checked", "--entry", "verify")
	if got.Code != 0 || !strings.Contains(got.Stdout, "next done") || !strings.Contains(got.Stderr, "Stages: checks (unit, lint). ") {
		t.Fatalf("a run that enters at verify did not end with its checks alone, exit %d:\n%s", got.Code, got.Output())
	}
	if len(calls("claude"))+len(calls("codex")) != 0 {
		t.Error("a run that enters at verify started an agent")
	}
}

// kitsu.toml can require a review of every change. A run cannot leave that
// review out, so its stage list names it.
func TestALocalRunNamesTheReviewThatTheRepositoryRequires(t *testing.T) {
	r := kitsutest.Demo(t)
	r.Write("kitsu.toml", strings.Replace(kitsutest.DemoDeclaration, "version = 1\n", "version = 1\nrequire_review = true\n", 1))
	r.Git("commit", "-qam", "Require a review")
	r.Setenv("LOCALAPPDATA", filepath.Join(r.Home, "cache"))
	approveCIRepo(t, r)
	calls := scriptHosts(t, r, map[string][]hostTurn{
		"claude": {{Write: map[string]string{"src/hello.txt": "hello\n"}, Result: completed}},
		"codex":  {{Diff: true, Result: approved}},
	})
	got := r.Kitsu("task", "run", "--local", "Add a greeting", "--id", "required", "--require-review=false")
	if got.Code != 0 || !strings.Contains(got.Stdout, "next done") || len(calls("codex")) != 1 {
		t.Fatalf("the required review did not run, exit %d, %d reviewer starts:\n%s", got.Code, len(calls("codex")), got.Output())
	}
	if !strings.Contains(got.Stderr, "then checks (unit, lint), then reviewer (codex, gpt-6.1-sol).") {
		t.Errorf("the stage list leaves out the review that kitsu.toml requires:\n%s", got.Stderr)
	}
}

// A chat that started a run is told why it paused and how to go on.
func TestALocalRunTellsAChatWhyItPaused(t *testing.T) {
	r := budgetRepo(t)
	scriptHosts(t, r, map[string][]hostTurn{
		"claude": {{Write: map[string]string{"src/hello.txt": "hello\n"}, Result: completed}, {Write: map[string]string{"src/hello.txt": "hello, world\n"}, Result: completed}},
		"codex":  {{Diff: true, Result: map[string]any{"decision": "findings", "note": "One defect.", "findings": []string{"code: the greeting has no name"}}}},
	})
	got := r.Kitsu("task", "run", "--local", "Add a greeting", "--id", "paused", "--review-limit", "1", "--json")
	for _, want := range []string{`"next":"paused"`, `"reason":"review_limit"`, `"resume":["kitsu","task","next","paused","--brief"]`} {
		if got.Code == 0 || !strings.Contains(got.Stdout, want) {
			t.Fatalf("a run at its review limit did not reply with %s, exit %d:\n%s", want, got.Code, got.Output())
		}
	}
}
