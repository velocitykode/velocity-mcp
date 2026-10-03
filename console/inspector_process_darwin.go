//go:build darwin

package console

import "golang.org/x/sys/unix"

// Process states the kernel reports for a member of a process group.
const (
	darwinProcessStopped = 4 // SSTOP
	darwinProcessZombie  = 5 // SZOMB
)

// listProcessGroup lists the members of the process group pgid from the
// kernel's process table.
func listProcessGroup(pgid int) ([]processInfo, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return nil, err
	}
	members := make([]processInfo, 0, len(procs))
	for i := range procs {
		p := &procs[i]
		members = append(members, processInfo{
			pid:     int(p.Proc.P_pid),
			ppid:    int(p.Eproc.Ppid),
			stopped: p.Proc.P_stat == darwinProcessStopped,
			zombie:  p.Proc.P_stat == darwinProcessZombie,
		})
	}
	return members, nil
}
