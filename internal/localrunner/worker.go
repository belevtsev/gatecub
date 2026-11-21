package localrunner

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/belevtsev/kitsu/v3/internal/onboarding"
	"github.com/belevtsev/kitsu/v3/internal/proc"
	"github.com/belevtsev/kitsu/v3/internal/workflowrunner"
)

const (
	answerLimit = 64 << 10 // the structured result of a worker
	eventLimit  = 4 << 20  // one line of a host's event stream
	// A host may still be writing its own session record when it has answered.
	drain = 2 * time.Second
)

var sessionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// answer is what the host reported about one worker.
type answer struct {
	session, model string
	result         json.RawMessage
	usage          map[string]int64
	done           bool
}

// work starts the agent of a role job in tree, the private copy, as a fresh
// headless process of its host, and puts into got what the host reported.
// For a role that may write, got also holds the files the agent changed.
func (a attempt) work(ctx context.Context, dir, tree string, files []workflowrunner.File, got *workflowrunner.Receipt) error {
	job := a.Job
	program, native := onboarding.HostExecutable(job.Host)
	if job.Host != "codex" && job.Host != "claude" || !native {
		return fmt.Errorf("the role %s needs the program %q on an absolute PATH entry", job.Role, job.Host)
	}
	if strings.TrimSpace(job.Prompt) == "" || !object(job.Schema) {
		return errors.New("the job has no assignment or no schema for its result")
	}
	said := &answer{}
	args, parse, finish, err := launch(job, dir, tree, said)
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := &stream{parse: parse, cancel: cancel}
	ended := proc.Run(runCtx, proc.Spec{Path: program, Args: args, Dir: tree, Env: workerEnvironment(),
		Stdin: strings.NewReader(job.Prompt), Stdout: events, StderrLimit: 8 << 10, WaitDelay: drain})
	events.consume()
	// The supervisor's log keeps what the host said.
	fmt.Fprintf(os.Stderr, "%s\n%s: status %d, %v\n", ended.Stderr, job.Host, ended.Code, ended.Err)
	switch {
	case ended.Cleanup != nil:
		return fmt.Errorf("%w: %s: %v", errAlive, job.Host, ended.Cleanup)
	case ctx.Err() != nil:
		return ctx.Err()
	case events.err != nil:
		return fmt.Errorf("%s: %w", job.Host, events.err)
	case ended.Code != 0:
		return fmt.Errorf("%s ended with status %d: %s. Its output is in %s", job.Host, ended.Code,
			diagnose(string(ended.Stderr)), filepath.Join(dir, "supervisor.log"))
	}
	if err := finish(); err != nil {
		return fmt.Errorf("%s: %w", job.Host, err)
	}
	if !said.done || !sessionID.MatchString(said.session) || !object(said.result) || len(said.result) > answerLimit {
		return fmt.Errorf("%s did not report one session with a completed, structured result", job.Host)
	}
	got.Host, got.Role, got.AgentType, got.Session = job.Host, job.Role, "kitsu-"+job.Role, said.session
	// Claude Code names its model. Codex names none, and neither host reports
	// its effort: there the receipt holds what the host was started with.
	got.Model, got.Effort = cmp.Or(said.model, job.Model, "default"), job.Effort
	got.Usage, got.Result = said.usage, said.result
	if !job.ReadOnly {
		got.Changes, err = changes(ctx, a.Project, tree, files)
	}
	return err
}

func object(data []byte) bool {
	data = bytes.TrimSpace(data)
	return len(data) > 0 && data[0] == '{' && json.Valid(data)
}

// diagnose names the kind of failure a host reported, without repeating what
// a model or a tool wrote.
func diagnose(text string) string {
	text = strings.ToLower(text)
	for _, kind := range [][2]string{
		{"not logged in", "the host is not signed in"}, {"authentication", "the host is not signed in"},
		{"unauthorized", "the host is not signed in"}, {"/login", "the host is not signed in"},
		{"sandbox", "the host's permissions blocked the assignment"}, {"permission", "the host's permissions blocked the assignment"},
		{"quota", "a limit of the host was reached"}, {"limit", "a limit of the host was reached"},
	} {
		if strings.Contains(text, kind[0]) {
			return kind[1]
		}
	}
	return "the host did not complete the assignment"
}

// passed are the variables a worker inherits: where the user and their
// programs are, how to reach the network and how the host signs in. Anything
// else in the developer's environment, such as the token of another service
// or the variables of a chat that started the run, stays out.
var passed = []string{
	"HOME", "PATH", "USER", "LOGNAME", "SHELL", "TERM", "LANG", "TZ", "TMPDIR", "TMP", "TEMP",
	"USERPROFILE", "USERNAME", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA", "PROGRAMDATA",
	"SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "PATHEXT",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
	"CODEX_HOME", "CODEX_API_KEY", "OPENAI_API_KEY", "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY",
}

func workerEnvironment() []string {
	return slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(strings.ToUpper(kv), "=")
		return !slices.Contains(passed, name) && !strings.HasPrefix(name, "LC_") && !strings.HasPrefix(name, "XDG_")
	})
}

// stream is the standard output of a host: one JSON event on each line. The
// first event that parse refuses stops the host.
type stream struct {
	line   []byte
	parse  func([]byte) error
	cancel context.CancelFunc
	err    error
}

func (s *stream) Write(p []byte) (int, error) {
	for rest := p; len(rest) > 0 && s.err == nil; {
		before, after, complete := bytes.Cut(rest, []byte{'\n'})
		if len(s.line)+len(before) > eventLimit {
			s.err = errors.New("an event of the host is larger than 4 MiB")
			s.cancel()
			break
		}
		s.line = append(s.line, before...)
		if complete {
			s.consume()
		}
		rest = after
	}
	return len(p), nil
}

func (s *stream) consume() {
	if line := bytes.TrimSpace(s.line); len(line) != 0 && s.err == nil {
		if s.err = s.parse(line); s.err != nil {
			s.cancel()
		}
	}
	s.line = s.line[:0]
}

// changes are the files of the private copy that differ from the candidate
// the worker was given: a regular file that is new or has other bytes or
// another mode, and a file that is gone. A new file that git ignores in the
// project is build output and is left out: the import refuses a result with
// one, by the same rules. The repository of the copy is asked nothing, because
// the worker may have committed in it or changed its settings.
func changes(ctx context.Context, project, tree string, before []workflowrunner.File) ([]workflowrunner.File, error) {
	root, err := os.OpenRoot(tree)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	d := difference{root: root, known: make(map[string]workflowrunner.File, len(before))}
	for _, file := range before {
		d.known[file.Path] = file
	}
	var fresh []string
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		switch old, existed := d.known[name]; {
		case err != nil:
			return err
		case name == ".git" && entry.IsDir():
			return fs.SkipDir // Anything else of that name is a change, which the import refuses.
		case entry.IsDir():
			return nil
		case !existed:
			fresh = append(fresh, name)
			return nil
		default:
			return d.read(name, old.Mode)
		}
	})
	if err == nil {
		fresh, err = unignored(ctx, project, fresh)
	}
	for _, name := range fresh {
		if err == nil {
			err = d.read(name, "")
		}
	}
	for name := range d.known {
		d.out = append(d.out, workflowrunner.File{Path: name})
	}
	if err == nil && d.size > workflowrunner.MaxBundle {
		err = errors.New("the worker changed more than the 16 MiB a job may return")
	}
	slices.SortFunc(d.out, func(a, b workflowrunner.File) int { return strings.Compare(a.Path, b.Path) })
	return d.out, err
}

// difference collects the changes of one private copy. known holds the
// candidate's files that were not yet found in the copy.
type difference struct {
	root  *os.Root
	known map[string]workflowrunner.File
	out   []workflowrunner.File
	size  int
}

// read adds the file at name unless it is the candidate's own. mode is the
// candidate's mode of that file, or nothing for a new one.
func (d *difference) read(name, mode string) error {
	info, err := d.root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() > workflowrunner.MaxBundle {
		return errors.Join(err, fmt.Errorf("the worker left %q, which is not a regular file of the bundle's size", name))
	}
	data, err := d.root.ReadFile(name)
	if runtime.GOOS != "windows" || mode == "" { // Windows keeps no executable bit.
		mode = "100644"
		if info.Mode()&0o111 != 0 {
			mode = "100755"
		}
	}
	if old := d.known[name]; err == nil && (mode != old.Mode || !bytes.Equal(data, old.Data)) {
		d.out = append(d.out, workflowrunner.File{Path: name, Mode: mode, Data: data})
		d.size += len(data)
	}
	delete(d.known, name)
	return err
}

// unignored drops the names that git ignores in the repository at dir.
func unignored(ctx context.Context, dir string, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	ended := proc.Run(ctx, proc.Spec{Path: "git", Args: []string{"check-ignore", "--no-index", "-z", "--stdin"}, Dir: dir,
		Env: environment(), Stdin: strings.NewReader(strings.Join(names, "\x00") + "\x00"), StdoutLimit: 32 << 20, StderrLimit: 4 << 10})
	if ended.Code != 0 && ended.Code != 1 || ended.Overflow { // 1: none of them is ignored
		return nil, fmt.Errorf("git check-ignore: %s %v", bytes.TrimSpace(ended.Stderr), ended.Err)
	}
	ignored := map[string]bool{}
	for name := range strings.SplitSeq(string(ended.Stdout), "\x00") {
		ignored[name] = true
	}
	return slices.DeleteFunc(names, func(name string) bool { return ignored[name] }), nil
}
