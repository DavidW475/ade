package project

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/DavidW475/ade/internal/cron"
)

// Trigger starts runs of a factory automatically (doc/factory.md#5-triggers). Exactly one of
// GitHubIssues and Schedule is set.
type Trigger struct {
	// Name identifies the trigger within the project (state, dashboard).
	Name    string `yaml:"name" json:"name"`
	Factory string `yaml:"factory" json:"factory"`
	// Inputs are fixed inputs of the run (for issues, task comes from the issue).
	Inputs map[string]string `yaml:"inputs" json:"inputs,omitempty"`
	// Sandbox applies to the runs (nil: yes – triggers process untrusted input).
	Sandbox *bool `yaml:"sandbox" json:"sandbox,omitempty"`
	// AllowUnbudgeted allows factories without budget.max_cost_usd.
	AllowUnbudgeted bool `yaml:"allow_unbudgeted" json:"allow_unbudgeted,omitempty"`

	GitHubIssues *GitHubIssues `yaml:"github_issues" json:"github_issues,omitempty"`
	Schedule     *Schedule     `yaml:"schedule" json:"schedule,omitempty"`
}

// GitHubIssues starts one run per open issue with a label.
type GitHubIssues struct {
	Label string `yaml:"label" json:"label"`
	// Authors restricts to issues of these GitHub users (empty: all).
	Authors []string `yaml:"authors" json:"authors,omitempty"`
	// Interval is the polling interval (default 5m, at least 1m).
	Interval time.Duration `yaml:"interval" json:"interval_ns,omitempty"`
	// Comment: comments on the issue at start and end (default: yes).
	Comment *bool `yaml:"comment" json:"comment,omitempty"`
}

// Schedule starts runs on a schedule.
type Schedule struct {
	Cron string `yaml:"cron" json:"cron"`
	// CatchUp catches up on the last missed date after a pause of the daemon
	// (default: yes).
	CatchUp *bool `yaml:"catch_up" json:"catch_up,omitempty"`
}

// Limits for triggers.
const (
	DefaultIssueInterval = 5 * time.Minute
	MinIssueInterval     = time.Minute
	maxTriggers          = 20
)

// Kind returns the kind of the trigger.
func (t *Trigger) Kind() string {
	if t.GitHubIssues != nil {
		return "github_issues"
	}
	return "schedule"
}

// SandboxOn reports whether the runs use the sandbox.
func (t *Trigger) SandboxOn() bool { return t.Sandbox == nil || *t.Sandbox }

// IssueInterval returns the polling interval.
func (g *GitHubIssues) IssueInterval() time.Duration {
	if g.Interval <= 0 {
		return DefaultIssueInterval
	}
	return g.Interval
}

// Comments reports whether ADE comments on the issue.
func (g *GitHubIssues) Comments() bool { return g.Comment == nil || *g.Comment }

// CatchUpOn reports whether a missed date is caught up.
func (s *Schedule) CatchUpOn() bool { return s.CatchUp == nil || *s.CatchUp }

var (
	triggerNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	labelRE       = regexp.MustCompile(`^[^,\x00-\x1f]{1,50}$`)
	ghUserRE      = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})(?:\[bot\])?$`)
)

func validateTriggers(ts []Trigger) string {
	if len(ts) > maxTriggers {
		return fmt.Sprintf("triggers: at most %d", maxTriggers)
	}
	seen := map[string]bool{}
	for i := range ts {
		t := &ts[i]
		at := fmt.Sprintf("triggers[%d]", i)
		if !triggerNameRE.MatchString(t.Name) {
			return fmt.Sprintf("%s.name: %q is invalid (lowercase letters, digits, - and _)", at, t.Name)
		}
		if seen[t.Name] {
			return fmt.Sprintf("%s.name: %s already exists", at, t.Name)
		}
		seen[t.Name] = true
		at = "triggers." + t.Name
		if !triggerNameRE.MatchString(t.Factory) {
			return fmt.Sprintf("%s.factory: %q is not a valid factory name", at, t.Factory)
		}
		for k, v := range t.Inputs {
			if !triggerNameRE.MatchString(k) || len(v) > 100<<10 {
				return fmt.Sprintf("%s.inputs.%s: invalid", at, k)
			}
		}
		if (t.GitHubIssues == nil) == (t.Schedule == nil) {
			return at + ": specify exactly one of github_issues or schedule"
		}
		if g := t.GitHubIssues; g != nil {
			g.Label = strings.TrimSpace(g.Label)
			if !labelRE.MatchString(g.Label) {
				return at + ".github_issues.label: 1 to 50 characters, without a comma"
			}
			for _, a := range g.Authors {
				if !ghUserRE.MatchString(a) {
					return fmt.Sprintf("%s.github_issues.authors: %q is not a GitHub name", at, a)
				}
			}
			if g.Interval != 0 && (g.Interval < MinIssueInterval || g.Interval > 24*time.Hour) {
				return at + ".github_issues.interval: between 1m and 24h"
			}
			if _, ok := t.Inputs["task"]; ok {
				return at + ".inputs.task: comes from the issue for github_issues"
			}
		}
		if s := t.Schedule; s != nil {
			if _, err := cron.Parse(s.Cron); err != nil {
				return fmt.Sprintf("%s.schedule.cron: %v", at, err)
			}
		}
	}
	return ""
}
