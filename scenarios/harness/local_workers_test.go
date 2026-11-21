package harness_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/belevtsev/kitsu/v3/internal/kitsutest"
)

var (
	completed = map[string]any{"status": "complete", "note": "Done as asked."}
	approved  = map[string]any{"decision": "approve", "note": "The change does what was asked.", "findings": []string{}}
)

// In localRunRoles the developer is Codex and the reviewer is Claude Code.
func TestLocalRunSendsFailedChecksAndFindingsBackToTheDeveloper(t *testing.T) {
	r := localRunRepo(t, kitsutest.Demo(t))
	r.Setenv("GITHUB_TOKEN", "a secret of another service")
	r.Setenv("CLAUDECODE", "1")
	calls := scriptHosts(t, r, map[string][]hostTurn{
		"codex": {
			{Write: map[string]string{"src/app.txt": "broken\n", "src/hello.txt": "hello", "build/out.bin": "built"}, Result: completed},
			{Write: map[string]string{"src/app.txt": "ok\n"}, Result: completed},
			{Write: map[string]string{"src/hello.txt": "hello\n"}, Result: completed},
		},
		"claude": {
			{Diff: true, Result: map[string]any{"decision": "findings", "note": "One defect.", "findings": []string{"test: src/hello.txt has no final newline"}}},
			{Diff: true, Result: approved},
		},
	})
	got := r.Kitsu("task", "run", "--local", "Add a greeting", "--id", "greeting", "--entry", "implement", "--require-review")
	if got.Code != 0 || !strings.Contains(got.Output(), "next done") {
		t.Fatalf("the run did not end verified and reviewed, exit %d:\n%s", got.Code, got.Output())
	}
	if r.Read("src/app.txt") != "ok\n" || r.Read("src/hello.txt") != "hello\n" {
		t.Errorf("the worktree does not hold the repaired change: %q, %q", r.Read("src/app.txt"), r.Read("src/hello.txt"))
	}
	if r.Exists("build/out.bin") {
		t.Error("a file that git ignores in the private copy was imported")
	}

	developer, reviewer := calls("codex"), calls("claude")
	if len(developer) != 3 || len(reviewer) != 2 {
		t.Fatalf("want 3 developer and 2 reviewer starts, got %d and %d:\n%s", len(developer), len(reviewer), got.Output())
	}
	if !strings.Contains(developer[0].Prompt, "Fix the code, not the gate") || !strings.Contains(reviewer[0].Prompt, "weakened or removed") {
		t.Errorf("the workers were not told to leave the checks as strong as they were:\n%s\n%s", developer[0].Prompt, reviewer[0].Prompt)
	}
	if !strings.Contains(developer[1].Prompt, "failed_assertion") || !strings.Contains(developer[2].Prompt, "has no final newline") {
		t.Errorf("a repair was not told what failed:\n%s\n%s", developer[1].Prompt, developer[2].Prompt)
	}
	for _, call := range append(developer, reviewer...) {
		names := strings.ToUpper(strings.Join(call.Env, " ")) // Windows spells it Path.
		if strings.HasPrefix(call.Dir, r.Dir) || strings.Contains(names, "GITHUB_TOKEN") || strings.Contains(names, "CLAUDECODE") || !slices.Contains(strings.Fields(names), "PATH") {
			t.Errorf("a worker ran in the worktree or inherited more than the passed variables: %s %v", call.Dir, call.Env)
		}
	}
	if args := strings.Join(developer[0].Args, " "); !strings.HasPrefix(args, "exec --cd ") || !strings.Contains(args, "--sandbox workspace-write") {
		t.Errorf("the developer was not started to write in its copy: %s", args)
	}
	if args := strings.Join(reviewer[0].Args, " "); !strings.Contains(args, "--permission-mode plan") || !strings.Contains(args, "--setting-sources user") {
		t.Errorf("the reviewer was not started read-only with the user's settings alone: %s", args)
	}
	// The private copy can read the objects of the project, so the comparison
	// that the assignment names runs there.
	if !strings.Contains(reviewer[1].Diff, "+hello") || strings.HasPrefix(reviewer[1].Diff, "failed") {
		t.Errorf("the reviewer could not run the comparison of its assignment:\n%s", reviewer[1].Diff)
	}
	if show := r.Kitsu("task", "show", "greeting"); !strings.Contains(show.Output(), "repository checks: passed (runner: local)") {
		t.Errorf("the saved task lost its local evidence:\n%s", show.Output())
	}
}

func TestLocalRunRefusesAWorkerThatChangesProtectedPaths(t *testing.T) {
	for name, write := range map[string]map[string]string{
		"host settings":        {".claude/settings.json": `{"permissions":{"allow":["Bash"]}}`, "src/hello.txt": "hello\n"},
		"the check policy":     {"kitsu.toml": "version = 1\n", "src/hello.txt": "hello\n"},
		"the role assignments": {"kitsu-roles.toml": "version = 2\n"},
		"a change record":      {"openspec/changes/greeting/proposal.md": "# Greeting\n", "src/hello.txt": "hello\n"},
	} {
		t.Run(name, func(t *testing.T) {
			r := localRunRepo(t, kitsutest.Demo(t))
			before := r.Git("status", "--porcelain", "--ignored")
			write["../outside.txt"] = "written beside the private copy\n"
			calls := scriptHosts(t, r, map[string][]hostTurn{"codex": {{Write: write, Result: completed}}})
			got := r.Kitsu("task", "run", "--local", "Add a greeting", "--id", "refused", "--entry", "implement")
			if got.Code == 0 || !strings.Contains(got.Output(), "invalid or protected path") {
				t.Fatalf("a change to %s was not refused, exit %d:\n%s", name, got.Code, got.Output())
			}
			if after := r.Git("status", "--porcelain", "--ignored"); after != before || r.Exists("src/hello.txt") {
				t.Errorf("the refused result changed the worktree:\n%s", after)
			}
			if len(calls("codex")) != 1 {
				t.Errorf("want one developer start, got %d", len(calls("codex")))
			}
		})
	}
}

func TestLocalRunStartsNoArchitectAndNoTestWriter(t *testing.T) {
	r := localRunRepo(t, kitsutest.Demo(t))
	calls := scriptHosts(t, r, nil)
	for _, entry := range []string{"plan", "tests"} {
		got := r.Kitsu("task", "run", "--local", "Fix the demo", "--id", "planned", "--entry", entry)
		if got.Code != 1 || !strings.Contains(got.Output(), "no architect and no test writer yet") {
			t.Fatalf("a run that starts at %s was not refused, exit %d:\n%s", entry, got.Code, got.Output())
		}
	}
	if show := r.Kitsu("task", "show", "planned"); show.Code == 0 || len(calls("codex"))+len(calls("claude")) != 0 {
		t.Fatalf("the refused run saved a task or started a host:\n%s", show.Output())
	}
}
