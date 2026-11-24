package notify

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/value-quest/igor/internal/runner"
)

//go:embed igor.icns
var iconICNS []byte

const (
	bundleMarker = ".igor-icon-v2"
	msgFileName  = ".notify-msg"
)

// AppPath returns the path to the helper Igor.app bundle inside igorHome.
func AppPath(igorHome string) string {
	return filepath.Join(igorHome, "Igor.app")
}

// EnsureAppBundle builds ~/.igor/Igor.app with the embedded Igor icon so macOS
// notifications display the custom Igor icon instead of Script Editor's icon.
func EnsureAppBundle(igorHome string, run runner.Runner) (string, error) {
	appPath := AppPath(igorHome)
	markerPath := filepath.Join(appPath, "Contents", "Resources", bundleMarker)
	if _, err := os.Stat(markerPath); err == nil {
		return appPath, nil
	}

	if err := os.MkdirAll(igorHome, 0o755); err != nil {
		return "", err
	}
	_ = os.RemoveAll(appPath)

	msgPath := filepath.Join(igorHome, msgFileName)
	script := fmt.Sprintf(`on run
	try
		set msg to do shell script "cat " & quoted form of %q
		if msg is not "" then
			display notification msg with title "igor"
		end if
	end try
end run
`, msgPath)

	res := run.Run("osacompile", "-o", appPath, "-e", script)
	if !res.OK() {
		return "", fmt.Errorf("osacompile failed: %s", strings.TrimSpace(res.Stderr))
	}

	resDir := filepath.Join(appPath, "Contents", "Resources")
	// Remove macOS's compiled Assets.car and CFBundleIconName so applet.icns is used instead of the default scroll icon.
	_ = os.Remove(filepath.Join(resDir, "Assets.car"))
	if err := os.WriteFile(filepath.Join(resDir, "applet.icns"), iconICNS, 0o644); err != nil {
		return "", err
	}

	infoPlist := filepath.Join(appPath, "Contents", "Info.plist")
	run.Run("plutil", "-remove", "CFBundleIconName", infoPlist)
	run.Run("plutil", "-replace", "CFBundleIdentifier", "-string", "dev.igor.notifier", infoPlist)
	run.Run("plutil", "-replace", "CFBundleName", "-string", "igor", infoPlist)
	run.Run("plutil", "-replace", "LSUIElement", "-bool", "true", infoPlist)
	run.Run("codesign", "--force", "--deep", "--sign", "-", appPath)
	run.Run("/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister", "-f", appPath)

	_ = os.WriteFile(markerPath, []byte("1\n"), 0o644)
	return appPath, nil
}

// Send delivers a macOS notification using Igor.app (with the custom Igor icon),
// falling back to osascript if needed.
func Send(igorHome string, run runner.Runner, msg string) {
	if !run.IsFake() {
		if appPath, err := EnsureAppBundle(igorHome, run); err == nil {
			msgPath := filepath.Join(igorHome, msgFileName)
			if err := os.WriteFile(msgPath, []byte(msg), 0o644); err == nil {
				if run.Run("open", "-g", "-n", appPath).OK() {
					return
				}
			}
		}
	}

	escaped := strings.ReplaceAll(msg, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	run.Run("osascript", "-e", fmt.Sprintf(`display notification "%s" with title "igor"`, escaped))
}
