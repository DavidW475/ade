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

// Limits for patches in the diff, so huge changes do not clog the API.
const (
	MaxFilePatch  = 256 << 10
	MaxTotalPatch = 4 << 20
)

// FileChange is a changed file.
type FileChange struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Status    string `json:"status"` // added, modified, deleted, renamed, copied, type_changed, unmerged
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Binary    bool   `json:"binary"`
	Patch     string `json:"patch,omitempty"`
	// Truncated is true if the patch was left out because of its size.
	Truncated bool `json:"truncated,omitempty"`
}

// Diff is the difference between a base commit and the working tree.
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

// WorktreeDiff compares the working tree of dir (committed, staged, unstaged and new,
// non-ignored files) with the commit base.
//
// A temporary index is used for that (GIT_INDEX_FILE): the workspace's index stays
// unchanged, even while an agent is working in it.
func WorktreeDiff(ctx context.Context, w Worktree, base string, withPatch bool) (*Diff, error) {
	out, err := Run(ctx, workspaceCmd(w, nil, "rev-parse", "--verify", "HEAD^{commit}"))
	if err != nil {
		return nil, err
	}
	head := strings.TrimSpace(string(out))

	idx, cleanup, err := tempIndex(ctx, w)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	env := []string{"GIT_INDEX_FILE=" + idx}

	if _, err := Run(ctx, workspaceCmd(w, env, "add", "--all", "--", ".")); err != nil {
		return nil, err
	}

	diffArgs := func(extra ...string) []string {
		return append([]string{"diff", "--cached", "--find-renames", "--no-color", "--no-ext-diff", "--no-textconv"},
			append(extra, base, "--")...)
	}
	nameStatus, err := Run(ctx, workspaceCmd(w, env, diffArgs("--name-status", "-z")...))
	if err != nil {
		return nil, err
	}
	numStat, err := Run(ctx, workspaceCmd(w, env, diffArgs("--numstat", "-z")...))
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
		patch, err := Run(ctx, workspaceCmd(w, env, diffArgs("--patch")...))
		if err != nil {
			return nil, err
		}
		attachPatches(d.Files, patch)
	}
	return d, nil
}

// remoteTempIndex creates the copy of the index where the runner is (in the container under
// /tmp) and prints its path. Without an index only the path remains; git then starts empty.
const remoteTempIndex = `f=$(mktemp /tmp/ade-index-XXXXXX) || exit 1
i=$(git rev-parse --git-path index) || exit 1
cp -- "$i" "$f" 2>/dev/null || rm -f -- "$f"
printf '%s' "$f"`

// tempIndex copies the repository's index into a temporary file. The copy saves git add
// from rehashing unchanged files.
func tempIndex(ctx context.Context, w Worktree) (string, func(), error) {
	if w.Runner != nil {
		run := func(argv ...string) ([]byte, error) {
			cmd := w.Runner(ctx, baseEnv, argv)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				return nil, &Error{Args: argv[:1], Stderr: stderr.String(), Err: err}
			}
			return stdout.Bytes(), nil
		}
		out, err := run("sh", "-c", remoteTempIndex)
		if err != nil {
			return "", nil, err
		}
		path := strings.TrimSpace(string(out))
		if !strings.HasPrefix(path, "/tmp/ade-index-") {
			return "", nil, fmt.Errorf("temporary index: unexpected output %q", path)
		}
		cleanup := func() { _, _ = run("rm", "-f", "--", path) }
		return path, cleanup, nil
	}
	dir := w.Path
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
		// Without an index (fresh repository) git starts with an empty index; an empty file
		// would be invalid.
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

// parseNameStatus reads `git diff --name-status -z`: "M\0path\0" or "R100\0old\0new\0".
func parseNameStatus(out []byte) ([]FileChange, error) {
	fields := splitZ(out)
	var files []FileChange
	for i := 0; i < len(fields); {
		code := fields[i]
		if code == "" {
			return nil, fmt.Errorf("name-status: empty status code")
		}
		status, ok := statusNames[code[0]]
		if !ok {
			status = "modified"
		}
		f := FileChange{Status: status}
		if code[0] == 'R' || code[0] == 'C' {
			if i+2 >= len(fields) {
				return nil, fmt.Errorf("name-status: incomplete entry")
			}
			f.OldPath, f.Path = fields[i+1], fields[i+2]
			i += 3
		} else {
			if i+1 >= len(fields) {
				return nil, fmt.Errorf("name-status: incomplete entry")
			}
			f.Path = fields[i+1]
			i += 2
		}
		files = append(files, f)
	}
	return files, nil
}

// applyNumStat reads `git diff --numstat -z` in the same order as name-status:
// "plus\tminus\tpath\0" or for renames "plus\tminus\t\0old\0new\0".
// Binary files have "-\t-".
func applyNumStat(files []FileChange, out []byte) error {
	fields := splitZ(out)
	n := 0
	for i := 0; i < len(fields); n++ {
		parts := strings.SplitN(fields[i], "\t", 3)
		if len(parts) != 3 {
			return fmt.Errorf("numstat: unexpected format %q", fields[i])
		}
		if parts[2] == "" { // rename: two more fields
			i += 3
		} else {
			i++
		}
		if n >= len(files) {
			return fmt.Errorf("numstat: more entries than name-status")
		}
		if parts[0] == "-" && parts[1] == "-" {
			files[n].Binary = true
			continue
		}
		files[n].Additions, _ = strconv.Atoi(parts[0])
		files[n].Deletions, _ = strconv.Atoi(parts[1])
	}
	if n != len(files) {
		return fmt.Errorf("numstat: %d entries, name-status: %d", n, len(files))
	}
	return nil
}

// attachPatches splits the patch output at the "diff --git" header lines. The order
// matches that of name-status. If the count does not match, the patches stay empty.
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
