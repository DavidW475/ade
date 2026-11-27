package trigger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DavidW475/ade/internal/factory"
	"github.com/DavidW475/ade/internal/project"
)

// ghTimeout begrenzt einen Aufruf von gh.
const ghTimeout = time.Minute

// maxIssueText begrenzt den Text eines Issues in der Aufgabe.
const maxIssueText = 64 << 10

type issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	URL    string `json:"url"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
}

// pollIssues startet für jedes offene Issue mit dem Label, das noch keinen Run hat, einen Run.
func (s *Service) pollIssues(ctx context.Context, p *project.View, t *project.Trigger) error {
	g := t.GitHubIssues
	out, err := s.gh(ctx, p.RepoPath, "", "issue", "list", "--state", "open", "--label", g.Label,
		"--limit", "100", "--json", "number,title,body,url,author")
	if err != nil {
		return err
	}
	var issues []issue
	if err := json.Unmarshal([]byte(out), &issues); err != nil {
		return fmt.Errorf("Antwort von gh issue list nicht lesbar: %w", err)
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
	started := 0
	var errs []string
	for _, is := range issues {
		if started >= maxStartsPerPoll || ctx.Err() != nil {
			break
		}
		key := strconv.Itoa(is.Number)
		if has, err := s.opts.Store.HasTriggerRun(ctx, p.ID, t.Name, key); err != nil || has {
			continue
		}
		if len(g.Authors) > 0 && !slices.ContainsFunc(g.Authors, func(a string) bool { return strings.EqualFold(a, is.Author.Login) }) {
			continue
		}
		v, err := s.startRun(ctx, p, t, key, factory.StartRequest{
			Task: issueTask(is), TriggerRef: is.URL, Closes: is.Number,
		})
		if err != nil {
			errs = append(errs, fmt.Sprintf("Issue #%d: %v", is.Number, err))
			if v == nil {
				continue
			}
		}
		started++
		if g.Comments() {
			msg := fmt.Sprintf("ADE: Run `%s` der Fabrik `%s` gestartet (Label `%s`). Der Pull Request folgt hier, wenn er fertig ist.", v.ID, v.Factory, g.Label)
			if err := s.comment(ctx, p.RepoPath, is.Number, msg); err != nil {
				s.log.Warn("Issue kommentieren", "projekt", p.ID, "issue", is.Number, "err", err)
			}
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// issueTask macht aus einem Issue die Aufgabe des Runs. Der Text stammt von GitHub und ist
// fremde Eingabe: Er steht klar abgegrenzt als Beschreibung, nicht als Anweisung an ADE.
func issueTask(is issue) string {
	body := strings.TrimSpace(trimTo(is.Body, maxIssueText))
	if body == "" {
		body = "(ohne Beschreibung)"
	}
	return fmt.Sprintf("GitHub-Issue #%d: %s\n%s\n\n%s", is.Number, strings.TrimSpace(is.Title), is.URL, body)
}

// comment schreibt einen Kommentar an ein Issue.
func (s *Service) comment(ctx context.Context, repo string, number int, body string) error {
	_, err := s.gh(ctx, repo, body, "issue", "comment", strconv.Itoa(number), "--body-file", "-")
	return err
}

// gh ruft das gh-Binary im Repo auf (für das Remote) und liefert die Ausgabe.
func (s *Service) gh(ctx context.Context, dir, stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.opts.GH, args...) //nolint:gosec // G204: feste Argumente, Werte aus project.yaml
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", errors.New("GitHub CLI (gh) nicht gefunden – installieren und `gh auth login` ausführen")
		}
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("gh %s: %s", args[0]+" "+args[1], trimTo(msg, 500))
	}
	return out.String(), nil
}
