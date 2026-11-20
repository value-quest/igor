package updater

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/value-quest/igor/internal/config"
	"github.com/value-quest/igor/internal/runner"
	"github.com/value-quest/igor/internal/runtime"
)

type GitHubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

type SuiteRelease struct {
	Tag             string
	Arch            string
	Shorts          []string
	AssetURLs       map[string]string
	Versions        map[string]string
	FrankenPHP      string
	HasVersionsJSON bool
}

type UpdatePlan struct {
	Rows    [][]string
	Pending []string
	Dropped []string
}

type Updater struct {
	cfg        *config.Config
	run        runner.Runner
	runtimes   *runtime.Manager
	HTTPClient *http.Client
	APIBase    string
	GitHubBase string
}

func New(cfg *config.Config, run runner.Runner, runtimes *runtime.Manager) *Updater {
	return &Updater{
		cfg:        cfg,
		run:        run,
		runtimes:   runtimes,
		HTTPClient: &http.Client{Timeout: 5 * time.Minute},
		APIBase:    "https://api.github.com",
		GitHubBase: "https://github.com",
	}
}

func DetectArch() (string, error) {
	switch goruntime.GOARCH {
	case "arm64":
		return "arm64", nil
	case "amd64":
		return "amd64", nil
	default:
		return "", fmt.Errorf("unsupported architecture %s", goruntime.GOARCH)
	}
}

func (u *Updater) FetchGitHubRelease(repo string) (*GitHubRelease, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", strings.TrimRight(u.APIBase, "/"), repo)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "igor")
	resp, err := u.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach GitHub for %s: %w", repo, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("could not reach GitHub for %s (HTTP %d)", repo, resp.StatusCode)
	}
	var rel GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("invalid JSON from GitHub for %s: %w", repo, err)
	}
	return &rel, nil
}

var tagRe = regexp.MustCompile(`^v?[0-9][0-9A-Za-z.+-]*$`)

func (u *Updater) FetchLatestSuite() (*SuiteRelease, error) {
	arch, err := DetectArch()
	if err != nil {
		return nil, err
	}
	gh, err := u.FetchGitHubRelease(u.cfg.SuiteRepo)
	if err != nil {
		return nil, err
	}
	if !tagRe.MatchString(gh.TagName) {
		return nil, fmt.Errorf("unexpected release tag '%s'", gh.TagName)
	}

	assetRe := regexp.MustCompile(`^frankenphp-darwin-` + regexp.QuoteMeta(arch) + `-(php\d+)\.tar\.gz$`)
	rel := &SuiteRelease{
		Tag:       gh.TagName,
		Arch:      arch,
		AssetURLs: map[string]string{},
		Versions:  map[string]string{},
	}

	var versionsURL string
	for _, a := range gh.Assets {
		if sm := assetRe.FindStringSubmatch(a.Name); sm != nil {
			rel.Shorts = append(rel.Shorts, sm[1])
			dl := a.BrowserDownloadURL
			if dl == "" {
				dl = fmt.Sprintf("%s/%s/releases/download/%s/%s", strings.TrimRight(u.GitHubBase, "/"), u.cfg.SuiteRepo, gh.TagName, a.Name)
			}
			rel.AssetURLs[sm[1]] = dl
		}
		if a.Name == "versions.json" {
			versionsURL = a.BrowserDownloadURL
			if versionsURL == "" {
				versionsURL = fmt.Sprintf("%s/%s/releases/download/%s/versions.json", strings.TrimRight(u.GitHubBase, "/"), u.cfg.SuiteRepo, gh.TagName)
			}
		}
	}

	if len(rel.Shorts) == 0 {
		return nil, fmt.Errorf("release %s has no darwin-%s builds", gh.TagName, arch)
	}

	if versionsURL != "" {
		if resp, err := u.HTTPClient.Get(versionsURL); err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var parsed map[string]string
				if json.NewDecoder(resp.Body).Decode(&parsed) == nil {
					rel.Versions = parsed
					rel.FrankenPHP = parsed["frankenphp"]
					rel.HasVersionsJSON = true
				}
			}
		}
	}

	return rel, nil
}

func (u *Updater) PlanUpdate(rel *SuiteRelease, force bool) UpdatePlan {
	var plan UpdatePlan
	shortSet := map[string]bool{}

	for _, short := range rel.Shorts {
		shortSet[short] = true
		v := short[3:4] + "." + short[4:]
		avail := rel.Versions[short]
		instPHP := u.runtimes.InstalledPHPPatch(v)
		instFP := u.runtimes.MarkerGet(v, "frankenphp")

		if _, err := os.Stat(filepath.Join(u.cfg.PHPRoot, "php"+v)); os.IsNotExist(err) {
			availCol := rel.Tag
			if avail != "" {
				availCol = avail
			}
			plan.Rows = append(plan.Rows, []string{v, "not installed", availCol})
			plan.Pending = append(plan.Pending, v)
			continue
		}

		availCol := rel.Tag
		if avail != "" {
			availCol = fmt.Sprintf("%s (FrankenPHP %s)", avail, rel.FrankenPHP)
		}
		if instPHP == "" {
			instPHP = "?"
		}
		if instFP == "" {
			instFP = "?"
		}
		plan.Rows = append(plan.Rows, []string{v, fmt.Sprintf("%s (FrankenPHP %s)", instPHP, instFP), availCol})

		switch {
		case force:
			plan.Pending = append(plan.Pending, v)
		case avail != "" && rel.FrankenPHP != "":
			if instPHP != avail || instFP != rel.FrankenPHP {
				plan.Pending = append(plan.Pending, v)
			}
		default:
			if u.runtimes.MarkerGet(v, "tag") != rel.Tag {
				plan.Pending = append(plan.Pending, v)
			}
		}
	}

	for _, v := range u.runtimes.InstalledVersions() {
		short := "php" + strings.ReplaceAll(v, ".", "")
		if shortSet[short] {
			continue
		}
		instPHP := u.runtimes.InstalledPHPPatch(v)
		instFP := u.runtimes.MarkerGet(v, "frankenphp")
		plan.Rows = append(plan.Rows, []string{v, fmt.Sprintf("%s (FrankenPHP %s)", instPHP, instFP), "no longer in the release"})
		plan.Dropped = append(plan.Dropped, v)
	}

	return plan
}

func (u *Updater) DownloadFile(url, dest string) error {
	resp, err := u.HTTPClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d downloading %s", resp.StatusCode, url)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func (u *Updater) InstallVersion(version string, rel *SuiteRelease, tmpDir string) (installedPHP, note string, err error) {
	short := "php" + strings.ReplaceAll(version, ".", "")
	url := rel.AssetURLs[short]
	tarball := filepath.Join(tmpDir, short+".tar.gz")
	extractDir := filepath.Join(tmpDir, short)

	if err := u.DownloadFile(url, tarball); err != nil {
		return "", "", fmt.Errorf("download failed for PHP %s: %w", version, err)
	}
	if err := extractTarGz(tarball, extractDir); err != nil {
		return "", "", fmt.Errorf("extract failed for PHP %s: %w", version, err)
	}

	installer := filepath.Join(extractDir, "install.sh")
	info, statErr := os.Stat(installer)
	if statErr != nil || info.Mode()&0o111 == 0 {
		return "", "", fmt.Errorf("no install.sh in the PHP %s archive", version)
	}

	if res := u.run.Run(installer, "--versioned", version); !res.OK() {
		return "", "", fmt.Errorf("install.sh failed for PHP %s: %s", version, strings.TrimSpace(res.Stderr))
	}

	fpCheck := u.run.Run(filepath.Join(u.cfg.PHPBin(version), "frankenphp"), "version")
	got := u.runtimes.InstalledPHPPatch(version)
	if !fpCheck.OK() || got == "" {
		return "", "", fmt.Errorf("PHP %s does not run after the install - the previous version is in %s/php%s.backup.*", version, u.cfg.PHPRoot, version)
	}

	if avail := rel.Versions[short]; avail != "" && got != avail {
		note = fmt.Sprintf("versions.json says %s, the installed binary is %s (release changed during download?)", avail, got)
	}

	fpAvail := rel.FrankenPHP
	if fpAvail == "" {
		fpAvail = "unknown"
	}
	_ = u.runtimes.MarkerWrite(version, rel.Tag, fpAvail, got)

	backups := u.runtimes.BackupsFor(version)
	if len(backups) > 1 {
		for _, old := range backups[1:] {
			_ = os.RemoveAll(old)
		}
	}

	return got, note, nil
}

func extractTarGz(tarball, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target := filepath.Join(destDir, filepath.Clean(hdr.Name))
		if !strings.HasPrefix(target, filepath.Clean(destDir)+string(os.PathSeparator)) && target != filepath.Clean(destDir) {
			return fmt.Errorf("illegal path in archive: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		}
	}
	return nil
}
