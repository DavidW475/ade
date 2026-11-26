package agent

import (
	"bytes"
	"os"
	"strconv"
	"syscall"
)

// procDir ist das proc-Dateisystem (Tests setzen ein eigenes Verzeichnis).
var procDir = "/proc"

// findOrphans sucht Prozesse, deren Umgebung ADE_SESSION_ID mit einer der gegebenen
// Session-IDs enthält. Kindprozesse erben die Variable, werden also mitgefunden. Die Prüfung
// über die Umgebung statt über gespeicherte PIDs schützt vor wiederverwendeten PIDs und vor
// Prozessen eines anderen ADE-Daemons.
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

// killOrphans beendet die gefundenen Prozesse hart und liefert ihre Anzahl.
func killOrphans(ids map[string]bool) map[int]string {
	found := findOrphans(ids)
	for pid := range found {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	return found
}
