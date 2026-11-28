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

// ghTimeout limits one call of gh.
const ghTimeout = time.Minute

// maxIssueText limits the text of an issue in the task.
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

// pollIssues starts a run for every open issue with the label that has no run yet.
func (s *Service) pollIssues(ctx context.Context, p *project.View, t *project.Trigger) error {
	g := t.GitHubIssues
	out, err := s.gh(ctx, p.RepoPath, "", "issue", "list", "--state", "open", "--label", g.Label,
		"--limit", "100", "--json", "number,title,body,url,author")
	if err != nil {
		return err
	}
	var issues []issue
	if err := json.Unmarshal([]byte(out), &issues); err != nil {
		return fmt.Errorf("answer of gh issue list not readable: %w", err)
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
			errs = append(errs, fmt.Sprintf("issue #%d: %v", is.Number, err))
			if v == nil {
				continue
			}
		}
		started++
		if g.Comments() {
			msg := fmt.Sprintf("ADE: run `%s` of factory `%s` started (label `%s`). The pull request will follow here when it is done.", v.ID, v.Factory, g.Label)
			if err := s.comment(ctx, p.RepoPath, is.Number, msg); err != nil {
				s.log.Warn("comment on issue", "project", p.ID, "issue", is.Number, "err", err)
			}
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// issueTask turns an issue into the run's task. The text comes from GitHub and is untrusted
// input: it is clearly separated as a description, not as an instruction to ADE.
func issueTask(is issue) string {
	body := strings.TrimSpace(trimTo(is.Body, maxIssueText))
	if body == "" {
		body = "(no description)"
	}
	return fmt.Sprintf("GitHub issue #%d: %s\n%s\n\n%s", is.Number, strings.TrimSpace(is.Title), is.URL, body)
}

// comment writes a comment on an issue.
func (s *Service) comment(ctx context.Context, repo string, number int, body string) error {
	_, err := s.gh(ctx, repo, body, "issue", "comment", strconv.Itoa(number), "--body-file", "-")
	return err
}

// gh calls the gh binary in the repository (for the remote) and returns the output.
func (s *Service) gh(ctx context.Context, dir, stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.opts.GH, args...) //nolint:gosec // G204: fixed arguments, values from project.yaml
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", errors.New("GitHub CLI (gh) not found – install it and run `gh auth login`")
		}
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("gh %s: %s", args[0]+" "+args[1], trimTo(msg, 500))
	}
	return out.String(), nil
}
