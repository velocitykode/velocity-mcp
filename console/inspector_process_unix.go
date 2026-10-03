//go:build unix

package console

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// inspectorStopSignals are the signals that end an inspector run. The first
// three are a request to stop. The quit signal is in the set so that it ends
// the processes the run started and removes the configuration file before it
// ends the command (see endAsSignalled); left to its default it would end the
// command on the spot and leave both behind.
//
// The stop and continue signals of job control are deliberately absent. The
// inspector runs in the command's own process group, so a terminal stops and
// continues it together with the command, and there is nothing to relay.
func inspectorStopSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}
}

// relayedSignal is what the inspector is told when the run is stopped by sig.
// A quit is passed on as a quit, because it means "end now, without the
// orderly shutdown"; every other stop is passed on as an interrupt, which is
// what the inspector shuts its own children down on.
func relayedSignal(sig os.Signal) syscall.Signal {
	if sig == syscall.SIGQUIT {
		return syscall.SIGQUIT
	}
	return syscall.SIGINT
}

// endAsSignalled ends the command the way sig would have ended it had the
// command not caught it, once the run has cleaned up after itself. Only the
// quit signal needs this: the others are a request to stop, which returning
// from the command answers.
//
// The signal is put back to its default action and raised again, so the
// command ends exactly as it does when nothing catches a quit. It returns
// only if the signal did not end the process, and reports that as an error
// so the caller still stops.
func endAsSignalled(sig os.Signal) error {
	if sig != syscall.SIGQUIT {
		return nil
	}
	stopped := errors.New("mcp: the MCP Inspector was stopped by a quit signal")
	signal.Reset(syscall.SIGQUIT)
	if err := syscall.Kill(os.Getpid(), syscall.SIGQUIT); err != nil {
		return stopped
	}
	// Delivery is asynchronous, and ending on a quit takes the runtime a
	// moment. The wait is only ever cut short by the process ending.
	time.Sleep(10 * time.Second)
	return stopped
}

// endedByStopSignal reports whether the inspector's exit says it was ended by
// one of the signals that stop a run: killed by the signal itself, or exited
// with the code a program reports for having been (128 plus the signal
// number, the convention shells and node follow).
func endedByStopSignal(err error) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok {
		return false
	}
	for _, sig := range inspectorStopSignals() {
		number, isNumber := sig.(syscall.Signal)
		if !isNumber {
			continue
		}
		if status.Signaled() && status.Signal() == number {
			return true
		}
		if status.Exited() && status.ExitStatus() == 128+int(number) {
			return true
		}
	}
	return false
}

// processInfo is what the run needs to know about one member of its process
// group: who it is, who started it, and whether it is still running.
type processInfo struct {
	pid  int
	ppid int
	// stopped reports a process suspended by job control.
	stopped bool
	// zombie reports a process that has exited and is waiting to be reaped;
	// it cannot be signalled and holds nothing open.
	zombie bool
}

// processGroupSnapshot is the membership of the command's process group at
// one moment. The run takes one before the inspector is started, so that the
// processes it finds there afterwards can be told apart from the ones that
// were there all along: the other members of a shell pipeline, or the script
// that started the command. A nil snapshot means the group could not be
// listed and the inspector was given a group of its own (see placeInspector).
type processGroupSnapshot map[int]struct{}

// processGroupLister lists the members of a process group. It is the
// platform's listProcessGroup, held in a variable so a test can stand in for
// a platform that cannot list one.
var processGroupLister = listProcessGroup

// snapshotProcessGroup records the current members of the command's process
// group. It returns nil where the group cannot be listed.
func snapshotProcessGroup() processGroupSnapshot {
	members, err := processGroupLister(syscall.Getpgrp())
	if err != nil {
		return nil
	}
	before := make(processGroupSnapshot, len(members))
	for _, m := range members {
		before[m.pid] = struct{}{}
	}
	return before
}

// placeInspector decides, before the inspector is started, which process
// group it runs in, and returns the snapshot the rest of the run works from.
//
// Where the command's group can be listed the inspector joins it and is
// handed the command's standard input, so a terminal treats the two as one
// job. Where it cannot, the run would have no way to find the processes the
// inspector starts among the group's other members, so the inspector is given
// a group of its own instead, which can be signalled and ended as a whole.
// Nothing started by the run outlives it either way; what is given up in the
// second case is the terminal's own handling of the job, so standard input is
// withheld there (a process outside the foreground job that read the terminal
// would be stopped rather than served) and the returned snapshot is nil.
func placeInspector(cmd *exec.Cmd) processGroupSnapshot {
	before := snapshotProcessGroup()
	if before == nil {
		cmd.Stdin = nil
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	return before
}

// signalOwnGroup sends sig to the group placeInspector gave the inspector
// when the command's group could not be listed. The group id is the
// inspector's pid; a group that no longer exists reports nothing.
func signalOwnGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	err := syscall.Kill(-cmd.Process.Pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// startedProcesses picks the live processes the inspector run started out of
// a listing of the command's process group: the inspector and everything
// descended from it that is still in the group.
//
// The inspector is started in the command's own group so that a terminal
// treats the two as one job, which means the group cannot be signalled as a
// whole without signalling the command and whatever else shares the group
// with it. The run's processes are picked out of the group instead:
//
//   - a process that was in the group before the inspector started is not one
//     of them, and neither is anything descended from such a process;
//   - a process whose line of parents within the group ends at the command is
//     one of them: the inspector while it runs, and what it started;
//   - a process whose line of parents within the group ends at an orphan is
//     one of them. When the inspector exits, the processes it started are
//     handed to the system's reaper (process 1, or a reaper outside the
//     command's session), and an orphan that appeared in the group during the
//     run is what an inspector leaves behind. It is also what any other
//     member of the group would leave behind if it started a process during
//     the run and exited before the run ended; that process is ended too,
//     which is the price of keeping the inspector in the terminal's job.
//
// A process that left the group (a new session or a group of its own) is out
// of reach here, as it was when the inspector had a group to itself.
func startedProcesses(before processGroupSnapshot, members []processInfo, self int, reaper func(pid int) bool) []int {
	byPid := make(map[int]processInfo, len(members))
	for _, m := range members {
		byPid[m.pid] = m
	}
	known := func(pid int) bool {
		_, ok := before[pid]
		return ok
	}

	var started []int
	for _, m := range members {
		if m.pid == self || m.zombie || known(m.pid) {
			continue
		}
		// Walk up to the first ancestor whose parent is the command or is
		// outside the group. The bound only guards against a listing made
		// while parents were changing.
		top, ours := m, true
		for range len(members) {
			if top.ppid == self {
				break
			}
			parent, inGroup := byPid[top.ppid]
			if !inGroup {
				break
			}
			if known(parent.pid) {
				ours = false
				break
			}
			top = parent
		}
		if !ours {
			continue
		}
		if top.ppid == self || reaper(top.ppid) {
			started = append(started, m.pid)
		}
	}
	return started
}

// isReaper reports whether pid is the process orphans are handed to rather
// than a process that started something itself: process 1, or a process in
// another session (a reaper appointed for the login session). A parent that
// cannot be looked up any more is not counted; the next listing shows who the
// process was handed to.
func isReaper(pid int) bool {
	if pid == 1 {
		return true
	}
	session, err := unix.Getsid(0)
	if err != nil {
		return false
	}
	sid, err := unix.Getsid(pid)
	if err != nil {
		return errors.Is(err, syscall.EPERM)
	}
	return sid != session
}

// listStartedProcesses lists the live processes the run started in the
// command's group.
func listStartedProcesses(before processGroupSnapshot) ([]int, error) {
	members, err := processGroupLister(syscall.Getpgrp())
	if err != nil {
		return nil, err
	}
	return startedProcesses(before, members, os.Getpid(), isReaper), nil
}

// interruptInspector passes a stop on to the inspector and to the processes
// it started, which a signal to the inspector alone does not reach. With the
// inspector in a group of its own (a nil snapshot) the group is signalled.
// Otherwise the inspector is signalled directly and the processes it started
// are picked out of the command's group; the direct signal means the stop
// gets to the inspector even if the group cannot be listed this once.
func interruptInspector(cmd *exec.Cmd, before processGroupSnapshot, sig syscall.Signal) error {
	if before == nil {
		return signalOwnGroup(cmd, sig)
	}
	err := cmd.Process.Signal(sig)
	started, _ := listStartedProcesses(before)
	for _, pid := range started {
		if pid != cmd.Process.Pid {
			_ = syscall.Kill(pid, sig)
		}
	}
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

// endStartedProcesses kills whatever the run started that is still alive once
// the inspector itself has exited, so nothing the command started outlives
// it. With the inspector in a group of its own (a nil snapshot) the group is
// killed. Otherwise the command's group is listed again after each round
// until a listing shows none of the run's processes left, because a process
// can start another between the listing and the kill, and a listing that
// fails is tried again rather than taken for an empty one. The rounds are
// bounded: a group that keeps producing processes, or that cannot be listed
// for the whole of that time, is given up on rather than waited on forever.
func endStartedProcesses(cmd *exec.Cmd, before processGroupSnapshot) {
	if before == nil {
		_ = signalOwnGroup(cmd, syscall.SIGKILL)
		return
	}
	for range 200 {
		started, err := listStartedProcesses(before)
		if err == nil && len(started) == 0 {
			return
		}
		for _, pid := range started {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// parseProcStat reads one process from the contents of a Linux
// /proc/<pid>/stat file and reports its process group alongside it. The
// second field is the command name in parentheses and may itself hold spaces
// and parentheses, so the fields after it are counted from the last closing
// parenthesis: state, parent, process group.
func parseProcStat(raw []byte) (info processInfo, pgid int, ok bool) {
	open := bytes.IndexByte(raw, '(')
	end := bytes.LastIndexByte(raw, ')')
	if open < 0 || end < open {
		return processInfo{}, 0, false
	}
	pid, err := strconv.Atoi(string(bytes.TrimSpace(raw[:open])))
	if err != nil {
		return processInfo{}, 0, false
	}
	fields := bytes.Fields(raw[end+1:])
	if len(fields) < 3 || len(fields[0]) != 1 {
		return processInfo{}, 0, false
	}
	ppid, err := strconv.Atoi(string(fields[1]))
	if err != nil {
		return processInfo{}, 0, false
	}
	pgid, err = strconv.Atoi(string(fields[2]))
	if err != nil {
		return processInfo{}, 0, false
	}
	state := fields[0][0]
	return processInfo{
		pid:     pid,
		ppid:    ppid,
		stopped: state == 'T' || state == 't',
		zombie:  state == 'Z' || state == 'X',
	}, pgid, true
}
