package localrunner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/belevtsev/kitsu/v3/internal/workflowrunner"
)

const attemptName = "ATTEMPT0ATTEMPT0ATTEMPT0AB"

// asUser gives the runner the configuration and cache directories of a user
// of the test's own, with no signing key yet.
func asUser(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "AppData", "LocalAppData"} {
		t.Setenv(name, filepath.Join(home, name))
	}
}

func open(t *testing.T, root string) *Runner {
	t.Helper()
	r, err := Open(t.Context(), root, workflowrunner.Policy{Declaration: "declared"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestVerifyAcceptsOnlyAReceiptOfThisKeyWorktreeAndReservation(t *testing.T) {
	asUser(t)
	root := t.TempDir()
	r := open(t, root)
	signed := r.sign(r.receipt(attemptName, "binding", "job", "returned", ""))

	saved, err := Load(root)
	if err != nil || saved.Authority() != r.Authority() {
		t.Fatalf("a later command did not find the same runner: %v", err)
	}
	if got, err := saved.Verify(signed, attemptName, "binding"); err != nil || got.Profile != Profile || !got.Stopped {
		t.Fatalf("the runner refused its own receipt %+v: %v", got, err)
	}

	forged := signed
	forged.Receipt = []byte(strings.Replace(string(signed.Receipt), `"returned"`, `"failed"`, 1))
	elsewhere, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	asUser(t)
	stranger := open(t, root)
	for name, err := range map[string]error{
		"another binding":   second(r.Verify(signed, attemptName, "other")),
		"another attempt":   second(r.Verify(signed, attemptName+"C", "binding")),
		"changed bytes":     second(r.Verify(forged, attemptName, "binding")),
		"another worktree":  second(elsewhere.Verify(signed, attemptName, "binding")),
		"another user":      second(stranger.Verify(signed, attemptName, "binding")),
		"another authority": authorityDiffers(r, elsewhere, stranger),
	} {
		if err == nil {
			t.Errorf("a receipt for %s was accepted", name)
		}
	}
}

func second(_ workflowrunner.Receipt, err error) error { return err }

// authorityDiffers fails, as an accepted receipt does, when another worktree
// or user has the authority of r.
func authorityDiffers(r *Runner, others ...*Runner) error {
	for _, other := range others {
		if other.Authority() == r.Authority() {
			return nil
		}
	}
	return os.ErrInvalid
}

func TestAJobTheRunnerNoLongerHoldsIsReleasedWithItsReason(t *testing.T) {
	asUser(t)
	r := open(t, t.TempDir())
	_, lost, err := r.Resume(t.Context(), attemptName, "binding", "job")
	if err != nil {
		t.Fatal(err)
	}
	if lost.Status != "stopped" || !lost.Stopped || !lost.Release || lost.Job != "job" || !strings.Contains(string(lost.Result), "no longer holds this job") {
		t.Errorf("want a released stop that says why, got %+v with %s", lost, lost.Result)
	}
	if entries, _ := os.ReadDir(r.cache); len(entries) != 0 {
		t.Errorf("a job that never started left %d entries behind", len(entries))
	}

	job := workflowrunner.Job{Attempt: attemptName, Binding: "binding", Profile: Profile, Policy: "another policy", Kind: "verify"}
	if _, _, err := r.Run(t.Context(), job); err == nil {
		t.Error("a job built for another policy was accepted")
	}
}
