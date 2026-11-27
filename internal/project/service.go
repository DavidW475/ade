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

// View ist ein Projekt mit seiner aktuell gelesenen Konfiguration.
type View struct {
	*store.Project
	// BaseBranch ist der wirksame Basis-Branch (project.yaml vor default_branch).
	BaseBranch string  `json:"base_branch"`
	Config     *Config `json:"config"`
	// ConfigError ist gesetzt, wenn .ade/project.yaml ungültig ist.
	ConfigError string `json:"config_error,omitempty"`
	// Sandbox ist die wirksame Konfiguration aus .ade/sandbox.yaml (mit Voreinstellungen).
	Sandbox *sandbox.Config `json:"sandbox"`
	// SandboxError ist gesetzt, wenn .ade/sandbox.yaml ungültig ist.
	SandboxError string `json:"sandbox_error,omitempty"`
	// Warnings sind Hinweise wie "liegt unter /mnt".
	Warnings []string `json:"warnings,omitempty"`
	// Approvals sind die wirksamen Freigabe-Regeln (bei ungültiger project.yaml leer: alles
	// braucht eine Freigabe).
	Approvals Approvals `json:"approvals"`
}

// Approvals sind die wirksamen Regeln je Umgebung.
type Approvals struct {
	Sandbox approval.RuleSet `json:"sandbox"`
	Local   approval.RuleSet `json:"local"`
}

// ApprovalConfig liefert den Abschnitt approvals der project.yaml eines Projekts; bei einer
// ungültigen Datei einen Fehler (dann erlaubt keine Regel etwas).
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

// Service registriert und liest Projekte.
type Service struct {
	store *store.Store
}

// NewService erzeugt einen Service.
func NewService(st *store.Store) *Service { return &Service{store: st} }

// NewID erzeugt eine kurze, zufällige ID aus Kleinbuchstaben und Ziffern. Sie taugt als
// DNS-Label (ws-<id>.localhost) und als Teil eines Branch-Namens.
func NewID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

// Add registriert das Git-Repository, in dem path liegt.
func (s *Service) Add(ctx context.Context, path string) (*View, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, apperr.Invalid("Pfad %q muss absolut sein", path)
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, apperr.Invalid("Pfad %s existiert nicht", path)
	}
	if !st.IsDir() {
		return nil, apperr.Invalid("%s ist kein Verzeichnis", path)
	}
	if !git.Available() {
		return nil, errors.New("git ist nicht installiert")
	}
	info, err := git.Inspect(ctx, path)
	if err != nil {
		return nil, apperr.Invalid("%s ist kein Git-Repository", path)
	}
	if info.Bare {
		return nil, apperr.Invalid("%s ist ein Bare-Repository; ADE braucht ein Repository mit Arbeitsverzeichnis", path)
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

// Get liefert ein Projekt mit aktueller Konfiguration.
func (s *Service) Get(ctx context.Context, id string) (*View, error) {
	p, err := s.store.GetProject(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.load(ctx, p), nil
}

// Find sucht ein Projekt über ID, Name oder Repo-Pfad.
func (s *Service) Find(ctx context.Context, ref string) (*View, error) {
	p, err := s.store.FindProject(ctx, ref)
	if err != nil {
		return nil, err
	}
	return s.load(ctx, p), nil
}

// List liefert alle Projekte.
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

// Remove meldet ein Projekt ab. Das Repository selbst bleibt unverändert.
func (s *Service) Remove(ctx context.Context, id string) error {
	return s.store.DeleteProject(ctx, id)
}

func (s *Service) load(ctx context.Context, p *store.Project) *View {
	cfg, err := LoadConfig(ctx, p.RepoPath)
	if _, statErr := os.Stat(p.RepoPath); statErr != nil {
		err = fmt.Errorf("Repository %s ist nicht mehr vorhanden", p.RepoPath)
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
			"Das Repository liegt im Windows-Dateisystem (/mnt/…). Zugriffe sind dort sehr langsam; besser nach ~/ in die WSL verschieben.")
	}
	return v
}
