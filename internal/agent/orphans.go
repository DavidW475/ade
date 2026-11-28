package agent

import (
	"bytes"
	"os"
	"strconv"
	"syscall"
)

// procDir is the proc file system (tests set their own directory).
var procDir = "/proc"

// findOrphans looks for processes whose environment contains ADE_SESSION_ID with one of the
// given session IDs. Child processes inherit the variable and are therefore found as well.
// Checking the environment instead of stored PIDs protects against reused PIDs and against
// processes of another ADE daemon.
func findOrphans(ids map[string]bool) map[int]string {
	out := map[int]string{}
	if len(ids) == 0 {
		return out
	}
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return out
	}
	self := os.Getpid()
	prefix := []byte(EnvSessionID + "=")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		data, err := os.ReadFile(procDir + "/" + e.Name() + "/environ")
		if err != nil {
			continue
		}
		for kv := range bytes.SplitSeq(data, []byte{0}) {
			if v, ok := bytes.CutPrefix(kv, prefix); ok && ids[string(v)] {
				out[pid] = string(v)
				break
			}
		}
	}
	return out
}

// killOrphans kills the found processes and returns their count.
func killOrphans(ids map[string]bool) map[int]string {
	found := findOrphans(ids)
	for pid := range found {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	return found
}
