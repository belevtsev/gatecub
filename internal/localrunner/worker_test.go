package localrunner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/belevtsev/kitsu/v3/internal/workflowrunner"
)

// TestMain lets this test program be the supervisor that the runner starts
// for a job, and a host named codex that never answers.
func TestMain(m *testing.M) {
	switch name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe"); {
	case name == "codex":
		os.Exit(silentHost())
	case len(os.Args) == 4 && os.Args[1] == "task" && os.Args[2] == "run-job":
		if err := Supervise(context.Background(), os.Args[3], nil); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// silentHost starts a child of its own, writes both process IDs beside its
// program and waits far longer than any job of these tests may run.
func silentHost() int {
	child := exec.Command("sleep", "120")
	if err := child.Start(); err != nil {
		return 70
	}
	pids := fmt.Sprintf("%d %d", os.Getpid(), child.Process.Pid)
	if err := os.WriteFile(filepath.Join(filepath.Dir(os.Args[0]), "pids"), []byte(pids), 0o644); err != nil {
		return 71
	}
	fmt.Println(`{"type":"thread.started","thread_id":"silent"}`)
	time.Sleep(2 * time.Minute)
	return 0
}

func roleJob(r *Runner, deadline time.Time) workflowrunner.Job {
	return workflowrunner.Job{Attempt: attemptName, Binding: "binding", Profile: Profile, Policy: workflowrunner.Digest(r.Offer().Policy),
		Kind: "implement", Role: "developer", Host: "codex", Prompt: "Change nothing.", Schema: json.RawMessage(`{"type":"object"}`), Deadline: deadline}
}

func TestARoleNeedsItsHostOnPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var got workflowrunner.Receipt
	a := attempt{Job: roleJob(&Runner{}, time.Now().Add(time.Minute))}
	if err := a.work(t.Context(), t.TempDir(), t.TempDir(), nil, &got); err == nil || !strings.Contains(err.Error(), `needs the program "codex"`) {
		t.Fatalf("a role without its host did not say so: %v", err)
	}
}

// The copy lies under the user's cache directory, so the objects of its
// repository can lie deeper than the 260 characters Git for Windows opens by
// default.
func TestThePrivateCopyHoldsARepositoryBelowALongPath(t *testing.T) {
	asUser(t)
	// 215 characters: a new repository still fits, an object of it does not.
	base := t.TempDir()
	tree := filepath.Join(base, strings.Repeat("p", max(1, 209-len(base))), "tree")
	candidate := []workflowrunner.File{{Path: "src/app.txt", Mode: "100644", Data: []byte("ok\n")}}
	if err := materialize(t.Context(), t.TempDir(), tree, candidate); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", "status", "--porcelain")
	command.Dir = tree
	if out, err := command.CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("the copy at %s (%d characters) is not a clean repository of the candidate: %v\n%s", tree, len(tree), err, out)
	}
}

func TestChangesAreWhatDiffersFromTheCandidateWhateverTheWorkerDidToGit(t *testing.T) {
	asUser(t)
	tree := filepath.Join(t.TempDir(), "tree")
	file := func(name, data string) workflowrunner.File {
		return workflowrunner.File{Path: name, Mode: "100644", Data: []byte(data)}
	}
	candidate := []workflowrunner.File{file(".gitignore", "build/\n"), file("keep.txt", "kept\n"), file("edit.txt", "old\n"), file("gone.txt", "gone\n"), file("run.sh", "true\n")}
	// The project ignores build/ for everyone and *.log for this developer alone.
	project := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = dir
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(project, "init", "-q")
	for name, data := range map[string]string{".gitignore": "build/\n", ".git/info/exclude": "*.log\n"} {
		if err := os.WriteFile(filepath.Join(project, filepath.FromSlash(name)), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := materialize(t.Context(), project, tree, candidate); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"edit.txt": "new\n", "new/file.txt": "added\n", "build/out.bin": "built\n", "test.log": "ran\n"} {
		path := filepath.Join(tree, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(tree, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	want := []string{"edit.txt 100644 new\n", "gone.txt  ", "new/file.txt 100644 added\n"}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(filepath.Join(tree, "run.sh"), 0o755); err != nil {
			t.Fatal(err)
		}
		want = append(want, "run.sh 100755 true\n")
	}
	// A worker that commits its work hides nothing: the candidate is the measure.
	git(tree, "add", "-A")
	git(tree, "-c", "user.name=W", "-c", "user.email=w@localhost", "commit", "-qm", "Mine")
	differ := func() []string {
		t.Helper()
		got, err := changes(t.Context(), project, tree, candidate)
		if err != nil {
			t.Fatal(err)
		}
		var seen []string
		for _, f := range got {
			seen = append(seen, f.Path+" "+f.Mode+" "+string(f.Data))
		}
		return seen
	}
	if seen := differ(); !slices.Equal(seen, want) {
		t.Errorf("changes are\n%q\nwant\n%q", seen, want)
	}

	// A worker that replaces the repository of its copy by a file makes no
	// other file look deleted. The file itself is a change, which the import refuses.
	if err := os.Rename(filepath.Join(tree, ".git"), filepath.Join(filepath.Dir(tree), "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, ".git"), []byte("gitdir: ../moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if seen := differ(); !slices.Equal(seen, append([]string{".git 100644 gitdir: ../moved\n"}, want...)) {
		t.Errorf("with a file named .git, changes are\n%q", seen)
	}

	if runtime.GOOS != "windows" {
		if err := os.Symlink("keep.txt", filepath.Join(tree, "link")); err != nil {
			t.Fatal(err)
		}
		if _, err := changes(t.Context(), project, tree, candidate); err == nil || !strings.Contains(err.Error(), `"link"`) {
			t.Errorf("a link that the worker left was not refused: %v", err)
		}
	}
}
