//go:build !unix

package console

import (
	"os"
	"os/exec"
	"syscall"
)

// inspectorStopSignals are the signals that end an inspector run.
func inspectorStopSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
}

// relayedSignal is what the inspector is told when the run is stopped.
func relayedSignal(os.Signal) os.Signal { return os.Interrupt }

// endAsSignalled has nothing to do where no stop signal has to end the
// command itself.
func endAsSignalled(os.Signal) error { return nil }

// endedByStopSignal reports nothing where a process does not end by a signal
// sent to its job.
func endedByStopSignal(error) bool { return false }

// processGroupSnapshot is empty where process groups are not available.
type processGroupSnapshot struct{}

// placeInspector has nothing to decide where process groups are not
// available: the inspector is a plain child of the command.
func placeInspector(*exec.Cmd) processGroupSnapshot { return processGroupSnapshot{} }

// interruptInspector interrupts the inspector process itself.
func interruptInspector(cmd *exec.Cmd, _ processGroupSnapshot, sig os.Signal) error {
	return cmd.Process.Signal(sig)
}

// endStartedProcesses is a no-op where process groups are not available: the
// inspector itself has already exited when it is called.
func endStartedProcesses(*exec.Cmd, processGroupSnapshot) {}
