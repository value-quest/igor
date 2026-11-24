package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error
}

func (r Result) OK() bool {
	return r.ExitCode == 0 && r.Err == nil
}

type Opts struct {
	Dir         string
	Env         []string
	Stdin       io.Reader
	Interactive bool
	Timeout     time.Duration
}

type Runner interface {
	Run(name string, args ...string) Result
	RunWith(opts Opts, name string, args ...string) Result
	LookPath(file string, extraPaths ...string) (string, error)
	PortInUse(port string) bool
	IsFake() bool
}

type ExecRunner struct{}

func New() *ExecRunner { return &ExecRunner{} }

func (e *ExecRunner) IsFake() bool { return false }

func (e *ExecRunner) Run(name string, args ...string) Result {
	return e.RunWith(Opts{}, name, args...)
}

func (e *ExecRunner) RunWith(opts Opts, name string, args ...string) Result {
	var cmd *exec.Cmd
	if opts.Timeout > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
		defer cancel()
		cmd = exec.CommandContext(ctx, name, args...)
	} else {
		cmd = exec.Command(name, args...)
	}
	if opts.Dir != "" {
		cmd.Dir = opts.Dir
	}
	if len(opts.Env) > 0 {
		cmd.Env = append(os.Environ(), opts.Env...)
	}

	var stdout, stderr bytes.Buffer
	if opts.Interactive {
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	} else {
		if opts.Stdin != nil {
			cmd.Stdin = opts.Stdin
		}
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
	}

	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			code = 1
		}
	}

	return Result{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: code,
		Err:      err,
	}
}

func (e *ExecRunner) LookPath(file string, extraPaths ...string) (string, error) {
	if len(extraPaths) == 0 {
		return exec.LookPath(file)
	}
	orig := os.Getenv("PATH")
	combined := strings.Join(extraPaths, ":") + ":" + orig
	for _, dir := range strings.Split(combined, ":") {
		if dir == "" {
			continue
		}
		candidate := dir + "/" + file
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", exec.ErrNotFound
}

func (e *ExecRunner) PortInUse(port string) bool {
	return e.Run("lsof", "-nP", "-iTCP:"+port, "-sTCP:LISTEN").OK()
}
