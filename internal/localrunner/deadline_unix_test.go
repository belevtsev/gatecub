//go:build linux || darwin

package localrunner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAWorkerPastItsDeadlineIsStoppedAndLeavesNoProcess(t *testing.T) {
	asUser(t)
	host := installHost(t, "codex")
	r := open(t, t.TempDir())
	_, got, err := r.Run(t.Context(), roleJob(r, time.Now().Add(3*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "stopped" || !got.Stopped || !got.Release || !strings.Contains(string(got.Result), "deadline") {
		t.Fatalf("want a stop at the deadline, got %+v with %s", got, got.Result)
	}
	var worker, child int
	data, err := os.ReadFile(filepath.Join(host, "pids"))
	if _, scanErr := fmt.Sscan(string(data), &worker, &child); err != nil || scanErr != nil {
		t.Fatalf("the host never started: %v %v", err, scanErr)
	}
	for _, pid := range []int{worker, child} {
		for wait := time.Now().Add(5 * time.Second); !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(wait) {
				t.Fatalf("process %d of the stopped worker is still alive", pid)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(r.cache, attemptName, treeDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the private copy of the stopped job was kept: %v", err)
	}
}

// installHost puts a copy of this test program, named as the host, first on
// PATH and returns its directory.
func installHost(t *testing.T, host string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	program, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, host), program, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}
