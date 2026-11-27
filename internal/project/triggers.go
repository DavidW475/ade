package project

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/DavidW475/ade/internal/cron"
)

// Trigger startet Runs einer Fabrik automatisch (doc/factory.md#5-auslöser). Genau eins von
// GitHubIssues und Schedule ist gesetzt.
type Trigger struct {
	// Name identifiziert den Auslöser im Projekt (Zustand, Dashboard).
	Name    string `yaml:"name" json:"name"`
	Factory string `yaml:"factory" json:"factory"`
	// Inputs sind feste Eingaben des Runs (bei Issues kommt task aus dem Issue).
	Inputs map[string]string `yaml:"inputs" json:"inputs,omitempty"`
	// Sandbox gilt für die Runs (nil: ja – Auslöser verarbeiten fremde Eingaben).
	Sandbox *bool `yaml:"sandbox" json:"sandbox,omitempty"`
	// AllowUnbudgeted erlaubt Fabriken ohne budget.max_cost_usd.
	AllowUnbudgeted bool `yaml:"allow_unbudgeted" json:"allow_unbudgeted,omitempty"`

	GitHubIssues *GitHubIssues `yaml:"github_issues" json:"github_issues,omitempty"`
	Schedule     *Schedule     `yaml:"schedule" json:"schedule,omitempty"`
}

// GitHubIssues startet einen Run je offenem Issue mit Label.
type GitHubIssues struct {
	Label string `yaml:"label" json:"label"`
	// Authors beschränkt auf Issues dieser GitHub-Nutzer (leer: alle).
	Authors []string `yaml:"authors" json:"authors,omitempty"`
	// Interval ist der Abstand der Abfragen (Standard 5m, mindestens 1m).
	Interval time.Duration `yaml:"interval" json:"interval_ns,omitempty"`
	// Comment: Kommentare am Issue bei Start und Ende (Standard: ja).
	Comment *bool `yaml:"comment" json:"comment,omitempty"`
}

// Schedule startet Runs nach Zeitplan.
type Schedule struct {
	Cron string `yaml:"cron" json:"cron"`
	// CatchUp holt nach einer Pause des Daemons den letzten verpassten Termin nach
	// (Standard: ja).
	CatchUp *bool `yaml:"catch_up" json:"catch_up,omitempty"`
}

// Grenzen für Auslöser.
const (
	DefaultIssueInterval = 5 * time.Minute
	MinIssueInterval     = time.Minute
	maxTriggers          = 20
)

// Kind liefert die Art des Auslösers.
func (t *Trigger) Kind() string {
	if t.GitHubIssues != nil {
		return "github_issues"
	}
	return "schedule"
}

// SandboxOn meldet, ob die Runs in der Sandbox laufen.
func (t *Trigger) SandboxOn() bool { return t.Sandbox == nil || *t.Sandbox }

// IssueInterval liefert den Abstand der Abfragen.
func (g *GitHubIssues) IssueInterval() time.Duration {
	if g.Interval <= 0 {
		return DefaultIssueInterval
	}
	return g.Interval
}

// Comments meldet, ob ADE am Issue kommentiert.
func (g *GitHubIssues) Comments() bool { return g.Comment == nil || *g.Comment }

// CatchUpOn meldet, ob ein verpasster Termin nachgeholt wird.
func (s *Schedule) CatchUpOn() bool { return s.CatchUp == nil || *s.CatchUp }

var (
	triggerNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	labelRE       = regexp.MustCompile(`^[^,\x00-\x1f]{1,50}$`)
	ghUserRE      = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})(?:\[bot\])?$`)
)

func validateTriggers(ts []Trigger) string {
	if len(ts) > maxTriggers {
		return fmt.Sprintf("triggers: höchstens %d", maxTriggers)
	}
	seen := map[string]bool{}
	for i := range ts {
		t := &ts[i]
		at := fmt.Sprintf("triggers[%d]", i)
		if !triggerNameRE.MatchString(t.Name) {
			return fmt.Sprintf("%s.name: %q ist ungültig (Kleinbuchstaben, Ziffern, - und _)", at, t.Name)
		}
		if seen[t.Name] {
			return fmt.Sprintf("%s.name: %s gibt es schon", at, t.Name)
		}
		seen[t.Name] = true
		at = "triggers." + t.Name
		if !triggerNameRE.MatchString(t.Factory) {
			return fmt.Sprintf("%s.factory: %q ist kein gültiger Fabrik-Name", at, t.Factory)
		}
		for k, v := range t.Inputs {
			if !triggerNameRE.MatchString(k) || len(v) > 100<<10 {
				return fmt.Sprintf("%s.inputs.%s: ungültig", at, k)
			}
		}
		if (t.GitHubIssues == nil) == (t.Schedule == nil) {
			return at + ": genau eins von github_issues oder schedule angeben"
		}
		if g := t.GitHubIssues; g != nil {
			g.Label = strings.TrimSpace(g.Label)
			if !labelRE.MatchString(g.Label) {
				return at + ".github_issues.label: 1 bis 50 Zeichen, ohne Komma"
			}
			for _, a := range g.Authors {
				if !ghUserRE.MatchString(a) {
					return fmt.Sprintf("%s.github_issues.authors: %q ist kein GitHub-Name", at, a)
				}
			}
			if g.Interval != 0 && (g.Interval < MinIssueInterval || g.Interval > 24*time.Hour) {
				return at + ".github_issues.interval: zwischen 1m und 24h"
			}
			if _, ok := t.Inputs["task"]; ok {
				return at + ".inputs.task: kommt bei github_issues aus dem Issue"
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
