package localrunner

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/belevtsev/kitsu/v3/internal/fslock"
	"github.com/belevtsev/kitsu/v3/internal/proc"
	"github.com/belevtsev/kitsu/v3/internal/workflowrunner"
)

// logLimit is how much of a check's newest output its result keeps.
const logLimit = 120 << 10

// errAlive marks a job that left a process running.
var errAlive = errors.New("a process of the job could not be stopped")

// Supervise runs the job kept in dir and leaves its signed receipt there. It
// is the whole life of the detached command that a local run starts for a job,
// so the command that asked for the job may end at any time. program names the
// file to start for a check, or nothing to start the first word of its
// command.
func Supervise(ctx context.Context, dir string, program func(tree string, check workflowrunner.Check) (string, error)) error {
	// A command that only asks whether the job is taken holds the lock for a moment.
	lock, err := fslock.Acquire(ctx, filepath.Join(dir, lockFile), fslock.Options{Wait: 2 * time.Second})
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if _, err := os.Stat(filepath.Join(dir, receiptFile)); err == nil {
		return nil
	}
	var a attempt
	if err := readJSON(dir, attemptFile, &a); err != nil {
		return err
	}
	r, err := load(ctx, a.Project, false)
	if err != nil {
		return err
	}
	r.offer = a.Offer
	ctx, stop := context.WithDeadline(ctx, a.Job.Deadline)
	defer stop()
	go watch(ctx, stop, filepath.Join(dir, cancelFile))
	got := r.receipt(a.Job.Attempt, a.Job.Binding, a.Key, "returned", "")
	err = a.run(ctx, dir, program, &got)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		got = r.receipt(a.Job.Attempt, a.Job.Binding, a.Key, "stopped", "the job did not end before its deadline")
	case ctx.Err() != nil:
		got = r.receipt(a.Job.Attempt, a.Job.Binding, a.Key, "stopped", "the job was stopped before it ended")
	case err != nil:
		got = r.receipt(a.Job.Attempt, a.Job.Binding, a.Key, "failed", err.Error())
	}
	got.Stopped = !errors.Is(err, errAlive)
	if err := writeJSON(dir, receiptFile, r.sign(got)); err != nil {
		return err
	}
	return errors.Join(os.RemoveAll(filepath.Join(dir, treeDir)), os.Remove(filepath.Join(dir, filesFile)))
}

// watch stops the job when the command that waits for it asks so.
func watch(ctx context.Context, stop context.CancelFunc, mark string) {
	for ctx.Err() == nil {
		if _, err := os.Stat(mark); err == nil {
			stop()
		}
		select {
		case <-ctx.Done():
		case <-time.After(poll):
		}
	}
}

// run makes the private copy of the candidate and does the job in it: the
// agent of a role, or every check in order. A check that fails does not stop
// the ones after it. What the job returned goes into got.
func (a attempt) run(ctx context.Context, dir string, program func(string, workflowrunner.Check) (string, error), got *workflowrunner.Receipt) error {
	var files []workflowrunner.File
	if err := readJSON(dir, filesFile, &files); err != nil {
		return err
	}
	// A copy of its own: the worker of a supervisor that was killed may still
	// be writing into the one before. Its name is short, because most programs
	// on Windows open no path longer than 260 characters.
	_ = os.RemoveAll(filepath.Join(dir, treeDir))
	tree := filepath.Join(dir, treeDir, rand.Text()[:8])
	if err := materialize(ctx, a.Project, tree, files); err != nil {
		return fmt.Errorf("the private copy of the candidate could not be made: %w", err)
	}
	if a.Job.Role != "" {
		return a.work(ctx, dir, tree, files, got)
	}
	for _, id := range a.Job.Checks {
		result, err := a.check(ctx, tree, id, program)
		if err != nil {
			return err
		}
		got.Checks = append(got.Checks, result)
	}
	return nil
}

// check runs one check of the policy in the private copy. The outcome is an
// error when the program gave no verdict of its own.
func (a attempt) check(ctx context.Context, tree, id string, program func(string, workflowrunner.Check) (string, error)) (workflowrunner.CheckResult, error) {
	got := workflowrunner.CheckResult{ID: id, Outcome: "error"}
	i := slices.IndexFunc(a.Offer.Policy.Checks, func(c workflowrunner.Check) bool { return c.ID == id })
	if i < 0 {
		return got, fmt.Errorf("the job names the check %q, which its policy does not hold", id)
	}
	check := a.Offer.Policy.Checks[i]
	log := &proc.TailBuffer{Limit: logLimit}
	file, err := program(tree, check)
	if err != nil {
		got.Log = []byte("kitsu: " + err.Error() + "\n")
		return got, nil
	}
	ended := proc.Run(ctx, proc.Spec{Path: cmp.Or(file, check.Argv[0]), Args: check.Argv[1:], Dir: tree, Env: environment(),
		Stdout: log, Stderr: log, Timeout: a.Limits[id]})
	switch {
	case ended.Cleanup != nil:
		return got, fmt.Errorf("%w: check %s: %v", errAlive, id, ended.Cleanup)
	case ended.Stopped:
		fmt.Fprintf(log, "\nkitsu: the check did not end within its timeout of %s\n", a.Limits[id])
	case ended.Code < 0:
		fmt.Fprintf(log, "\nkitsu: the program `%s` could not be started or did not exit on its own: %v\n", check.Argv[0], ended.Err)
	case ended.Code == 0:
		got.Outcome, got.Exit = "passed", &ended.Code
	default:
		got.Outcome, got.Exit = "failed", &ended.Code
	}
	got.Log = log.Bytes()
	return got, nil
}

// materialize writes the candidate into tree and commits it there, so that a
// check which asks git about the candidate gets an answer. The repository of
// the copy has that one commit and none of the history. It can read the
// objects of the project, so that a reviewer can run the comparison its
// assignment names.
func materialize(ctx context.Context, project, tree string, files []workflowrunner.File) error {
	if err := os.RemoveAll(tree); err != nil {
		return err
	}
	if err := os.MkdirAll(tree, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(tree)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for _, file := range files {
		mode := fs.FileMode(0o644)
		if file.Mode == "100755" {
			mode = 0o755
		}
		if err := root.MkdirAll(path.Dir(file.Path), 0o755); err != nil {
			return err
		}
		if err := root.WriteFile(file.Path, file.Data, mode); err != nil {
			return err
		}
	}
	objects := proc.Run(ctx, proc.Spec{Path: "git", Args: []string{"rev-parse", "--path-format=absolute", "--git-path", "objects"},
		Dir: project, Env: environment(), StdoutLimit: 4 << 10})
	for i, args := range [][]string{
		{"init", "-q", "--template="},
		{"config", "core.fsmonitor", "false"}, // a daemon would outlive the job and hold the copy
		{"config", "core.longpaths", "true"},  // Git for Windows stops at 260 characters without it
		{"add", "-A", "-f"},
		{"-c", "user.name=Kitsu", "-c", "user.email=kitsu@localhost", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull,
			"commit", "-q", "--no-verify", "--allow-empty", "-m", "Candidate"},
	} {
		if ended := proc.Run(ctx, proc.Spec{Path: "git", Args: args, Dir: tree, Env: environment(), StderrLimit: 4 << 10}); ended.Code != 0 {
			return fmt.Errorf("git %s: %s %v", args[0], bytes.TrimSpace(ended.Stderr), ended.Err)
		}
		if i == 0 && objects.Code == 0 {
			if err := root.WriteFile(".git/objects/info/alternates", objects.Stdout, 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// elsewhere are the variables that point git at one repository. A check runs
// in the private copy, so it must not inherit them.
var elsewhere = []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY", "GIT_PREFIX"}

// environment is the environment of a check: this command's own, as for every
// declared check, without the variables in elsewhere.
func environment() []string {
	return slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(elsewhere, name)
	})
}
