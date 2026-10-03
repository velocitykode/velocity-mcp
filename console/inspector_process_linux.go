//go:build linux

package console

import (
	"os"
	"path/filepath"
)

// listProcessGroup lists the members of the process group pgid from the proc
// filesystem. A process that exits while the table is being read is simply
// absent from the result.
func listProcessGroup(pgid int) ([]processInfo, error) {
	stats, err := filepath.Glob("/proc/[0-9]*/stat")
	if err != nil {
		return nil, err
	}
	if len(stats) == 0 {
		return nil, os.ErrNotExist
	}
	var members []processInfo
	for _, path := range stats {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if info, group, ok := parseProcStat(raw); ok && group == pgid {
			members = append(members, info)
		}
	}
	return members, nil
}
