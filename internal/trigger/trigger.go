// Package trigger starts factory runs automatically (doc/factory.md#5-triggers): one run per
// open GitHub issue with a label (polling via gh) and runs on a schedule. The triggers are
// configured in .ade/project.yaml (triggers), their state is in SQLite.
package trigger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/cron"
	"github.com/DavidW475/ade/internal/factory"
	"github.com/DavidW475/ade/internal/project"
	"github.com/DavidW475/ade/internal/store"
)

// DefaultTick is the interval at which the service checks all triggers.
const DefaultTick = time.Minute

// maxStartsPerPoll limits the runs one poll starts (the rest follows at the next
// one).
const maxStartsPerPoll = 5

// lookback limits how far a schedule looks back after a pause.
const lookback = 7 * 24 * time.Hour

// Runs starts and reads runs (factory.Runner).
type Runs interface {
	Start(ctx context.Context, req factory.StartRequest) (*factory.View, error)
	Get(ctx context.Context, id string) (*factory.View, error)
}

// Options configures the service.
type Options struct {
	Store    *store.Store
	Projects *project.Service
	Runs     Runs
	// GH is the gh binary (default: gh in PATH).
	GH     string
	Logger *slog.Logger
	// Now and Tick override clock and tick (tests).
	Now  func() time.Time
	Tick time.Duration
}

// Service checks the triggers of all projects at the tick.
type Service struct {
	opts Options
	log  *slog.Logger

	mu     sync.Mutex // one check at a time
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates the service.
func New(opts Options) *Service {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.GH == "" {
		opts.GH = "gh"
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Tick <= 0 {
		opts.Tick = DefaultTick
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{opts: opts, log: opts.Logger, ctx: ctx, cancel: cancel}
}

// Start checks the triggers right away and then at the tick.
func (s *Service) Start() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(s.opts.Tick)
		defer t.Stop()
		for {
			s.Check(s.ctx)
			select {
			case <-s.ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// Close stops the service.
func (s *Service) Close() {
	s.cancel()
	s.wg.Wait()
}

// Check checks all triggers once: due polls and dates, reports to issues.
func (s *Service) Check(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, err := s.opts.Projects.List(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("read projects", "err", err)
		}
		return
	}
	now := s.opts.Now()
	for _, p := range ps {
		if p.ConfigError != "" || p.Config == nil {
			continue
		}
		for i := range p.Config.Triggers {
			if ctx.Err() != nil {
				return
			}
			s.checkTrigger(ctx, p, &p.Config.Triggers[i], now)
		}
		s.report(ctx, p)
	}
}

func (s *Service) checkTrigger(ctx context.Context, p *project.View, t *project.Trigger, now time.Time) {
	st, err := s.opts.Store.GetTriggerState(ctx, p.ID, t.Name)
	if err != nil || st.Paused {
		return
	}
	var msg string
	switch {
	case t.GitHubIssues != nil:
		if st.LastCheck != nil && now.Sub(*st.LastCheck) < t.GitHubIssues.IssueInterval() {
			return
		}
		msg = errText(s.pollIssues(ctx, p, t))
		s.update(ctx, p.ID, t.Name, store.TriggerUpdate{LastCheck: &now, LastError: &msg})
	case t.Schedule != nil:
		s.schedule(ctx, p, t, st, now)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Service) update(ctx context.Context, projectID, name string, u store.TriggerUpdate) {
	if err := s.opts.Store.UpdateTriggerState(ctx, projectID, name, u); err != nil && ctx.Err() == nil {
		s.log.Error("store trigger state", "project", projectID, "trigger", name, "err", err)
	}
}

// startRun starts a run for the trigger and records it under key.
func (s *Service) startRun(ctx context.Context, p *project.View, t *project.Trigger, key string, req factory.StartRequest) (*factory.View, error) {
	sandboxed := t.SandboxOn()
	req.ProjectID, req.Factory, req.Sandbox = p.ID, t.Factory, &sandboxed
	req.Trigger, req.RequireBudget = t.Name, !t.AllowUnbudgeted
	if req.Inputs == nil {
		req.Inputs = map[string]string{}
	}
	for k, v := range t.Inputs {
		req.Inputs[k] = v
	}
	v, err := s.opts.Runs.Start(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := s.opts.Store.AddTriggerRun(ctx, &store.TriggerRun{ProjectID: p.ID, Trigger: t.Name, Key: key, RunID: v.ID}); err != nil {
		return v, err
	}
	s.log.Info("run triggered", "project", p.ID, "trigger", t.Name, "key", key, "run", v.ID)
	return v, nil
}

// --- Schedule ---------------------------------------------------------------------------

func (s *Service) schedule(ctx context.Context, p *project.View, t *project.Trigger, st *store.TriggerState, now time.Time) {
	sched, err := cron.Parse(t.Schedule.Cron)
	if err != nil {
		return // checked when loading the project.yaml
	}
	if st.LastFire == nil {
		// Seen for the first time: count from now, catch up on nothing.
		s.update(ctx, p.ID, t.Name, store.TriggerUpdate{LastCheck: &now, LastFire: &now})
		return
	}
	from := st.LastFire.In(now.Location())
	if now.Sub(from) > lookback {
		from = now.Add(-lookback)
	}
	var slot time.Time
	for n := sched.Next(from); !n.IsZero() && !n.After(now); n = sched.Next(n) {
		slot = n
	}
	if slot.IsZero() {
		s.update(ctx, p.ID, t.Name, store.TriggerUpdate{LastCheck: &now})
		return
	}
	u := store.TriggerUpdate{LastCheck: &now, LastFire: &slot}
	msg := ""
	switch {
	case now.Sub(slot) > 2*s.opts.Tick+time.Minute && !t.Schedule.CatchUpOn():
		msg = fmt.Sprintf("date %s missed (daemon was not running), catch_up is off", slot.Format("2006-01-02 15:04"))
	default:
		if active := s.activeRun(ctx, p.ID, t.Name); active != "" {
			msg = fmt.Sprintf("date %s skipped: run %s is still running", slot.Format("2006-01-02 15:04"), active)
			break
		}
		_, err := s.startRun(ctx, p, t, slot.UTC().Format(time.RFC3339), factory.StartRequest{
			TriggerRef: "Zeitplan " + t.Schedule.Cron + ", Termin " + slot.Format("2006-01-02 15:04 MST"),
		})
		msg = errText(err)
	}
	u.LastError = &msg
	s.update(ctx, p.ID, t.Name, u)
}

// activeRun returns the still running run of a trigger (empty: none).
func (s *Service) activeRun(ctx context.Context, projectID, name string) string {
	trs, err := s.opts.Store.ListTriggerRuns(ctx, store.TriggerRunFilter{ProjectID: projectID, Trigger: name, Limit: 5})
	if err != nil {
		return ""
	}
	for _, tr := range trs {
		if v, err := s.opts.Runs.Get(ctx, tr.RunID); err == nil && store.RunActive(v.Status) {
			return v.ID
		}
	}
	return ""
}

// --- Reports ----------------------------------------------------------------------------

// report reports ended runs of issue triggers on the issue.
func (s *Service) report(ctx context.Context, p *project.View) {
	trs, err := s.opts.Store.ListTriggerRuns(ctx, store.TriggerRunFilter{ProjectID: p.ID, Unreported: true})
	if err != nil {
		return
	}
	for _, tr := range trs {
		v, err := s.opts.Runs.Get(ctx, tr.RunID)
		if err == nil && store.RunActive(v.Status) {
			continue
		}
		var t *project.Trigger
		for i := range p.Config.Triggers {
			if p.Config.Triggers[i].Name == tr.Trigger {
				t = &p.Config.Triggers[i]
			}
		}
		if t != nil && t.GitHubIssues != nil && t.GitHubIssues.Comments() && v != nil {
			if n, err := strconv.Atoi(tr.Key); err == nil {
				if err := s.comment(ctx, p.RepoPath, n, endComment(v)); err != nil {
					s.log.Warn("comment on issue", "project", p.ID, "issue", n, "err", err)
					continue // again next time
				}
			}
		}
		if err := s.opts.Store.MarkTriggerRunReported(ctx, p.ID, tr.Trigger, tr.Key); err != nil {
			s.log.Error("record report", "run", tr.RunID, "err", err)
		}
	}
}

func endComment(v *factory.View) string {
	switch {
	case v.Status == store.RunDone && v.PRURL != "":
		return fmt.Sprintf("ADE: run `%s` (factory `%s`) is done – pull request: %s", v.ID, v.Factory, v.PRURL)
	case v.Status == store.RunDone:
		return fmt.Sprintf("ADE: run `%s` (factory `%s`) is done.", v.ID, v.Factory)
	case v.Status == store.RunCanceled:
		return fmt.Sprintf("ADE: run `%s` (factory `%s`) was canceled: %s", v.ID, v.Factory, v.Error)
	}
	return fmt.Sprintf("ADE: run `%s` (factory `%s`) failed: %s", v.ID, v.Factory, v.Error)
}

// --- View and control -------------------------------------------------------------------

// View is a trigger with state for dashboard and API.
type View struct {
	project.Trigger
	Kind  string             `json:"kind"`
	State store.TriggerState `json:"state"`
	// Next is the next poll or the next date.
	Next *time.Time `json:"next,omitempty"`
	// Runs are the most recently triggered runs.
	Runs []store.TriggerRun `json:"runs"`
}

// List returns the triggers of a project.
func (s *Service) List(ctx context.Context, projectID string) ([]View, error) {
	p, err := s.opts.Projects.Get(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := []View{}
	if p.Config == nil {
		return out, nil
	}
	now := s.opts.Now()
	for _, t := range p.Config.Triggers {
		st, err := s.opts.Store.GetTriggerState(ctx, p.ID, t.Name)
		if err != nil {
			return nil, err
		}
		v := View{Trigger: t, Kind: t.Kind(), State: *st}
		if !st.Paused {
			switch {
			case t.GitHubIssues != nil:
				next := now
				if st.LastCheck != nil {
					next = st.LastCheck.Add(t.GitHubIssues.IssueInterval())
				}
				v.Next = &next
			case t.Schedule != nil:
				if sched, err := cron.Parse(t.Schedule.Cron); err == nil {
					if n := sched.Next(now); !n.IsZero() {
						v.Next = &n
					}
				}
			}
		}
		if v.Runs, err = s.opts.Store.ListTriggerRuns(ctx, store.TriggerRunFilter{ProjectID: p.ID, Trigger: t.Name, Limit: 10}); err != nil {
			return nil, err
		}
		if v.Runs == nil {
			v.Runs = []store.TriggerRun{}
		}
		out = append(out, v)
	}
	return out, nil
}

// SetPaused pauses or resumes a trigger.
func (s *Service) SetPaused(ctx context.Context, projectID, name string, paused bool) (*View, error) {
	list, err := s.List(ctx, projectID)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(list, func(v View) bool { return v.Name == name })
	if i < 0 {
		return nil, apperr.NotFound("trigger %s does not exist in project %s", name, projectID)
	}
	if err := s.opts.Store.UpdateTriggerState(ctx, projectID, name, store.TriggerUpdate{Paused: &paused}); err != nil {
		return nil, err
	}
	s.log.Info("trigger changed", "project", projectID, "trigger", name, "paused", paused)
	list, err = s.List(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for _, v := range list {
		if v.Name == name {
			return &v, nil
		}
	}
	return nil, errors.New("trigger disappeared")
}

// trimTo shortens s to at most n bytes (not in the middle of a UTF-8 character).
func trimTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return strings.TrimSpace(s[:n]) + "\n\n[… truncated]"
}
