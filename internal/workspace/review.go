package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/git"
	"github.com/DavidW475/ade/internal/store"
)

// Review und Übernahme (architecture.md 4.2): Der Daemon committet offene Änderungen im
// Workspace, holt den Branch per fetch ins Haupt-Repo und mergt oder veröffentlicht ihn
// von dort. Der Agent selbst hat weder Zugriff auf das Haupt-Repo noch Push-Rechte.

// Remote ist das Remote des Haupt-Repos, zu dem Pull Requests gepusht werden.
const Remote = "origin"

// Zeitgrenzen für Netzwerkschritte.
const (
	PushTimeout = 3 * time.Minute
	GHTimeout   = 2 * time.Minute
)

// RoleFeedback kennzeichnet Sessions, die aus „Feedback geben“ entstanden sind.
const RoleFeedback = "feedback"

// SetBusyCheck registriert eine Prüfung, die vor Mergen und PR läuft (z. B. „arbeitet
// gerade ein Agent?“). Liefert sie einen Fehler, wird abgebrochen.
func (m *Manager) SetBusyCheck(fn func(ctx context.Context, id string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.busyCheck = fn
}

// SetGitHubCLI setzt den Pfad zum gh-Binary (Standard: gh im PATH).
func (m *Manager) SetGitHubCLI(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gh = path
}

func (m *Manager) checkBusy(ctx context.Context, id string) error {
	m.mu.Lock()
	fn := m.busyCheck
	m.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(ctx, id)
}

// lockWorkspace serialisiert Commit, Fetch, Merge und Push eines Workspaces.
func (m *Manager) lockWorkspace(id string) func() {
	m.mu.Lock()
	l := m.locks[id]
	if l == nil {
		l = &wsLock{ch: make(chan struct{}, 1)}
		m.locks[id] = l
	}
	l.refs++
	m.mu.Unlock()
	l.ch <- struct{}{}
	return func() {
		<-l.ch
		m.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(m.locks, id)
		}
		m.mu.Unlock()
	}
}

type wsLock struct {
	ch   chan struct{}
	refs int
}

// Collect committet offene Änderungen im Workspace („ade: wip <label>“) und holt den
// Branch ade/<id> ins Haupt-Repo. label ist z. B. die ID der beendeten Session.
func (m *Manager) Collect(ctx context.Context, id, label string) (*store.Workspace, error) {
	unlock := m.lockWorkspace(id)
	defer unlock()
	return m.collect(ctx, id, label)
}

func (m *Manager) collect(ctx context.Context, id, label string) (*store.Workspace, error) {
	w, err := m.store.GetWorkspace(ctx, id)
	if err != nil {
		return nil, err
	}
	if !hasWorktree(w) {
		return nil, apperr.Conflict("Workspace %s hat keinen Arbeitsbaum (Status %s)", id, w.Status)
	}
	p, err := m.store.GetProject(ctx, w.ProjectID)
	if err != nil {
		return nil, err
	}
	if label == "" {
		label = w.ID
	}
	committed, err := git.CommitAll(ctx, w.Path, "ade: wip "+label)
	if err != nil {
		return nil, fmt.Errorf("WIP-Commit: %w", err)
	}
	if err := git.FetchBranch(ctx, p.RepoPath, w.Path, w.Branch); err != nil {
		return nil, fmt.Errorf("Branch ins Projekt-Repo holen: %w", err)
	}
	head, err := git.ResolveCommit(ctx, p.RepoPath, "refs/heads/"+w.Branch)
	if err != nil {
		return nil, err
	}
	if err := m.store.UpdateWorkspace(ctx, id, store.WorkspaceUpdate{HeadCommit: &head}); err != nil {
		return nil, err
	}
	if committed || head != w.HeadCommit {
		m.log.Info("Workspace übernommen", "id", id, "commit", head, "wip_commit", committed)
	}
	return m.store.GetWorkspace(ctx, id)
}

// MergeConflictError meldet Konflikte beim Mergen in den Basis-Branch.
type MergeConflictError struct {
	Branch string
	Files  []string
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("Merge in %s hat Konflikte in %d Datei(en): %s – Feedback geben oder im Workspace auflösen",
		e.Branch, len(e.Files), strings.Join(e.Files, ", "))
}

func (e *MergeConflictError) Unwrap() error { return apperr.ErrConflict }

// Merge übernimmt den Branch des Workspaces mit einem Merge-Commit in den Basis-Branch des
// Haupt-Repos. Ist der Basis-Branch dort ausgecheckt, läuft git merge in dessen
// Arbeitsbaum, sonst wird der Merge ohne Arbeitsbaum berechnet. Konflikte ändern nichts.
func (m *Manager) Merge(ctx context.Context, id string) (*store.Workspace, error) {
	if err := m.checkBusy(ctx, id); err != nil {
		return nil, err
	}
	unlock := m.lockWorkspace(id)
	defer unlock()
	w, err := m.collect(ctx, id, "")
	if err != nil {
		return nil, err
	}
	p, err := m.store.GetProject(ctx, w.ProjectID)
	if err != nil {
		return nil, err
	}
	repo := p.RepoPath
	if w.HeadCommit == w.BaseCommit {
		return nil, apperr.Invalid("keine Änderungen zum Mergen")
	}
	base, err := git.ResolveCommit(ctx, repo, "refs/heads/"+w.BaseBranch)
	if err != nil {
		return nil, apperr.Conflict("Basis-Branch %s gibt es im Projekt-Repo nicht mehr", w.BaseBranch)
	}
	if done, err := git.IsAncestor(ctx, repo, w.HeadCommit, base); err != nil {
		return nil, err
	} else if done {
		return nil, apperr.Conflict("%s ist bereits vollständig in %s enthalten", w.Branch, w.BaseBranch)
	}
	tree, conflicts, err := git.MergeTree(ctx, repo, base, w.HeadCommit)
	if err != nil {
		return nil, fmt.Errorf("Merge berechnen: %w", err)
	}
	if len(conflicts) > 0 {
		return nil, &MergeConflictError{Branch: w.BaseBranch, Files: conflicts}
	}

	msg := fmt.Sprintf("Merge %s: %s", w.Branch, m.title(ctx, w))
	wt, err := git.CheckedOutIn(ctx, repo, w.BaseBranch)
	if err != nil {
		return nil, err
	}
	var commit string
	if wt != "" {
		commit, err = git.MergeInWorktree(ctx, wt, "refs/heads/"+w.Branch, msg)
		if err != nil {
			return nil, apperr.Conflict("Merge im Arbeitsbaum %s fehlgeschlagen (Änderungen dort? Dann erst committen oder stashen): %v", wt, err)
		}
	} else {
		commit, err = git.MergeCommit(ctx, repo, w.BaseBranch, base, w.HeadCommit, tree, msg)
		if err != nil {
			return nil, fmt.Errorf("Merge-Commit anlegen: %w", err)
		}
	}
	if err := m.store.UpdateWorkspace(ctx, id, store.WorkspaceUpdate{MergeCommit: &commit}); err != nil {
		return nil, err
	}
	m.log.Info("Workspace gemergt", "id", id, "basis", w.BaseBranch, "commit", commit, "arbeitsbaum", wt)
	return m.store.GetWorkspace(ctx, id)
}

// PRDraft ist der Vorschlag für einen Pull Request.
type PRDraft struct {
	Title  string `json:"title"`
	Body   string `json:"body"`
	Base   string `json:"base"`
	Head   string `json:"head"`
	Remote string `json:"remote"`
	// RemoteURL ist leer, wenn das Projekt-Repo kein Remote hat.
	RemoteURL string `json:"remote_url,omitempty"`
	// URL ist der bestehende PR (dann aktualisiert ein Push ihn nur).
	URL string `json:"url,omitempty"`
}

// PullRequestDraft erzeugt Titel und Beschreibung für einen PR aus Aufgabe, Feedback und Diff.
func (m *Manager) PullRequestDraft(ctx context.Context, id string) (*PRDraft, error) {
	w, err := m.store.GetWorkspace(ctx, id)
	if err != nil {
		return nil, err
	}
	p, err := m.store.GetProject(ctx, w.ProjectID)
	if err != nil {
		return nil, err
	}
	d := &PRDraft{Title: m.title(ctx, w), Base: w.BaseBranch, Head: w.Branch, Remote: Remote, URL: w.PRURL}
	d.RemoteURL, _ = git.RemoteURL(ctx, p.RepoPath, Remote)

	var b strings.Builder
	sessions, _ := m.store.ListSessions(ctx, store.SessionFilter{WorkspaceID: id, Limit: 1000})
	var task string
	var feedback []string
	var cost float64
	for i := len(sessions) - 1; i >= 0; i-- { // älteste zuerst
		s := sessions[i]
		cost += s.CostUSD
		switch {
		case s.Role == RoleFeedback && s.Task != "":
			feedback = append(feedback, s.Task)
		case task == "" && s.Task != "":
			task = s.Task
		}
	}
	if task != "" {
		b.WriteString("## Aufgabe\n\n" + task + "\n\n")
	}
	if len(feedback) > 0 {
		b.WriteString("## Feedback\n\n")
		for _, f := range feedback {
			b.WriteString("- " + strings.ReplaceAll(strings.TrimSpace(f), "\n", "\n  ") + "\n")
		}
		b.WriteString("\n")
	}
	if diff, err := m.Diff(ctx, id, false); err == nil && len(diff.Files) > 0 {
		fmt.Fprintf(&b, "## Änderungen\n\n%d Datei(en), +%d −%d\n\n", len(diff.Files), diff.Additions, diff.Deletions)
		for i, f := range diff.Files {
			if i == 50 {
				fmt.Fprintf(&b, "- … und %d weitere\n", len(diff.Files)-50)
				break
			}
			fmt.Fprintf(&b, "- `%s` (%s)\n", f.Path, statusLabel(f.Status))
		}
		b.WriteString("\n")
	}
	b.WriteString("---\n")
	fmt.Fprintf(&b, "Erstellt mit [ADE](https://github.com/DavidW475/ade) · Workspace `%s`", w.ID)
	if len(sessions) > 0 {
		fmt.Fprintf(&b, " · %d Agent-Session(s)", len(sessions))
	}
	if cost > 0 {
		fmt.Fprintf(&b, " · Kosten %.2f $", cost)
	}
	b.WriteString("\n")
	d.Body = b.String()
	return d, nil
}

var statusLabels = map[string]string{
	"added": "neu", "modified": "geändert", "deleted": "gelöscht", "renamed": "umbenannt",
	"copied": "kopiert", "type_changed": "Typ geändert", "unmerged": "Konflikt",
}

func statusLabel(s string) string {
	if l, ok := statusLabels[s]; ok {
		return l
	}
	return s
}

// title ist die erste Zeile der ersten Aufgabe im Workspace (höchstens 72 Zeichen).
func (m *Manager) title(ctx context.Context, w *store.Workspace) string {
	sessions, _ := m.store.ListSessions(ctx, store.SessionFilter{WorkspaceID: w.ID, Limit: 1000})
	for i := len(sessions) - 1; i >= 0; i-- {
		s := sessions[i]
		if s.Role == RoleFeedback || strings.TrimSpace(s.Task) == "" {
			continue
		}
		t := strings.TrimSpace(s.Task)
		if j := strings.IndexByte(t, '\n'); j >= 0 {
			t = strings.TrimSpace(t[:j])
		}
		if utf8.RuneCountInString(t) > 72 {
			r := []rune(t)
			t = strings.TrimSpace(string(r[:71])) + "…"
		}
		return t
	}
	return "Änderungen aus " + w.Branch
}

// PRRequest ist der Body von POST /api/workspaces/{id}/pull-request.
type PRRequest struct {
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
	Draft bool   `json:"draft,omitempty"`
}

// PullRequest pusht den Branch vom Host zum Remote und erstellt mit gh einen Pull Request.
// Gibt es schon einen, wird er durch den Push nur aktualisiert.
func (m *Manager) PullRequest(ctx context.Context, id string, req PRRequest) (*store.Workspace, error) {
	if err := m.checkBusy(ctx, id); err != nil {
		return nil, err
	}
	if len(req.Title) > 256 || len(req.Body) > 64<<10 {
		return nil, apperr.Invalid("Titel oder Beschreibung zu lang")
	}
	if strings.ContainsRune(req.Title, '\n') {
		return nil, apperr.Invalid("der Titel darf keinen Zeilenumbruch enthalten")
	}
	unlock := m.lockWorkspace(id)
	defer unlock()
	w, err := m.collect(ctx, id, "")
	if err != nil {
		return nil, err
	}
	p, err := m.store.GetProject(ctx, w.ProjectID)
	if err != nil {
		return nil, err
	}
	if _, err := git.RemoteURL(ctx, p.RepoPath, Remote); err != nil {
		return nil, apperr.Invalid("das Projekt-Repo hat kein Remote %q – PRs brauchen ein GitHub-Remote", Remote)
	}
	if w.HeadCommit == w.BaseCommit {
		return nil, apperr.Invalid("keine Änderungen für einen Pull Request")
	}
	m.mu.Lock()
	gh := m.gh
	m.mu.Unlock()
	if w.PRURL == "" {
		if _, err := exec.LookPath(gh); err != nil {
			return nil, apperr.Invalid("GitHub CLI (gh) nicht gefunden – installieren und `gh auth login` ausführen")
		}
	}

	pctx, cancel := context.WithTimeout(ctx, PushTimeout)
	err = git.Push(pctx, p.RepoPath, Remote, w.Branch)
	cancel()
	if err != nil {
		return nil, apperr.Conflict("Push nach %s fehlgeschlagen: %v", Remote, err)
	}
	m.log.Info("Branch gepusht", "id", id, "remote", Remote, "branch", w.Branch)
	if w.PRURL != "" {
		return w, nil
	}

	if req.Title == "" || req.Body == "" {
		d, err := m.PullRequestDraft(ctx, id)
		if err != nil {
			return nil, err
		}
		if req.Title == "" {
			req.Title = d.Title
		}
		if req.Body == "" {
			req.Body = d.Body
		}
	}
	url, err := m.createPR(ctx, gh, p.RepoPath, w, req)
	if err != nil {
		return nil, err
	}
	if err := m.store.UpdateWorkspace(ctx, id, store.WorkspaceUpdate{PRURL: &url}); err != nil {
		return nil, err
	}
	m.log.Info("Pull Request erstellt", "id", id, "url", url)
	return m.store.GetWorkspace(ctx, id)
}

func (m *Manager) createPR(ctx context.Context, gh, repo string, w *store.Workspace, req PRRequest) (string, error) {
	args := []string{"pr", "create", "--head", w.Branch, "--base", w.BaseBranch, "--title", req.Title, "--body-file", "-"}
	if req.Draft {
		args = append(args, "--draft")
	}
	out, stderr, err := runGH(ctx, gh, repo, req.Body, args...)
	if err != nil && strings.Contains(stderr, "already exists") {
		out, stderr, err = runGH(ctx, gh, repo, "", "pr", "view", w.Branch, "--json", "url", "--jq", ".url")
	}
	if err != nil {
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = err.Error()
		}
		return "", apperr.Conflict("gh pr create fehlgeschlagen: %s", msg)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") {
			return l, nil
		}
	}
	return "", fmt.Errorf("gh lieferte keine PR-URL: %q", out)
}

func runGH(ctx context.Context, gh, dir, stdin string, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, GHTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, gh, args...) //nolint:gosec // G204: feste Argumente, Werte vom angemeldeten Nutzer
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1", "GIT_TERMINAL_PROMPT=0")
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// FileSide ist eine Seite einer geänderten Datei.
type FileSide struct {
	Exists   bool   `json:"exists"`
	Binary   bool   `json:"binary,omitempty"`
	TooLarge bool   `json:"too_large,omitempty"`
	Text     string `json:"text,omitempty"`
}

// FileContents sind Basis- und aktueller Stand einer Datei für den Diff-Editor.
type FileContents struct {
	Path     string   `json:"path"`
	OldPath  string   `json:"old_path,omitempty"`
	Original FileSide `json:"original"`
	Modified FileSide `json:"modified"`
}

// File liefert eine Datei im Basis-Commit (unter oldPath, falls umbenannt) und im
// Arbeitsbaum des Workspaces.
func (m *Manager) File(ctx context.Context, id, file, oldPath string) (*FileContents, error) {
	if err := checkRelPath(file); err != nil {
		return nil, err
	}
	if oldPath == "" {
		oldPath = file
	} else if err := checkRelPath(oldPath); err != nil {
		return nil, err
	}
	w, err := m.store.GetWorkspace(ctx, id)
	if err != nil {
		return nil, err
	}
	if !hasWorktree(w) {
		return nil, apperr.Conflict("Workspace %s hat keinen Arbeitsbaum (Status %s)", id, w.Status)
	}
	fc := &FileContents{Path: file}
	if oldPath != file {
		fc.OldPath = oldPath
	}
	data, ok, err := git.Blob(ctx, w.Path, w.BaseCommit, oldPath)
	if fc.Original, err = side(data, ok, err); err != nil {
		return nil, err
	}
	data, ok, err = git.WorktreeBlob(ctx, w.Path, file)
	if fc.Modified, err = side(data, ok, err); err != nil {
		return nil, err
	}
	return fc, nil
}

func side(data []byte, ok bool, err error) (FileSide, error) {
	switch {
	case errors.Is(err, git.ErrTooLarge):
		return FileSide{Exists: true, TooLarge: true}, nil
	case err != nil:
		return FileSide{}, err
	case !ok:
		return FileSide{}, nil
	case git.IsBinary(data) || !utf8.Valid(data):
		return FileSide{Exists: true, Binary: true}, nil
	}
	return FileSide{Exists: true, Text: string(data)}, nil
}

// checkRelPath lässt nur relative Pfade innerhalb des Repos zu.
func checkRelPath(p string) error {
	if p == "" || strings.ContainsRune(p, 0) || strings.HasPrefix(p, "/") || path.Clean(p) != p ||
		p == ".." || strings.HasPrefix(p, "../") || len(p) > 4096 {
		return apperr.Invalid("ungültiger Dateipfad %q", p)
	}
	return nil
}
