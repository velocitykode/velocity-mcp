//go:build unix && !darwin && !linux

package console

import "errors"

// listProcessGroup reports that the process group cannot be listed on this
// platform. The run then gives the inspector a process group of its own (see
// placeInspector) and signals and ends that group as a whole.
func listProcessGroup(int) ([]processInfo, error) {
	return nil, errors.New("mcp: listing a process group is not supported on this platform")
}
