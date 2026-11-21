package localrunner

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/belevtsev/kitsu/v3/internal/workflowrunner"
)

// launch builds the command line of the worker's host. The assignment goes in
// on standard input. parse reads one event of the host's output into said,
// and finish reads what the host left in a file. A worker that may write
// edits files of the private copy without asking and gets no wider permission;
// one that may not runs in the host's read-only mode. Claude Code loads the
// user's own settings and none of a project.
func launch(job workflowrunner.Job, dir, tree string, said *answer) (args []string, parse func([]byte) error, finish func() error, err error) {
	if job.Host == "claude" {
		mode := "acceptEdits"
		if job.ReadOnly {
			mode = "plan"
		}
		var schema bytes.Buffer
		_ = json.Compact(&schema, job.Schema)
		args = []string{"--print", "--input-format", "text", "--output-format", "stream-json", "--verbose",
			"--permission-prompts", "none", "--strict-mcp-config", "--no-chrome", "--setting-sources", "user",
			"--permission-mode", mode, "--json-schema", schema.String()}
		args = append(append(args, option("--model", job.Model)...), option("--effort", job.Effort)...)
		return append(args, "--", "Complete the Kitsu assignment from standard input."), said.claude, func() error { return nil }, nil
	}
	schema, result := filepath.Join(dir, "schema.json"), filepath.Join(dir, "answer.json")
	if err := os.WriteFile(schema, job.Schema, 0o600); err != nil {
		return nil, nil, nil, err
	}
	_ = os.Remove(result) // A supervisor that replaces a dead one must not read the old answer.
	sandbox := "workspace-write"
	if job.ReadOnly {
		sandbox = "read-only"
	}
	args = append([]string{"exec", "--cd", tree, "--sandbox", sandbox, "--json", "--color", "never",
		"--output-schema", schema, "--output-last-message", result}, option("--model", job.Model)...)
	if job.Effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+strconv.Quote(job.Effort))
	}
	return append(args, "-"), said.codex, func() error {
		data, err := os.ReadFile(result)
		said.result = bytes.TrimSpace(data)
		return err
	}, nil
}

func option(name, value string) []string {
	if value == "" {
		return nil
	}
	return []string{name, value}
}

var errEvent = errors.New("an event of the host is not a JSON object")

// claude reads one event of `claude --print --output-format stream-json`.
func (said *answer) claude(data []byte) error {
	var event struct {
		Type, Subtype, Model, Result string
		Session                      string          `json:"session_id"`
		Failed                       bool            `json:"is_error"`
		Output                       json.RawMessage `json:"structured_output"`
		Usage                        struct {
			Input   int64 `json:"input_tokens"`
			Output  int64 `json:"output_tokens"`
			Cached  int64 `json:"cache_read_input_tokens"`
			Created int64 `json:"cache_creation_input_tokens"`
		}
	}
	if json.Unmarshal(data, &event) != nil {
		return errEvent
	}
	switch {
	case event.Type == "system" && event.Subtype == "init":
		if said.session != "" && said.session != event.Session {
			return errors.New("the host started a second session")
		}
		said.session, said.model = event.Session, event.Model
	case event.Type == "result" && (event.Failed || event.Subtype != "success"):
		return errors.New(diagnose(event.Result))
	case event.Type == "result":
		if event.Session != said.session || said.done {
			return errors.New("the result names another session, or is the second one")
		}
		u := event.Usage
		said.result, said.done = event.Output, true
		said.usage = map[string]int64{"input_tokens": u.Input + u.Cached + u.Created, "cached_input_tokens": u.Cached, "output_tokens": u.Output}
	}
	return nil
}

// codex reads one event of `codex exec --json`. Codex reports a warning about
// the user's configuration as an item of type error, and a retry as an error
// event: neither ends the turn.
func (said *answer) codex(data []byte) error {
	var event struct {
		Type    string
		Session string `json:"thread_id"`
		Error   struct{ Message string }
		Item    struct{ Type, Message string }
		Usage   struct {
			Input     int64 `json:"input_tokens"`
			Cached    int64 `json:"cached_input_tokens"`
			Output    int64 `json:"output_tokens"`
			Reasoning int64 `json:"reasoning_output_tokens"`
		}
	}
	if json.Unmarshal(data, &event) != nil {
		return errEvent
	}
	switch event.Type {
	case "thread.started":
		if said.session != "" {
			return errors.New("the host started a second session")
		}
		said.session = event.Session
	case "turn.completed":
		u := event.Usage
		said.done = true
		said.usage = map[string]int64{"input_tokens": u.Input, "cached_input_tokens": u.Cached, "output_tokens": u.Output, "reasoning_output_tokens": u.Reasoning}
	case "turn.failed":
		return errors.New(diagnose(event.Error.Message))
	case "item.completed":
		if event.Item.Type == "error" && strings.HasPrefix(event.Item.Message, "model rerouted:") {
			return errors.New("the host used another model than the assigned one")
		}
	}
	return nil
}
