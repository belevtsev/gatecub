package harness_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/belevtsev/kitsu/v3/internal/kitsutest"
)

// A local run needs all role assignments today, although it starts at most a
// developer and a reviewer.
const localRunRoles = `version = 2
[roles.lookup]
host = "codex"
[roles.spec-writer]
host = "claude"
[roles.architect]
host = "codex"
[roles.quality-engineer]
host = "claude"
[roles.developer]
host = "codex"
[roles.reviewer]
host = "claude"
`

// localRunRepo commits the role assignments and approves the declared checks,
// as the developer does with kitsu check trust. The local runner keeps its
// jobs inside the test's own home.
func localRunRepo(t *testing.T, r *kitsutest.Repo) *kitsutest.Repo {
	t.Helper()
	r.Write("kitsu-roles.toml", localRunRoles)
	r.Git("add", "-A")
	r.Git("commit", "-qm", "Assign the roles")
	r.Setenv("LOCALAPPDATA", filepath.Join(r.Home, "cache"))
	approveCIRepo(t, r)
	return r
}

func TestLocalRunVerifiesAChangeWithTheDeclaredChecksAlone(t *testing.T) {
	r := localRunRepo(t, kitsutest.Demo(t))
	got := r.Kitsu("task", "run", "--local", "Verify the demo", "--id", "passes", "--entry", "verify")
	if got.Code != 0 || !strings.Contains(got.Output(), "repository checks: passed (runner: local)") || !strings.Contains(got.Output(), "next done") {
		t.Fatalf("a passing change was not verified, exit %d:\n%s", got.Code, got.Output())
	}
	// The view of a later command verifies the kept receipt again.
	if show := r.Kitsu("task", "show", "passes"); !strings.Contains(show.Output(), "repository checks: passed (runner: local)") {
		t.Fatalf("the saved task lost its local evidence:\n%s", show.Output())
	}

	r.Write("src/app.txt", "broken\n")
	got = r.Kitsu("task", "run", "--local", "Verify the broken demo", "--id", "fails", "--entry", "verify")
	if got.Code != 5 || !strings.Contains(got.Output(), "reviewed check unit (failed)") || strings.Contains(got.Output(), "next done") {
		t.Fatalf("a failing change was not refused with exit 5, exit %d:\n%s", got.Code, got.Output())
	}
}

func TestLocalRunRefusesUnapprovedChecks(t *testing.T) {
	r := localRunRepo(t, kitsutest.Demo(t))
	r.Write("kitsu.toml", kitsutest.DemoDeclaration+"\n[[check]]\nid = \"extra\"\nrun = [\"sh\", \"lint.sh\"]\n")
	r.Git("commit", "-qam", "Declare one more check")
	got := r.Kitsu("task", "run", "--local", "Verify the demo", "--id", "untrusted", "--entry", "verify")
	if got.Code != 1 || !strings.Contains(got.Output(), "kitsu check trust") {
		t.Fatalf("checks that nobody approved were not refused, exit %d:\n%s", got.Code, got.Output())
	}
	if show := r.Kitsu("task", "show", "untrusted"); show.Code == 0 {
		t.Fatalf("the refused run saved the task:\n%s", show.Output())
	}
}

// The check of this repository waits for a file, so that the test can end the
// command while its job runs.
func TestLocalRunKeepsTheResultOfAJobWhoseCommandWasKilled(t *testing.T) {
	r := kitsutest.NewRepo(t)
	marks := filepath.ToSlash(t.TempDir())
	r.Write("kitsu.toml", "version = 1\n\n[[check]]\nid = \"gate\"\nrun = [\"sh\", \"gate.sh\", \""+marks+"\"]\n")
	r.Write("gate.sh", "echo run >> \"$1/runs\"\nwhile [ ! -e \"$1/go\" ]; do sleep 0.1; done\ntest \"$(cat app.txt)\" = ok\n")
	r.Write("app.txt", "ok\n")
	localRunRepo(t, r)

	command := r.Command(filepath.Join(kitsutest.Binary(t), "kitsu"), "task", "run", "--local", "Verify the change", "--id", "kept", "--entry", "verify")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	runs := filepath.Join(filepath.FromSlash(marks), "runs")
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(runs); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			t.Fatal("the check of the local job did not start")
		}
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()

	// A changed declaration refuses the resume, and the job goes on.
	declaration := r.Read("kitsu.toml")
	r.Write("kitsu.toml", declaration+"\n[[check]]\nid = \"more\"\nrun = [\"sh\", \"gate.sh\"]\n")
	if got := r.Kitsu("task", "run", "--resume", "kept"); got.Code != 1 || !strings.Contains(got.Output(), "kitsu.toml changed since this task started") {
		t.Fatalf("a changed kitsu.toml did not refuse the resume, exit %d:\n%s", got.Code, got.Output())
	}
	r.Write("kitsu.toml", declaration)

	if err := os.WriteFile(filepath.Join(filepath.FromSlash(marks), "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := r.Kitsu("task", "run", "--resume", "kept")
	if got.Code != 0 || !strings.Contains(got.Output(), "repository checks: passed (runner: local)") {
		t.Fatalf("the resume did not return the kept result, exit %d:\n%s", got.Code, got.Output())
	}
	if ran, err := os.ReadFile(runs); err != nil || string(ran) != "run\n" {
		t.Fatalf("the check ran %q, want once: the resume must wait for the job that was running: %v", ran, err)
	}
}
