package git

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Grenzen für Patches im Diff, damit riesige Änderungen die API nicht verstopfen.
const (
	MaxFilePatch  = 256 << 10
	MaxTotalPatch = 4 << 20
)

// FileChange ist eine geänderte Datei.
type FileChange struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Status    string `json:"status"` // added, modified, deleted, renamed, copied, type_changed, unmerged
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Binary    bool   `json:"binary"`
	Patch     string `json:"patch,omitempty"`
	// Truncated ist true, wenn der Patch wegen seiner Größe weggelassen wurde.
	Truncated bool `json:"truncated,omitempty"`
}

// Diff ist der Unterschied zwischen einem Basis-Commit und dem Arbeitsbaum.
type Diff struct {
	Base      string       `json:"base_commit"`
	Head      string       `json:"head_commit"`
	Files     []FileChange `json:"files"`
	Additions int          `json:"additions"`
	Deletions int          `json:"deletions"`
}

var statusNames = map[byte]string{
	'A': "added",
	'M': "modified",
	'D': "deleted",
	'R': "renamed",
	'C': "copied",
	'T': "type_changed",
	'U': "unmerged",
}

// WorktreeDiff vergleicht den Arbeitsbaum von dir (committete, gestagte, ungestagte und
// neue, nicht ignorierte Dateien) mit dem Commit base.
//
// Dafür wird ein temporärer Index verwendet (GIT_INDEX_FILE): Der Index des Workspaces
// bleibt unverändert, auch wenn ein Agent gerade darin arbeitet.
func WorktreeDiff(ctx context.Context, dir, base string, withPatch bool) (*Diff, error) {
	head, err := Output(ctx, dir, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return nil, err
	}

	idx, cleanup, err := tempIndex(ctx, dir)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	env := []string{"GIT_INDEX_FILE=" + idx}

	if _, err := Run(ctx, workspaceCmd(dir, env, "add", "--all", "--", ".")); err != nil {
		return nil, err
	}

	diffArgs := func(extra ...string) []string {
		return append([]string{"diff", "--cached", "--find-renames", "--no-color", "--no-ext-diff", "--no-textconv"},
			append(extra, base, "--")...)
	}
	nameStatus, err := Run(ctx, Cmd{Dir: dir, Env: env, Args: diffArgs("--name-status", "-z")})
	if err != nil {
		return nil, err
	}
	numStat, err := Run(ctx, Cmd{Dir: dir, Env: env, Args: diffArgs("--numstat", "-z")})
	if err != nil {
		return nil, err
	}

	files, err := parseNameStatus(nameStatus)
	if err != nil {
		return nil, err
	}
	if err := applyNumStat(files, numStat); err != nil {
		return nil, err
	}

	d := &Diff{Base: base, Head: head, Files: files}
	for _, f := range files {
		d.Additions += f.Additions
		d.Deletions += f.Deletions
	}

	if withPatch && len(files) > 0 {
		patch, err := Run(ctx, Cmd{Dir: dir, Env: env, Args: diffArgs("--patch")})
		if err != nil {
			return nil, err
		}
		attachPatches(d.Files, patch)
	}
	return d, nil
}

// tempIndex kopiert den Index des Repos in eine temporäre Datei. Die Kopie spart
// git add das erneute Hashen unveränderter Dateien.
func tempIndex(ctx context.Context, dir string) (string, func(), error) {
	gitIndex, err := Output(ctx, dir, "rev-parse", "--git-path", "index")
	if err != nil {
		return "", nil, err
	}
	if !filepath.IsAbs(gitIndex) {
		gitIndex = filepath.Join(dir, gitIndex)
	}
	tmp, err := os.CreateTemp("", "ade-index-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.Remove(tmp.Name()) }
	if src, err := os.Open(gitIndex); err == nil {
		_, err = io.Copy(tmp, src)
		_ = src.Close()
		if err != nil {
			_ = tmp.Close()
			cleanup()
			return "", nil, err
		}
	} else {
		// Ohne Index (frisches Repo) startet git mit einem leeren Index; eine leere Datei
		// wäre ungültig.
		_ = tmp.Close()
		cleanup()
		return tmp.Name(), cleanup, nil
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return tmp.Name(), cleanup, nil
}

// parseNameStatus liest `git diff --name-status -z`: "M\0pfad\0" bzw. "R100\0alt\0neu\0".
func parseNameStatus(out []byte) ([]FileChange, error) {
	fields := splitZ(out)
	var files []FileChange
	for i := 0; i < len(fields); {
		code := fields[i]
		if code == "" {
			return nil, fmt.Errorf("name-status: leerer Statuscode")
		}
		status, ok := statusNames[code[0]]
		if !ok {
			status = "modified"
		}
		f := FileChange{Status: status}
		if code[0] == 'R' || code[0] == 'C' {
			if i+2 >= len(fields) {
				return nil, fmt.Errorf("name-status: unvollständiger Eintrag")
			}
			f.OldPath, f.Path = fields[i+1], fields[i+2]
			i += 3
		} else {
			if i+1 >= len(fields) {
				return nil, fmt.Errorf("name-status: unvollständiger Eintrag")
			}
			f.Path = fields[i+1]
			i += 2
		}
		files = append(files, f)
	}
	return files, nil
}

// applyNumStat liest `git diff --numstat -z` in derselben Reihenfolge wie name-status:
// "plus\tminus\tpfad\0" bzw. bei Umbenennungen "plus\tminus\t\0alt\0neu\0".
// Binärdateien haben "-\t-".
func applyNumStat(files []FileChange, out []byte) error {
	fields := splitZ(out)
	n := 0
	for i := 0; i < len(fields); n++ {
		parts := strings.SplitN(fields[i], "\t", 3)
		if len(parts) != 3 {
			return fmt.Errorf("numstat: unerwartetes Format %q", fields[i])
		}
		if parts[2] == "" { // Umbenennung: zwei weitere Felder
			i += 3
		} else {
			i++
		}
		if n >= len(files) {
			return fmt.Errorf("numstat: mehr Einträge als name-status")
		}
		if parts[0] == "-" && parts[1] == "-" {
			files[n].Binary = true
			continue
		}
		files[n].Additions, _ = strconv.Atoi(parts[0])
		files[n].Deletions, _ = strconv.Atoi(parts[1])
	}
	if n != len(files) {
		return fmt.Errorf("numstat: %d Einträge, name-status: %d", n, len(files))
	}
	return nil
}

// attachPatches teilt die Patch-Ausgabe an den "diff --git"-Kopfzeilen auf. Die Reihenfolge
// entspricht der von name-status. Passt die Anzahl nicht, bleiben die Patches leer.
func attachPatches(files []FileChange, patch []byte) {
	var chunks [][]byte
	header := []byte("diff --git ")
	rest := patch
	for len(rest) > 0 {
		next := bytes.Index(rest[1:], append([]byte("\n"), header...))
		if next < 0 {
			chunks = append(chunks, rest)
			break
		}
		chunks = append(chunks, rest[:next+2])
		rest = rest[next+2:]
	}
	if len(chunks) != len(files) {
		return
	}
	total := 0
	for i := range files {
		c := chunks[i]
		if len(c) > MaxFilePatch || total+len(c) > MaxTotalPatch {
			files[i].Truncated = true
			continue
		}
		total += len(c)
		files[i].Patch = string(c)
	}
}

func splitZ(out []byte) []string {
	s := strings.TrimSuffix(string(out), "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}
