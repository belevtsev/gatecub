package proc

import "context"

// Detach starts the program outside the terminal session of kitsu and neither
// waits for it nor stops it: the program outlives the command that started
// it, and a terminal's interrupt does not reach it. Give it files for its
// output, as a pipe ends with this command. The returned channel closes when
// the program exits while this command still runs. The local runner starts
// the supervisor of a job this way.
func Detach(spec Spec) (<-chan struct{}, error) {
	command, _ := prepare(context.Background(), spec)
	command.SysProcAttr = detached()
	if err := command.Start(); err != nil {
		return nil, err
	}
	exited := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(exited)
	}()
	return exited, nil
}
