//go:build unix

package console

import (
	"context"
	"os"
	"path/filepath"
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
