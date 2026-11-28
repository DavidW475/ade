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

// Limits for the feedback prompt.
const (
	MaxFeedback      = 50 << 10
	maxFeedbackPatch = 48 << 10
)

// collect saves a workspace's state after an agent turn: WIP commit and fetch into the main
// repository (architecture.md 4.2). Errors are only logged.
func (m *Manager) collect(workspaceID, sessionID string) {
	if m.opts.Workspaces == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), workspace.PushTimeout)
	defer cancel()
	if _, err := m.opts.Workspaces.Collect(ctx, workspaceID, sessionID); err != nil {
		if errors.Is(err, apperr.ErrNotFound) || errors.Is(err, apperr.ErrConflict) {
			m.log.Debug("workspace not collected", "workspace", workspaceID, "session", sessionID, "err", err)
			return
		}
		m.log.Warn("WIP commit/collect failed", "workspace", workspaceID, "session", sessionID, "err", err)
	}
}

// WorkspaceBusy returns a conflict as long as an agent is working in the workspace (waiting
// agents and terminals do not count).
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
			return apperr.Conflict("agent %s is still working in the workspace – wait until it is done, or stop it", id)
		}
	}
	return nil
}

// FeedbackRequest is the body of POST /api/workspaces/{id}/feedback.
type FeedbackRequest struct {
	Comment string `json:"comment"`
	// Adapter, Model and PermissionMode override the values of the last session.
	Adapter        string `json:"adapter,omitempty"`
	Model          string `json:"model,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
}

// Feedback starts a new session in the same workspace that gets the user's comment and the
// current diff as its instruction. A waiting agent in the workspace is ended first. At the
// end of the session another commit is created on the same branch.
func (m *Manager) Feedback(ctx context.Context, workspaceID string, req FeedbackRequest) (*store.Session, error) {
	comment := strings.TrimSpace(req.Comment)
	if comment == "" {
		return nil, apperr.Invalid("feedback missing")
	}
	if len(comment) > MaxFeedback {
		return nil, apperr.Invalid("feedback is too long (at most 50 KiB)")
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

	// Take over the settings of the last agent session.
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
		return nil, apperr.Invalid("feedback only goes to agents, not to a terminal")
	}

	if err := m.stopIdleAgents(ctx, workspaceID); err != nil {
		return nil, err
	}
	sess, err := m.start(ctx, start, startExtra{Role: workspace.RoleFeedback, Prompt: FeedbackPrompt(comment, ws.BaseBranch, diff)})
	if err != nil {
		return nil, err
	}
	m.log.Info("feedback session started", "id", sess.ID, "workspace", workspaceID)
	return sess, nil
}

// stopIdleAgents ends waiting agents in the workspace (status “done”).
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

// FeedbackPrompt builds the instruction for a feedback session.
func FeedbackPrompt(comment, base string, d *git.Diff) string {
	var b strings.Builder
	b.WriteString("The user has reviewed your changes in this repository so far and asks for adjustments.\n\n")
	b.WriteString("## Feedback\n\n")
	b.WriteString(comment)
	b.WriteString("\n\n")
	if d == nil || len(d.Files) == 0 {
		fmt.Fprintf(&b, "There are no changes compared to %s so far.\n\n", base)
	} else {
		fmt.Fprintf(&b, "## Current state compared to %s\n\n%d file(s), +%d −%d:\n", base, len(d.Files), d.Additions, d.Deletions)
		for i, f := range d.Files {
			if i == 200 {
				fmt.Fprintf(&b, "- … and %d more\n", len(d.Files)-200)
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
			fmt.Fprintf(&b, "(%d file(s) without a patch because the diff is too large – look at them with `git diff %s` if needed.)\n\n", omitted, shortRev(d.Base))
		}
	}
	b.WriteString("Implement the feedback. Keep working on the current branch; you do not need to commit or push.\n")
	return b.String()
}

func shortRev(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}
