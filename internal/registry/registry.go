package registry

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/value-quest/igor/internal/config"
)

type Entry struct {
	Name    string
	Port    string
	Type    string
	PHP     string
	Dir     string
	Docroot string
}

func (e Entry) EffectiveDocroot() string {
	if e.Docroot == "" {
		return "public"
	}
	return e.Docroot
}

func (e Entry) AbsoluteDocroot() string {
	rel := e.EffectiveDocroot()
	if rel == "." {
		return e.Dir
	}
	return e.Dir + "/" + rel
}

func (e Entry) ToTSV() string {
	cols := []string{e.Name, e.Port, e.Type, e.PHP, e.Dir}
	if e.Docroot != "" {
		cols = append(cols, e.Docroot)
	}
	return strings.Join(cols, "\t")
}

type Store struct {
	cfg *config.Config
}

func New(cfg *config.Config) *Store {
	return &Store{cfg: cfg}
}

func (s *Store) All() ([]Entry, error) {
	f, err := os.Open(s.cfg.RegistryFile)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []Entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r\n")
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 5 {
			continue
		}
		e := Entry{
			Name: parts[0],
			Port: parts[1],
			Type: parts[2],
			PHP:  parts[3],
			Dir:  parts[4],
		}
		if len(parts) >= 6 {
			e.Docroot = parts[5]
		}
		entries = append(entries, e)
	}
	return entries, sc.Err()
}

func (s *Store) Get(name string) (*Entry, error) {
	all, err := s.All()
	if err != nil {
		return nil, err
	}
	for _, e := range all {
		if e.Name == name {
			cp := e
			return &cp, nil
		}
	}
	return nil, nil
}

func (s *Store) FindByDir(dir, typ string) (*Entry, error) {
	all, err := s.All()
	if err != nil {
		return nil, err
	}
	for _, e := range all {
		if e.Dir == dir && (typ == "" || e.Type == typ) {
			cp := e
			return &cp, nil
		}
	}
	return nil, nil
}

func (s *Store) SitesUsingPHP(version string) ([]Entry, error) {
	all, err := s.All()
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, e := range all {
		if e.Type == "php" && e.PHP == version {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *Store) RunnableNames() ([]string, error) {
	all, err := s.All()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range all {
		if e.Type == "php" || e.Type == "service" {
			names = append(names, e.Name)
		}
	}
	return names, nil
}

func (s *Store) RequirePHPSite(name string) (*Entry, error) {
	e, err := s.Get(name)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, fmt.Errorf("no site '%s'", name)
	}
	if e.Type != "php" {
		return nil, fmt.Errorf("'%s' is a proxy route - start that service yourself", name)
	}
	return e, nil
}

func (s *Store) NextPort() (int, error) {
	all, err := s.All()
	if err != nil {
		return 0, err
	}
	maxPort := config.FirstPort - 1
	for _, e := range all {
		if e.Type == "php" {
			if p, err := strconv.Atoi(e.Port); err == nil && p > maxPort {
				maxPort = p
			}
		}
	}
	return maxPort + 1, nil
}

func (s *Store) Add(e Entry) error {
	all, err := s.All()
	if err != nil {
		return err
	}
	all = append(all, e)
	return s.save(all)
}

func (s *Store) Upsert(e Entry) error {
	all, err := s.All()
	if err != nil {
		return err
	}
	var next []Entry
	for _, existing := range all {
		if existing.Name != e.Name {
			next = append(next, existing)
		}
	}
	next = append(next, e)
	return s.save(next)
}

func (s *Store) UpdatePHP(name, php string) error {
	all, err := s.All()
	if err != nil {
		return err
	}
	for i := range all {
		if all[i].Name == name {
			all[i].PHP = php
		}
	}
	return s.save(all)
}

func (s *Store) Remove(name string) error {
	all, err := s.All()
	if err != nil {
		return err
	}
	var next []Entry
	for _, e := range all {
		if e.Name != name {
			next = append(next, e)
		}
	}
	return s.save(next)
}

func (s *Store) Touch() error {
	if err := s.cfg.EnsureDir(s.cfg.IgorHome); err != nil {
		return err
	}
	if _, err := os.Stat(s.cfg.RegistryFile); os.IsNotExist(err) {
		return os.WriteFile(s.cfg.RegistryFile, nil, 0o644)
	}
	return nil
}

func (s *Store) save(entries []Entry) error {
	if err := s.cfg.EnsureDir(s.cfg.IgorHome); err != nil {
		return err
	}
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.ToTSV())
		b.WriteByte('\n')
	}
	tmp := s.cfg.RegistryFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.cfg.RegistryFile)
}
