package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/belevtsev/kitsu/v3/internal/decl"
	"github.com/belevtsev/kitsu/v3/internal/harness"
	"github.com/belevtsev/kitsu/v3/internal/localrunner"
	"github.com/belevtsev/kitsu/v3/internal/workflowrunner"
)

// localJobCommand is the supervisor that a local run starts for each of its
// jobs. A person has no use for it.
func localJobCommand() uxCommand {
	return uxCommand{path: "task run-job", args: "<directory>", hidden: true,
		summary: "Run one kept job of a local task run; kitsu task run --local starts this itself",
		run: func(ctx context.Context, _ Config, args []string) error {
			if len(args) != 1 {
				return usagef("task run-job takes the directory of one kept job")
			}
			return localrunner.Supervise(ctx, args[0], func(tree string, check workflowrunner.Check) (string, error) {
				return checkExecutable(tree, decl.Check{ID: check.ID, Run: check.Argv})
			})
		}}
}

// openWorkflowRunner picks the runner of a run: the one a saved task pinned,
// the local one for --local, and otherwise the registered service.
func openWorkflowRunner(ctx context.Context, cfg Config, s *session, o workflowOptions) (workflowrunner.Runner, error) {
	local, pinned := o.local, ""
	var unread error // why the saved task could not be read
	if o.resume != "" {
		l, err := readOnlyStageLedger(s.repo.GitDir)
		if err == nil {
			var task *harness.Task
			if task, err = readWorkflow(l, o.id); err == nil {
				local, pinned = task.Workflow.Profile == localrunner.Profile, task.RepositoryPolicy
			}
			l.close()
		}
		unread = err
	}
	switch {
	case local && o.resume == "" && o.scope.Entry != "verify":
		return nil, errors.New("a local run starts no agent yet. Add --entry verify to run the checks of kitsu.toml on your change")
	case local:
		return openLocalRunner(ctx, s, pinned)
	case cfg.workflowRunner != nil:
		return cfg.workflowRunner(ctx, s.repo.Root)
	}
	client, err := workflowrunner.Open(ctx, s.repo.Root)
	if errors.Is(err, workflowrunner.ErrUnregistered) && unread != nil {
		// With a runner service, the run itself reports the task. Without one,
		// "register a runner" would hide that the task is the problem.
		if errors.Is(unread, fs.ErrNotExist) {
			return nil, fmt.Errorf("there is no saved task %s", o.id)
		}
		return nil, fmt.Errorf("task %s cannot be resumed: %w", o.id, unread)
	}
	if err != nil {
		return nil, err
	}
	if client.Profile == localrunner.Profile {
		return nil, errors.New("a registered runner cannot use the profile name local")
	}
	return client, nil
}

// openLocalRunner builds the policy of a local run: the checks of kitsu.toml
// that the developer approved with kitsu check trust. pinned is the
// declaration a saved task started with.
func openLocalRunner(ctx context.Context, s *session, pinned string) (workflowrunner.Runner, error) {
	if err := localRunnerAllowed(s.repo.Root); err != nil {
		return nil, err
	}
	file, fingerprint, err := workingCheckPolicy(ctx, s.repo)
	if err != nil {
		return nil, err
	}
	if pinned != "" && pinned != fingerprint {
		return nil, errors.New("kitsu.toml changed since this task started. Restore it, or open a new task")
	}
	if file == nil || len(file.Checks) == 0 {
		return nil, errors.New("the checks of kitsu.toml gate a local run, and it declares none. Add one with kitsu check add")
	}
	if approval, err := trustFile(s.repo, ""); err != nil || string(approval.data) != fingerprint+"\n" {
		return nil, errors.New("approve the checks of kitsu.toml with kitsu check trust in your own terminal first")
	}
	config, err := dailyRoleConfig(s.repo.Root)
	if err != nil {
		return nil, err
	}
	policy := workflowrunner.Policy{Declaration: fingerprint, TestPaths: config.Tests}
	limits := map[string]time.Duration{}
	for _, check := range file.Checks {
		policy.Checks = append(policy.Checks, workflowrunner.Check{ID: check.ID, Argv: check.Run})
		limits[check.ID] = check.Timeout
	}
	return localrunner.Open(ctx, s.repo.Root, policy, limits)
}

// savedWorkflowRunner is the runner that signed the evidence of a saved task.
func savedWorkflowRunner(root string, w *harness.Workflow) (workflowrunner.Runner, error) {
	if w.Profile != localrunner.Profile {
		return workflowrunner.Load(root)
	}
	if err := localRunnerAllowed(root); err != nil {
		return nil, err
	}
	return localrunner.Load(root)
}

// registeredRunner finds the runner service of a worktree. Tests replace it.
var registeredRunner = workflowrunner.Load

// localRunnerAllowed refuses the local runner wherever an administrator
// registered a runner service for the worktree, and wherever that cannot be
// told. A task of the service must never count on local evidence, which the
// user who made the change can sign: its record names its runner, and that
// record is a file in the checkout.
func localRunnerAllowed(root string) error {
	if _, err := registeredRunner(root); !errors.Is(err, workflowrunner.ErrUnregistered) {
		return errors.New("a runner service is registered for this worktree, or its registration cannot be read. The local runner is not available here")
	}
	return nil
}

// workflowStopReason is why a job ended without a result, when its runner
// says so. The local runner does.
func workflowStopReason(got workflowrunner.Receipt) string {
	var result struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(got.Result, &result) != nil || result.Error == "" {
		return ""
	}
	return ": " + result.Error
}

// workflowStartNotice is what a new run says before its first job.
func workflowStartNotice(o workflowOptions) string {
	if o.local {
		return "Runner: local. The approved checks of kitsu.toml run as you, in a private copy of your change. No agent starts, so the run uses no tokens."
	}
	return optInCost + " Supervised runs are not measured."
}

// localRunnerMark tells a result of the local runner from one of a registered
// runner service wherever the result is shown.
func localRunnerMark(t *harness.Task) string {
	if t.Workflow.Profile == localrunner.Profile {
		return " (runner: local)"
	}
	return ""
}
