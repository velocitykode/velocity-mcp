//go:build unix

package console

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A --ca-cert value naming a named pipe must be refused like any other
// non-regular file, and refused without waiting: opening a pipe for reading
// blocks until a writer appears, so a command that opens before it checks the
// kind of file hangs for as long as nobody writes. The operator supplies this
// value, so the failure mode is a wedged command rather than a clear error.
func TestInspectorRejectsACertificatePipeWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("create the named pipe: %v", err)
	}

	rec := &runRecorder{}
	cmd, _, _ := newInspector(t, rec)

	done := make(chan error, 1)
	go func() {
		done <- cmd.Handle(nil, []string{"--url", "https://localhost:8443/mcp", "--ca-cert", path})
	}()

	// The check is a stat and a bounded open, so the answer is immediate; the
	// deadline only has to outlast a loaded machine, never a writer.
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a named pipe was accepted as a certificate file, want an error")
		}
		if !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("error = %v, want it to mention that the path must name a regular file", err)
		}
		if rec.calls != 0 {
			t.Fatal("the inspector was launched with a named pipe as its certificate file")
		}
	case <-deadline.C:
		// The command is blocked in the open. Become the writer it waits for,
		// so the goroutine ends with the test rather than outliving it and
		// holding the temporary directory open for the rest of the run.
		if w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close()
			<-done
		}
		t.Fatal("--ca-cert naming a named pipe blocked instead of being refused")
	}
}

// A path no process can open is refused with the same message as a path that
// names nothing: node reads the bundle, so a certificate it cannot open is as
// useless as an absent one. A symbolic link pointing at itself is such a path
// for every user, superuser included, so the refusal is exercised wherever the
// suite runs.
func TestInspectorRejectsACertificatePathThatCannotBeOpened(t *testing.T) {
	dir := t.TempDir()
	loop := filepath.Join(dir, "loop.pem")
	if err := os.Symlink("loop.pem", loop); err != nil {
		t.Fatalf("create the symbolic link: %v", err)
	}
	if _, err := os.Open(loop); err == nil {
		t.Fatal("the self-referential link opened, so this case proves nothing here")
	}

	rec := &runRecorder{}
	cmd, _, _ := newInspector(t, rec)

	err := cmd.Handle(nil, []string{"--url", "https://localhost:8443/mcp", "--ca-cert", loop})
	if err == nil {
		t.Fatal("a certificate path that cannot be opened was accepted, want an error")
	}
	if !strings.Contains(err.Error(), "readable certificate file") {
		t.Fatalf("error = %v, want it to mention that the path must name a readable certificate file", err)
	}
	if rec.calls != 0 {
		t.Fatal("the inspector was launched with a certificate path that cannot be opened")
	}
}

// Node applies NODE_TLS_REJECT_UNAUTHORIZED to every connection the process
// makes, so a value exported in the operator's shell would switch certificate
// verification off for the whole session, the requests an OAuth-protected
// session sends to its authorization server included. This command never sets
// it and must not pass it on either.
//
// The child is a shell reporting on what it was handed, so the assertion is
// made on the environment a launched process really sees: it exits non-zero
// when the environment is not the one the case describes, and the runner turns
// that into an error.
func TestInspectorRunDropsAnInheritedCertificateException(t *testing.T) {
	t.Setenv("NODE_TLS_REJECT_UNAUTHORIZED", "0")
	t.Setenv("node_tls_reject_unauthorized", "0")
	t.Setenv("NODE_OPTIONS", "--tls-min-v1.0 --insecure-http-parser")
	t.Setenv("NODE_EXTRA_CA_CERTS", "/tmp/rogue-ca.pem")
	t.Setenv("npm_config_strict_ssl", "false")
	t.Setenv("NPM_CONFIG_STRICT_SSL", "false")
	t.Setenv("npm_config_registry", "http://registry.evil.test/")
	t.Setenv("OPENSSL_CONF", "/tmp/openssl.cnf")
	t.Setenv("SSL_CERT_FILE", "/tmp/rogue-ca.pem")
	t.Setenv("DANGEROUSLY_OMIT_AUTH", "true")
	t.Setenv("MCP_PROXY_AUTH_TOKEN", "known")
	t.Setenv("ALLOWED_ORIGINS", "*")
	t.Setenv("MCP_PROXY_FULL_ADDRESS", "http://attacker.test:6277")
	t.Setenv("MCP_INSPECTOR_TEST_MARKER", "inherited")

	cases := []struct {
		name string
		// script exits zero when the child's environment is as described.
		script string
		// wantErr is for the row that proves a wrong environment is reported
		// rather than passed over.
		wantErr bool
	}{
		{name: "the inherited exception does not reach the child", script: `[ -z "${NODE_TLS_REJECT_UNAUTHORIZED+set}" ]`},
		{name: "nor does it in another spelling", script: `[ -z "${node_tls_reject_unauthorized+set}" ]`},
		{name: "node options are not inherited", script: `[ -z "${NODE_OPTIONS+set}" ]`},
		{name: "an inherited trust anchor is not inherited", script: `[ -z "${NODE_EXTRA_CA_CERTS+set}" ]`},
		{name: "npm settings are not inherited", script: `[ -z "${npm_config_strict_ssl+set}" ] && [ -z "${NPM_CONFIG_STRICT_SSL+set}" ] && [ -z "${npm_config_registry+set}" ]`},
		{name: "the OpenSSL stack settings are not inherited", script: `[ -z "${OPENSSL_CONF+set}" ] && [ -z "${SSL_CERT_FILE+set}" ]`},
		{name: "the inspector auth switches are not inherited", script: `[ -z "${DANGEROUSLY_OMIT_AUTH+set}" ] && [ -z "${MCP_PROXY_AUTH_TOKEN+set}" ] && [ -z "${ALLOWED_ORIGINS+set}" ] && [ -z "${MCP_PROXY_FULL_ADDRESS+set}" ]`},
		{name: "the rest of the environment is inherited", script: `[ "$MCP_INSPECTOR_TEST_MARKER" = inherited ]`},
		{name: "the launch overlay is applied", script: `[ "$HOST" = 127.0.0.1 ] && [ "$CLIENT_PORT" = 6274 ]`},
		{name: "a child that reports a mismatch fails the run", script: `exit 3`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runInspectorProcess(context.Background(), inspectorLaunch{
				Name: "/bin/sh",
				Args: []string{"-c", tc.script},
				Env:  map[string]string{"HOST": "127.0.0.1", "CLIENT_PORT": "6274"},
			})
			if tc.wantErr {
				if err == nil {
					t.Fatal("a child that exited non-zero was reported as a successful run")
				}
				return
			}
			if err != nil {
				t.Fatalf("the child was handed an environment it reported as wrong: %v", err)
			}
		})
	}
}

// waitForProcessExit blocks until no process with the given pid exists any
// more, polling rather than sleeping for a fixed time, and fails the test when
// it is still there at the deadline. A zombie counts as present: it is gone
// once its parent, or init after the parent died, has reaped it.
func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("process %d outlived the inspector run", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The inspector is npx, which starts node processes of its own to serve the UI
// and the proxy, and those are what hold the ports. Nothing started by the run
// may outlive it: not when the inspector honours the interrupt and leaves a
// child behind, not when it ignores the interrupt and has to be killed, and
// not when it exits on its own with a child still running. The child here is a
// shell standing in for npx; the grandchild it starts ignores every signal it
// can, writes its pid where the test can read it, and would otherwise live on
// for minutes.
func TestInspectorRunEndsEveryProcessItStarted(t *testing.T) {
	grace := inspectorShutdownGrace
	inspectorShutdownGrace = 200 * time.Millisecond
	t.Cleanup(func() { inspectorShutdownGrace = grace })

	const grandchild = `sh -c 'trap "" INT TERM; echo $$ > "$PIDFILE"; exec sleep 300' &`
	cases := []struct {
		name string
		// child is the script standing in for the inspector. Every one starts
		// the grandchild first.
		child string
		// cancel reports whether the run is interrupted once the grandchild
		// has reported itself; otherwise the child is left to exit on its own.
		cancel bool
	}{
		{name: "the inspector honours the interrupt", child: grandchild + ` wait`, cancel: true},
		{name: "the inspector ignores the interrupt", child: `trap "" INT; ` + grandchild + ` wait`, cancel: true},
		// The child waits until the grandchild has reported itself before
		// exiting, or the run would end the group before there is a pid to
		// read.
		{name: "the inspector exits on its own", child: grandchild + ` until [ -s "$PIDFILE" ]; do sleep 0.01; done; exit 0`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pid")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			done := make(chan error, 1)
			go func() {
				done <- runInspectorProcess(ctx, inspectorLaunch{
					Name: "/bin/sh",
					Args: []string{"-c", tc.child},
					Env:  map[string]string{"PIDFILE": pidFile},
				})
			}()

			pid := waitForPid(t, pidFile)
			if tc.cancel {
				cancel()
			}
			err := <-done
			if err != nil {
				t.Fatalf("run reported %v, want a clean end", err)
			}
			waitForProcessExit(t, pid)
		})
	}
}

// waitForContent blocks until the file at path holds something and returns it.
// A shell creates the file a redirection names before the command writing to
// it has run, so the file existing says nothing about its content; only a
// non-empty read does.
func waitForContent(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if raw, err := os.ReadFile(path); err == nil {
			if content := strings.TrimSpace(string(raw)); content != "" {
				return content
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing was ever written to %s", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForPid blocks until a process has reported its pid in the file at path.
func waitForPid(t *testing.T, path string) int {
	t.Helper()
	content := waitForContent(t, path)
	pid, err := strconv.Atoi(content)
	if err != nil || pid <= 0 {
		t.Fatalf("pid file %s holds %q", path, content)
	}
	return pid
}

// The environment variable that turns TestInspectorCommandProcess into the
// command under test, and the one carrying its shutdown grace period.
const (
	commandProcessEnv      = "MCP_INSPECTOR_TEST_COMMAND_PROCESS"
	commandProcessGraceEnv = "MCP_INSPECTOR_TEST_GRACE"
	// commandProcessNoListingEnv makes the command behave as on a platform
	// where a process group cannot be listed.
	commandProcessNoListingEnv = "MCP_INSPECTOR_TEST_NO_LISTING"
)

// TestInspectorCommandProcess is the command the terminal tests below drive:
// re-executed by startInspectorCommand, it runs mcp:inspector as an operator's
// shell would have started it, with the real process runner. On its own it
// does nothing.
func TestInspectorCommandProcess(t *testing.T) {
	if os.Getenv(commandProcessEnv) == "" {
		return
	}
	grace, err := time.ParseDuration(os.Getenv(commandProcessGraceEnv))
	if err != nil {
		os.Exit(90)
	}
	inspectorShutdownGrace = grace
	if os.Getenv(commandProcessNoListingEnv) != "" {
		processGroupLister = func(int) ([]processInfo, error) {
			return nil, errors.New("no process table on this platform")
		}
	}
	cmd := inspectorCommand{
		srv:    inventoryServer(),
		out:    io.Discard,
		binary: func() (string, error) { return "/projects/demo/vel", nil },
	}
	if err := cmd.Handle(nil, nil); err != nil {
		os.Exit(91)
	}
	os.Exit(0)
}

// inspectorJob is an mcp:inspector command running as a shell job: the leader
// of a process group of its own, with a stand-in for npx as its child and the
// process that child started as its grandchild.
type inspectorJob struct {
	cmd *exec.Cmd
	dir string
	// done receives the command's exit, once.
	done chan error
}

// startInspectorCommand starts mcp:inspector the way an interactive shell
// starts a command: as the leader of a new process group, which is the
// terminal's foreground job. The npx on its PATH is a script that records the
// configuration file it was handed and its own pid, then runs npx, the shell
// text of the case. Files are reported under the DIR the script is given, and
// env is added to the command's environment.
func startInspectorCommand(t *testing.T, npx string, stdin io.Reader, env ...string) *inspectorJob {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatalf("create the bin directory: %v", err)
	}
	script := "#!/bin/sh\nprintf '%s' \"$3\" > \"$DIR/config\"\necho $$ > \"$DIR/child\"\n" + npx + "\n"
	if err := os.WriteFile(filepath.Join(bin, "npx"), []byte(script), 0o755); err != nil {
		t.Fatalf("write the npx stand-in: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestInspectorCommandProcess$")
	cmd.Env = append(os.Environ(),
		commandProcessEnv+"=1",
		commandProcessGraceEnv+"=200ms",
		"DIR="+dir,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin = stdin
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the command: %v", err)
	}
	job := &inspectorJob{cmd: cmd, dir: dir, done: make(chan error, 1)}
	go func() { job.done <- cmd.Wait() }()
	t.Cleanup(func() {
		// Whatever the case left running is ended with the test: the job,
		// and the processes that may have been moved out of it.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		for _, name := range []string{"child", "grandchild"} {
			if raw, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 1 {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}
		}
	})
	return job
}

// signalJob sends sig to every process in the job, which is what a terminal
// does to its foreground job when the operator presses a signal key.
func (j *inspectorJob) signalJob(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if err := syscall.Kill(-j.cmd.Process.Pid, sig); err != nil {
		t.Fatalf("signal the job with %v: %v", sig, err)
	}
}

// waitForExit blocks until the command has exited and returns its exit code.
func (j *inspectorJob) waitForExit(t *testing.T) int {
	t.Helper()
	select {
	case <-j.done:
		return j.cmd.ProcessState.ExitCode()
	case <-time.After(60 * time.Second):
		t.Fatal("the command never exited")
		return -1
	}
}

// waitForJobState blocks until every one of pids is a member of the job and
// its stopped state is want, polling the process table.
func (j *inspectorJob) waitForJobState(t *testing.T, want bool, pids ...int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		members, err := listProcessGroup(j.cmd.Process.Pid)
		if err != nil {
			t.Fatalf("list the job: %v", err)
		}
		var pending []int
		for _, pid := range pids {
			i := slices.IndexFunc(members, func(m processInfo) bool { return m.pid == pid })
			if i < 0 || members[i].stopped != want {
				pending = append(pending, pid)
			}
		}
		if len(pending) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("processes %v never became members of the job with stopped=%v (job: %+v)", pending, want, members)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// grandchildScript starts the process a stand-in npx leaves behind. It ignores
// every signal named in ignore, reports its pid, and then waits for good on a
// pipe nobody writes to. A shell also makes a background command ignore the
// interrupt and quit signals, so it survives both of those whatever ignore
// says.
//
// It stays the shell it started as. Replacing itself with another program
// after reporting would leave a moment in which a stop signal is lost on it,
// and a test that signals as soon as the pid is written would land there.
func grandchildScript(ignore string) string {
	return `mkfifo "$DIR/hold"; sh -c 'trap "" ` + ignore + `; echo $$ > "$DIR/grandchild"; while :; do read _ < "$DIR/hold"; done' &`
}

// A terminal signals its foreground job as a whole, and mcp:inspector has to
// behave under those signals like the single job it looks like: the interrupt
// and quit keys end the command and everything the inspector started, and so
// does a signal that reaches the command alone. Whatever ends the run, no
// process it started is left running and the configuration file, which can
// hold a url with credentials, is removed.
func TestInspectorJobEndsWholeOnASignal(t *testing.T) {
	cases := []struct {
		name string
		// npx is the script standing in for the inspector.
		npx string
		// sig is sent to the whole job, as a signal key does, or to the
		// command alone when commandOnly is set.
		sig         syscall.Signal
		commandOnly bool
		// wantExit is the exit code of the command.
		wantExit int
	}{
		{name: "the interrupt key", npx: grandchildScript("INT TERM") + ` wait`, sig: syscall.SIGINT},
		{name: "the interrupt key, ignored by the inspector", npx: `trap "" INT; ` + grandchildScript("INT TERM") + ` wait`, sig: syscall.SIGINT},
		// A quit ends a Go program with exit code 2.
		{name: "the quit key", npx: grandchildScript("INT TERM") + ` wait`, sig: syscall.SIGQUIT, wantExit: 2},
		{name: "the quit key, ignored by the inspector", npx: `trap "" QUIT INT; ` + grandchildScript("INT TERM QUIT HUP") + ` wait`, sig: syscall.SIGQUIT, wantExit: 2},
		{name: "a hangup of the terminal", npx: grandchildScript("INT TERM HUP") + ` wait`, sig: syscall.SIGHUP},
		{name: "a terminate sent to the command alone", npx: grandchildScript("INT TERM") + ` wait`, sig: syscall.SIGTERM, commandOnly: true},
		{name: "an interrupt sent to the command alone", npx: grandchildScript("INT TERM") + ` wait`, sig: syscall.SIGINT, commandOnly: true},
		{name: "a quit sent to the command alone", npx: grandchildScript("INT TERM QUIT") + ` wait`, sig: syscall.SIGQUIT, commandOnly: true, wantExit: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := startInspectorCommand(t, tc.npx, nil)
			child := waitForPid(t, filepath.Join(job.dir, "child"))
			grandchild := waitForPid(t, filepath.Join(job.dir, "grandchild"))
			config := waitForContent(t, filepath.Join(job.dir, "config"))
			if _, err := os.Stat(config); err != nil {
				t.Fatalf("the configuration file is not there during the run: %v", err)
			}

			if tc.commandOnly {
				if err := syscall.Kill(job.cmd.Process.Pid, tc.sig); err != nil {
					t.Fatalf("signal the command: %v", err)
				}
			} else {
				job.signalJob(t, tc.sig)
			}

			if code := job.waitForExit(t); code != tc.wantExit {
				t.Fatalf("the command exited with code %d, want %d", code, tc.wantExit)
			}
			// The command is gone, so these are checked at once: a process
			// still running now has outlived the run.
			for name, pid := range map[string]int{"inspector": child, "process the inspector started": grandchild} {
				if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
					waitForZombieReaped(t, pid, name)
				}
			}
			if _, err := os.Stat(config); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the configuration file outlived the run (stat: %v)", err)
			}
		})
	}
}

// waitForZombieReaped accepts a process that has been killed and is only
// waiting for the system's reaper to collect it, and fails the test for one
// that is still running: the run ended it or it did not, and a process that
// was merely slow to die was not ended by the run.
func waitForZombieReaped(t *testing.T, pid int, name string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		pgid, err := syscall.Getpgid(pid)
		if err != nil {
			return
		}
		members, err := listProcessGroup(pgid)
		if err != nil {
			t.Fatalf("list the process group of the %s: %v", name, err)
		}
		i := slices.IndexFunc(members, func(m processInfo) bool { return m.pid == pid })
		if i >= 0 && !members[i].zombie {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the %s (pid %d) outlived the command", name, pid)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the %s (pid %d) was never reaped", name, pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The suspend key stops the foreground job and the shell's fg continues it.
// The inspector and what it started have to stop and continue with the
// command: a command stopped on its own would leave the inspector serving a
// terminal the operator has taken back.
func TestInspectorJobSuspendsAndResumesWhole(t *testing.T) {
	job := startInspectorCommand(t, grandchildScript("INT TERM")+` wait`, nil)
	command := job.cmd.Process.Pid
	child := waitForPid(t, filepath.Join(job.dir, "child"))
	grandchild := waitForPid(t, filepath.Join(job.dir, "grandchild"))

	job.signalJob(t, syscall.SIGTSTP)
	job.waitForJobState(t, true, command, child, grandchild)

	job.signalJob(t, syscall.SIGCONT)
	job.waitForJobState(t, false, command, child, grandchild)

	job.signalJob(t, syscall.SIGINT)
	if code := job.waitForExit(t); code != 0 {
		t.Fatalf("the resumed command exited with code %d on an interrupt, want 0", code)
	}
	waitForProcessExit(t, child)
	waitForProcessExit(t, grandchild)
}

// npx asks on the terminal before it installs the pinned package for the
// first time, so the inspector has to be handed the command's standard input
// and has to be in the command's process group: a terminal serves reads from
// its foreground job only, and stops any other process that tries.
func TestInspectorReadsTheCommandsStandardInput(t *testing.T) {
	job := startInspectorCommand(t, `read answer; printf '%s' "$answer" > "$DIR/answer"; `+grandchildScript("INT TERM")+` wait`, strings.NewReader("y\n"))
	child := waitForPid(t, filepath.Join(job.dir, "child"))
	grandchild := waitForPid(t, filepath.Join(job.dir, "grandchild"))

	if answer := waitForContent(t, filepath.Join(job.dir, "answer")); answer != "y" {
		t.Fatalf("the inspector read %q from standard input, want the answer typed to the command", answer)
	}
	for name, pid := range map[string]int{"inspector": child, "process the inspector started": grandchild} {
		pgid, err := syscall.Getpgid(pid)
		if err != nil {
			t.Fatalf("process group of the %s: %v", name, err)
		}
		if pgid != job.cmd.Process.Pid {
			t.Fatalf("the %s is in process group %d, want the command's group %d", name, pgid, job.cmd.Process.Pid)
		}
	}

	job.signalJob(t, syscall.SIGINT)
	if code := job.waitForExit(t); code != 0 {
		t.Fatalf("the command exited with code %d on an interrupt, want 0", code)
	}
	waitForProcessExit(t, child)
	waitForProcessExit(t, grandchild)
}

// Picking the run's processes out of the command's group depends on listing
// the group. On a platform where that cannot be done the run must not fall
// back to ending the inspector alone, which is the defect this command once
// had: the inspector is given a process group of its own there, and that
// group is signalled and ended as a whole.
func TestInspectorWithoutAProcessListingEndsAGroupOfItsOwn(t *testing.T) {
	// The inspector stand-in that exits on its own waits for the process it
	// started to report itself first, or there would be no pid to check.
	const exitsOnItsOwn = ` until [ -s "$DIR/grandchild" ]; do sleep 0.01; done; exit 0`
	cases := []struct {
		name string
		npx  string
		// sig is sent to the command alone; zero leaves the inspector to exit
		// by itself.
		sig      syscall.Signal
		wantExit int
	}{
		{name: "a terminate sent to the command", npx: grandchildScript("INT TERM") + ` wait`, sig: syscall.SIGTERM},
		{name: "a terminate the inspector ignores", npx: `trap "" INT; ` + grandchildScript("INT TERM") + ` wait`, sig: syscall.SIGTERM},
		{name: "a quit sent to the command", npx: grandchildScript("INT TERM QUIT") + ` wait`, sig: syscall.SIGQUIT, wantExit: 2},
		{name: "the inspector exits on its own", npx: grandchildScript("INT TERM") + exitsOnItsOwn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := startInspectorCommand(t, tc.npx, nil, commandProcessNoListingEnv+"=1")
			child := waitForPid(t, filepath.Join(job.dir, "child"))
			grandchild := waitForPid(t, filepath.Join(job.dir, "grandchild"))
			config := waitForContent(t, filepath.Join(job.dir, "config"))

			// Read while the inspector is certain to be running; the case
			// that exits on its own may be gone by now, and its group is not
			// what that case is about.
			if tc.sig != 0 {
				pgid, err := syscall.Getpgid(child)
				if err != nil {
					t.Fatalf("process group of the inspector: %v", err)
				}
				if pgid != child {
					t.Fatalf("the inspector is in process group %d, want a group of its own (%d)", pgid, child)
				}
				if err := syscall.Kill(job.cmd.Process.Pid, tc.sig); err != nil {
					t.Fatalf("signal the command: %v", err)
				}
			}

			if code := job.waitForExit(t); code != tc.wantExit {
				t.Fatalf("the command exited with code %d, want %d", code, tc.wantExit)
			}
			for name, pid := range map[string]int{"inspector": child, "process the inspector started": grandchild} {
				if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
					waitForZombieReaped(t, pid, name)
				}
			}
			if _, err := os.Stat(config); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the configuration file outlived the run (stat: %v)", err)
			}
		})
	}
}

// A listing of the group that fails once the run is under way is not an empty
// listing: the run tries again rather than conclude there is nothing left to
// end.
func TestInspectorRunRetriesAProcessListingThatFails(t *testing.T) {
	real := processGroupLister
	t.Cleanup(func() { processGroupLister = real })
	var calls int
	processGroupLister = func(pgid int) ([]processInfo, error) {
		calls++
		// The first call is the snapshot taken before the inspector starts;
		// the next few, made once it has exited, fail.
		if calls > 1 && calls <= 4 {
			return nil, errors.New("the process table is busy")
		}
		return real(pgid)
	}

	pidFile := filepath.Join(t.TempDir(), "pid")
	err := runInspectorProcess(context.Background(), inspectorLaunch{
		Name: "/bin/sh",
		Args: []string{"-c", `sh -c 'trap "" INT TERM; echo $$ > "$PIDFILE"; exec sleep 300' & until [ -s "$PIDFILE" ]; do sleep 0.01; done; exit 0`},
		Env:  map[string]string{"PIDFILE": pidFile},
	})
	if err != nil {
		t.Fatalf("run reported %v, want a clean end", err)
	}
	pid := waitForPid(t, pidFile)
	if calls <= 4 {
		t.Fatalf("the group was listed %d times, so the failing listings were never retried", calls)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		waitForZombieReaped(t, pid, "process the inspector started")
	}
}

// A signal sent to the whole job can end the inspector before the command has
// been handed its own copy of it. The run then waits a moment for that copy
// and ends as stopped; it reports a failed inspector only when no stop
// follows, which is an inspector that was signalled on its own.
func TestInspectorRunWaitsForTheStopThatEndedTheInspector(t *testing.T) {
	arrival := inspectorSignalArrival
	t.Cleanup(func() { inspectorSignalArrival = arrival })

	cases := []struct {
		name string
		// child is the script standing in for the inspector; it ends itself.
		child string
		// stop is the signal the command is handed after the inspector has
		// already exited; nil means none ever comes.
		stop os.Signal
		// wantErr is part of the error the run reports; empty means it ends
		// as stopped.
		wantErr string
	}{
		{name: "killed by an interrupt, the command's copy follows", child: `kill -INT $$; sleep 300`, stop: os.Interrupt},
		{name: "killed by a quit, the command's copy follows", child: `kill -QUIT $$; sleep 300`, stop: syscall.SIGQUIT},
		{name: "killed by a terminate, the command's copy follows", child: `kill -TERM $$; sleep 300`, stop: syscall.SIGTERM},
		{name: "killed by a hangup, the command's copy follows", child: `kill -HUP $$; sleep 300`, stop: syscall.SIGHUP},
		{name: "exited with the code for an interrupt, the command's copy follows", child: `exit 130`, stop: os.Interrupt},
		{name: "killed by an interrupt nobody sent the command", child: `kill -INT $$; sleep 300`, wantErr: "signal: interrupt"},
		{name: "exited with the code for a quit nobody sent the command", child: `exit 131`, wantErr: "exit status 131"},
		{name: "failed on its own", child: `exit 3`, wantErr: "exit status 3"},
		{name: "killed by a signal that is not a stop", child: `kill -USR1 $$; sleep 300`, wantErr: "signal: user defined signal 1"},
	}
	real := processGroupLister
	t.Cleanup(func() { processGroupLister = real })

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A stop that follows is given a long time to arrive, so the case
			// does not depend on how fast the machine is; a stop that never
			// comes is not waited on for long.
			inspectorSignalArrival = 30 * time.Second
			if tc.stop == nil {
				inspectorSignalArrival = 50 * time.Millisecond
			}

			// The group is listed once before the inspector starts and again
			// once it has exited, so the second listing marks the moment the
			// run knows the inspector is gone. The stop is delivered after
			// it, which is the order that used to be reported as a failure.
			var listings int
			exited := make(chan struct{})
			processGroupLister = func(pgid int) ([]processInfo, error) {
				members, err := real(pgid)
				if listings++; listings == 2 {
					close(exited)
				}
				return members, err
			}

			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			done := make(chan error, 1)
			go func() {
				done <- runInspectorProcess(ctx, inspectorLaunch{Name: "/bin/sh", Args: []string{"-c", tc.child}})
			}()

			if tc.stop != nil {
				<-exited
				cancel(stoppedBySignal{sig: tc.stop})
			}
			err := <-done
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("run reported %v, want it to end as stopped", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("run reported %v, want an error saying %q", err, tc.wantErr)
			}
		})
	}
}

// The run shares its process group with whatever else the shell put there, so
// the processes it started are picked out of the group rather than the group
// being signalled as a whole. Nothing that was there before the inspector
// started, and nothing descended from such a process, may be taken for one of
// the run's own.
func TestStartedProcessesPicksOnlyTheRunsOwn(t *testing.T) {
	const (
		self   = 100
		shell  = 50  // outside the group, in the command's session
		tee    = 101 // a pipeline sibling, there before the run
		reaper = 1
	)
	before := processGroupSnapshot{self: {}, tee: {}}
	isReaper := func(pid int) bool { return pid == reaper }

	cases := []struct {
		name    string
		members []processInfo
		want    []int
	}{
		{name: "nothing but what was there before", members: []processInfo{{pid: self, ppid: shell}, {pid: tee, ppid: shell}}},
		{
			name:    "the running inspector and what it started",
			members: []processInfo{{pid: self, ppid: shell}, {pid: 200, ppid: self}, {pid: 201, ppid: 200}, {pid: 202, ppid: 201}},
			want:    []int{200, 201, 202},
		},
		{
			name:    "what an exited inspector left behind, and its children",
			members: []processInfo{{pid: self, ppid: shell}, {pid: 201, ppid: reaper}, {pid: 202, ppid: 201}},
			want:    []int{201, 202},
		},
		{
			name:    "a child whose orphaned parent is waiting to be reaped",
			members: []processInfo{{pid: self, ppid: shell}, {pid: 201, ppid: reaper, zombie: true}, {pid: 202, ppid: 201}},
			want:    []int{202},
		},
		{
			name:    "a process a pipeline sibling started during the run",
			members: []processInfo{{pid: self, ppid: shell}, {pid: tee, ppid: shell}, {pid: 300, ppid: tee}, {pid: 301, ppid: 300}},
		},
		{
			name:    "a process the shell added to the job during the run",
			members: []processInfo{{pid: self, ppid: shell}, {pid: 400, ppid: shell}, {pid: 401, ppid: 400}},
		},
		{
			name:    "a parent that is gone from the listing and not yet replaced",
			members: []processInfo{{pid: self, ppid: shell}, {pid: 500, ppid: 499}},
		},
		{
			name:    "a listing in which two processes name each other as parent",
			members: []processInfo{{pid: self, ppid: shell}, {pid: 600, ppid: 601}, {pid: 601, ppid: 600}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := startedProcesses(before, tc.members, self, isReaper)
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("started processes = %v, want %v", got, tc.want)
			}
		})
	}
}

// The Linux process table is read from /proc/<pid>/stat, whose command field
// is free text in parentheses.
func TestParseProcStat(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		want     processInfo
		wantPgid int
		wantOK   bool
	}{
		{name: "a running process", raw: "4242 (node) S 4100 4000 3900 34816 4000 0 0", want: processInfo{pid: 4242, ppid: 4100}, wantPgid: 4000, wantOK: true},
		{name: "a stopped process", raw: "4242 (node) T 4100 4000 3900 34816", want: processInfo{pid: 4242, ppid: 4100, stopped: true}, wantPgid: 4000, wantOK: true},
		{name: "a zombie", raw: "4242 (node) Z 1 4000 3900 0", want: processInfo{pid: 4242, ppid: 1, zombie: true}, wantPgid: 4000, wantOK: true},
		{name: "a command name with spaces and parentheses", raw: "77 (a) S (b) R 9 9 9) R 12 34 56 0", want: processInfo{pid: 77, ppid: 12}, wantPgid: 34, wantOK: true},
		{name: "a truncated line", raw: "4242 (node) S 4100"},
		{name: "no command field", raw: "4242 node S 4100 4000 3900"},
		{name: "a parent that is not a number", raw: "4242 (node) S x 4000 3900"},
		{name: "an empty file", raw: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, pgid, ok := parseProcStat([]byte(tc.raw))
			if ok != tc.wantOK || got != tc.want || pgid != tc.wantPgid {
				t.Fatalf("parseProcStat = %+v, group %d, ok %v; want %+v, group %d, ok %v", got, pgid, ok, tc.want, tc.wantPgid, tc.wantOK)
			}
		})
	}
}
