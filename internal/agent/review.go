package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/git"
	"github.com/DavidW475/ade/internal/store"
	"github.com/DavidW475/ade/internal/workspace"
)

// Grenzen für den Feedback-Prompt.
const (
	MaxFeedback      = 50 << 10
	maxFeedbackPatch = 48 << 10
)

// collect sichert den Stand eines Workspaces nach einer Agent-Runde: WIP-Commit und Fetch
// ins Haupt-Repo (architecture.md 4.2). Fehler werden nur geloggt.
func (m *Manager) collect(workspaceID, sessionID string) {
	if m.opts.Workspaces == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), workspace.PushTimeout)
	defer cancel()
	if _, err := m.opts.Workspaces.Collect(ctx, workspaceID, sessionID); err != nil {
		if errors.Is(err, apperr.ErrNotFound) || errors.Is(err, apperr.ErrConflict) {
			m.log.Debug("Workspace nicht übernommen", "workspace", workspaceID, "session", sessionID, "err", err)
			return
		}
		m.log.Warn("WIP-Commit/Übernahme fehlgeschlagen", "workspace", workspaceID, "session", sessionID, "err", err)
	}
}

// WorkspaceBusy liefert einen Konflikt, solange im Workspace ein Agent arbeitet (wartende
// Agenten und Terminals zählen nicht).
func (m *Manager) WorkspaceBusy(_ context.Context, workspaceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, r := range m.runs {
		if r.workspaceID != workspaceID || r.caps.Terminal {
			continue
		}
		r.mu.Lock()
		status := r.status
		r.mu.Unlock()
		if status != store.SessionIdle {
			return apperr.Conflict("im Workspace arbeitet noch der Agent %s – warten, bis er fertig ist, oder ihn stoppen", id)
		}
	}
	return nil
}

// FeedbackRequest ist der Body von POST /api/workspaces/{id}/feedback.
type FeedbackRequest struct {
	Comment string `json:"comment"`
	// Adapter, Model und PermissionMode überschreiben die Werte der letzten Session.
	Adapter        string `json:"adapter,omitempty"`
	Model          string `json:"model,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
}

// Feedback startet im selben Workspace eine neue Session, die den Kommentar des Nutzers
// und den aktuellen Diff als Anweisung bekommt. Ein wartender Agent im Workspace wird
// vorher beendet. Am Ende der Session entsteht ein weiterer Commit im selben Branch.
func (m *Manager) Feedback(ctx context.Context, workspaceID string, req FeedbackRequest) (*store.Session, error) {
	comment := strings.TrimSpace(req.Comment)
	if comment == "" {
		return nil, apperr.Invalid("Feedback fehlt")
	}
	if len(comment) > MaxFeedback {
		return nil, apperr.Invalid("Feedback ist zu lang (höchstens 50 KiB)")
	}
	ws, err := m.opts.Workspaces.Get(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if err := m.WorkspaceBusy(ctx, workspaceID); err != nil {
		return nil, err
	}
	diff, err := m.opts.Workspaces.Diff(ctx, workspaceID, true)
	if err != nil {
		return nil, err
	}

	// Einstellungen der letzten Agent-Session übernehmen.
	start := StartRequest{WorkspaceID: workspaceID, Task: comment, Adapter: req.Adapter, Model: req.Model, PermissionMode: req.PermissionMode}
	ss, err := m.opts.Store.ListSessions(ctx, store.SessionFilter{WorkspaceID: workspaceID, Limit: 100})
	if err != nil {
		return nil, err
	}
	for _, s := range ss {
		a, err := m.opts.Adapters.Get(s.Adapter)
		if err != nil || a.Capabilities().Terminal {
			continue
		}
		if start.Adapter == "" {
			start.Adapter = s.Adapter
		}
		if start.Adapter == s.Adapter {
			if start.Model == "" {
				start.Model = s.Model
			}
			if start.PermissionMode == "" {
				start.PermissionMode = s.PermissionMode
			}
		}
		break
	}
	if a, err := m.opts.Adapters.Get(start.Adapter); err == nil && a.Capabilities().Terminal {
		return nil, apperr.Invalid("Feedback geht nur an Agenten, nicht an ein Terminal")
	}

	if err := m.stopIdleAgents(ctx, workspaceID); err != nil {
		return nil, err
	}
	sess, err := m.start(ctx, start, startExtra{Role: workspace.RoleFeedback, Prompt: FeedbackPrompt(comment, ws.BaseBranch, diff)})
	if err != nil {
		return nil, err
	}
	m.log.Info("Feedback-Session gestartet", "id", sess.ID, "workspace", workspaceID)
	return sess, nil
}

// stopIdleAgents beendet wartende Agenten im Workspace (Status „fertig“).
func (m *Manager) stopIdleAgents(ctx context.Context, workspaceID string) error {
	m.mu.Lock()
	var runs []*run
	for _, r := range m.runs {
		if r.workspaceID == workspaceID && !r.caps.Terminal {
			runs = append(runs, r)
		}
	}
	m.mu.Unlock()
	for _, r := range runs {
		r.stop(store.SessionDone)
	}
	for _, r := range runs {
		select {
		case <-r.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// FeedbackPrompt baut die Anweisung für eine Feedback-Session.
func FeedbackPrompt(comment, base string, d *git.Diff) string {
	var b strings.Builder
	b.WriteString("Der Nutzer hat deine bisherigen Änderungen in diesem Repository geprüft und bittet um Anpassungen.\n\n")
	b.WriteString("## Feedback\n\n")
	b.WriteString(comment)
	b.WriteString("\n\n")
	if d == nil || len(d.Files) == 0 {
		fmt.Fprintf(&b, "Gegenüber %s gibt es bisher keine Änderungen.\n\n", base)
	} else {
		fmt.Fprintf(&b, "## Aktueller Stand gegenüber %s\n\n%d Datei(en), +%d −%d:\n", base, len(d.Files), d.Additions, d.Deletions)
		for i, f := range d.Files {
			if i == 200 {
				fmt.Fprintf(&b, "- … und %d weitere\n", len(d.Files)-200)
				break
			}
			fmt.Fprintf(&b, "- %s (%s)\n", f.Path, f.Status)
		}
		b.WriteString("\n```diff\n")
		total, omitted := 0, 0
		for _, f := range d.Files {
			if f.Patch == "" || total+len(f.Patch) > maxFeedbackPatch {
				if !f.Binary {
					omitted++
				}
				continue
			}
			total += len(f.Patch)
			b.WriteString(strings.TrimRight(f.Patch, "\n"))
			b.WriteString("\n")
		}
		b.WriteString("```\n\n")
		if omitted > 0 {
			fmt.Fprintf(&b, "(%d Datei(en) ohne Patch, weil der Diff zu groß ist – bei Bedarf mit `git diff %s` ansehen.)\n\n", omitted, shortRev(d.Base))
		}
	}
	b.WriteString("Setze das Feedback um. Arbeite im aktuellen Branch weiter; committen und pushen musst du nicht.\n")
	return b.String()
}

func shortRev(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}
