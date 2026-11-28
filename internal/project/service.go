package project

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/DavidW475/ade/internal/apperr"
	"github.com/DavidW475/ade/internal/approval"
	"github.com/DavidW475/ade/internal/git"
	"github.com/DavidW475/ade/internal/sandbox"
	"github.com/DavidW475/ade/internal/store"
)

// View is a project with its currently read configuration.
type View struct {
	*store.Project
	// BaseBranch is the effective base branch (project.yaml before default_branch).
	BaseBranch string  `json:"base_branch"`
	Config     *Config `json:"config"`
	// ConfigError is set if .ade/project.yaml is invalid.
	ConfigError string `json:"config_error,omitempty"`
	// Sandbox is the effective configuration from .ade/sandbox.yaml (with defaults).
	Sandbox *sandbox.Config `json:"sandbox"`
	// SandboxError is set if .ade/sandbox.yaml is invalid.
	SandboxError string `json:"sandbox_error,omitempty"`
	// Warnings are notices like "is under /mnt".
	Warnings []string `json:"warnings,omitempty"`
	// Approvals are the effective approval rules (empty for an invalid project.yaml: everything
	// needs an approval).
	Approvals Approvals `json:"approvals"`
}

// Approvals are the effective rules per environment.
type Approvals struct {
	Sandbox approval.RuleSet `json:"sandbox"`
	Local   approval.RuleSet `json:"local"`
}

// ApprovalConfig returns the approvals section of a project's project.yaml; for an invalid
// file an error (then no rule allows anything).
func (s *Service) ApprovalConfig(ctx context.Context, id string) (*approval.Config, error) {
	v, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if v.ConfigError != "" {
		return nil, errors.New(v.ConfigError)
	}
	return v.Config.Approvals, nil
}

// Service registers and reads projects.
type Service struct {
	store *store.Store
}

// NewService creates a service.
func NewService(st *store.Store) *Service { return &Service{store: st} }

// NewID creates a short random ID from lowercase letters and digits. It works as a
// DNS label (ws-<id>.localhost) and as part of a branch name.
func NewID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

// Add registers the Git repository that path is in.
func (s *Service) Add(ctx context.Context, path string) (*View, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, apperr.Invalid("path %q must be absolute", path)
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, apperr.Invalid("path %s does not exist", path)
	}
	if !st.IsDir() {
		return nil, apperr.Invalid("%s is not a directory", path)
	}
	if !git.Available() {
		return nil, errors.New("git is not installed")
	}
	info, err := git.Inspect(ctx, path)
	if err != nil {
		return nil, apperr.Invalid("%s is not a Git repository", path)
	}
	if info.Bare {
		return nil, apperr.Invalid("%s is a bare repository; ADE needs a repository with a working tree", path)
	}
	top, err := filepath.EvalSymlinks(info.TopLevel)
	if err != nil {
		return nil, err
	}

	cfg, cfgErr := LoadConfig(ctx, top)
	branch := cfg.BaseBranch
	if branch == "" || !git.BranchExists(ctx, top, branch) {
		if branch, err = git.DefaultBranch(ctx, top); err != nil {
			return nil, apperr.Invalid("%s: %v", top, err)
		}
	}
	name := cfg.Name
	if name == "" {
		name = filepath.Base(top)
	}

	p := &store.Project{ID: NewID(), Name: name, RepoPath: top, DefaultBranch: branch}
	if err := s.store.CreateProject(ctx, p); err != nil {
		return nil, err
	}
	return s.view(p, cfg, cfgErr), nil
}

// Get returns a project with its current configuration.
func (s *Service) Get(ctx context.Context, id string) (*View, error) {
	p, err := s.store.GetProject(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.load(ctx, p), nil
}

// Find looks up a project by ID, name or repository path.
func (s *Service) Find(ctx context.Context, ref string) (*View, error) {
	p, err := s.store.FindProject(ctx, ref)
	if err != nil {
		return nil, err
	}
	return s.load(ctx, p), nil
}

// List returns all projects.
func (s *Service) List(ctx context.Context) ([]*View, error) {
	ps, err := s.store.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*View, len(ps))
	for i, p := range ps {
		out[i] = s.load(ctx, p)
	}
	return out, nil
}

// Remove unregisters a project. The repository itself stays unchanged.
func (s *Service) Remove(ctx context.Context, id string) error {
	return s.store.DeleteProject(ctx, id)
}

func (s *Service) load(ctx context.Context, p *store.Project) *View {
	cfg, err := LoadConfig(ctx, p.RepoPath)
	if _, statErr := os.Stat(p.RepoPath); statErr != nil {
		err = fmt.Errorf("repository %s no longer exists", p.RepoPath)
	}
	return s.view(p, cfg, err)
}

func (s *Service) view(p *store.Project, cfg *Config, cfgErr error) *View {
	v := &View{Project: p, Config: cfg, BaseBranch: p.DefaultBranch}
	sb, sbErr := sandbox.LoadConfig(p.RepoPath)
	v.Sandbox = sb
	if sbErr != nil {
		v.SandboxError = sbErr.Error()
	}
	if cfg.BaseBranch != "" {
		v.BaseBranch = cfg.BaseBranch
	}
	if cfgErr != nil {
		v.ConfigError = cfgErr.Error()
	} else {
		v.Approvals = Approvals{Sandbox: cfg.Approvals.Effective(true), Local: cfg.Approvals.Effective(false)}
	}
	if strings.HasPrefix(p.RepoPath, "/mnt/") {
		v.Warnings = append(v.Warnings,
			"The repository is on the Windows file system (/mnt/…). Access there is very slow; better move it to ~/ in WSL.")
	}
	return v
}
