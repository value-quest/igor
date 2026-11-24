package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/value-quest/igor/internal/config"
)

// GitHeadPath walks upwards from dir to find the enclosing git repo's HEAD file
// and git directory (handling both standard .git directories and worktree .git files).
func GitHeadPath(dir string) (headPath, gitDir string) {
	cur := filepath.Clean(dir)
	for {
		dotGit := filepath.Join(cur, ".git")
		info, err := os.Stat(dotGit)
		if err == nil {
			if info.IsDir() {
				return filepath.Join(dotGit, "HEAD"), dotGit
			}
			// Worktree or submodule: .git is a file containing "gitdir: <path>"
			if raw, err := os.ReadFile(dotGit); err == nil {
				line := strings.TrimSpace(string(raw))
				if strings.HasPrefix(line, "gitdir:") {
					target := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
					if !filepath.IsAbs(target) {
						target = filepath.Clean(filepath.Join(cur, target))
					}
					return filepath.Join(target, "HEAD"), target
				}
			}
			return "", ""
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", ""
		}
		cur = parent
	}
}

// CurrentBranch returns the active git branch name (empty if detached HEAD or not a git repo)
// and whether a rebase is currently in progress.
func CurrentBranch(dir string) (branch string, rebasing bool) {
	headPath, gitDir := GitHeadPath(dir)
	if headPath == "" {
		return "", false
	}
	if _, err := os.Stat(filepath.Join(gitDir, "rebase-merge")); err == nil {
		return "", true
	}
	if _, err := os.Stat(filepath.Join(gitDir, "rebase-apply")); err == nil {
		return "", true
	}
	raw, err := os.ReadFile(headPath)
	if err != nil {
		return "", false
	}
	line := strings.TrimSpace(string(raw))
	const refPrefix = "ref: refs/heads/"
	if !strings.HasPrefix(line, refPrefix) {
		return "", false
	}
	return strings.TrimPrefix(line, refPrefix), false
}

// SeedBranchState records the current git branch if no state file exists yet,
// preventing a spurious dependency sync on initial setup.
func SeedBranchState(cfg *config.Config, site, dir string) {
	stateFile := cfg.BranchStatePath(site)
	if _, err := os.Stat(stateFile); err == nil {
		return
	}
	branch, rebasing := CurrentBranch(dir)
	if rebasing || branch == "" {
		return
	}
	_ = cfg.EnsureDir(filepath.Dir(stateFile))
	_ = os.WriteFile(stateFile, []byte(branch+"\n"), 0o644)
}

// CheckAndUpdateBranch compares the current git branch against the last recorded one.
// Returns changed=true only when switching between two different named branches (not during rebases).
func CheckAndUpdateBranch(cfg *config.Config, site, dir string) (oldBranch, newBranch string, changed bool) {
	branch, rebasing := CurrentBranch(dir)
	if rebasing || branch == "" {
		return "", "", false
	}
	stateFile := cfg.BranchStatePath(site)
	raw, err := os.ReadFile(stateFile)
	if err != nil {
		// First time seeing this site's branch: seed without triggering a build.
		_ = cfg.EnsureDir(filepath.Dir(stateFile))
		_ = os.WriteFile(stateFile, []byte(branch+"\n"), 0o644)
		return "", branch, false
	}
	prev := strings.TrimSpace(string(raw))
	if prev == branch {
		return prev, branch, false
	}
	_ = cfg.EnsureDir(filepath.Dir(stateFile))
	_ = os.WriteFile(stateFile, []byte(branch+"\n"), 0o644)
	return prev, branch, true
}

// HasNpmBuildScript reports whether <dir>/package.json defines a non-empty scripts.build entry.
func HasNpmBuildScript(dir string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return false
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return false
	}
	return strings.TrimSpace(pkg.Scripts["build"]) != ""
}
