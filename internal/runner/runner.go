package runner

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

var inFlatpak = sync.OnceValue(func() bool {
	_, err := os.Stat("/.flatpak-info")
	return err == nil
})

// Verbose toggles whether the "+ cmd args" trace and child stdout/stderr are
// streamed to the parent. Defaults to true to preserve existing behavior; the
// CLI lowers this when --verbose is not set.
var Verbose = true

func HostArgs(name string, args []string) (string, []string) {
	if inFlatpak() {
		return "flatpak-spawn", append([]string{"--host", name}, args...)
	}
	return name, args
}

func truncateStderrTail(b []byte) string {
	tail := strings.TrimSpace(string(b))
	if len(tail) > 400 {
		return "..." + tail[len(tail)-400:]
	}
	return tail
}

func DefaultRun(stdin io.Reader, name string, args ...string) error {
	name, args = HostArgs(name, args)
	cmd := exec.Command(name, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}

	// Always capture stderr so failure messages have context, even in quiet mode.
	var stderrBuf bytes.Buffer
	if Verbose {
		cmd.Stdout = os.Stdout
		cmd.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)
		fmt.Fprintf(os.Stdout, "+ %s %s\n", name, strings.Join(args, " "))
	} else {
		cmd.Stdout = io.Discard
		cmd.Stderr = &stderrBuf
	}

	if err := cmd.Run(); err != nil {
		tail := truncateStderrTail(stderrBuf.Bytes())
		if tail != "" {
			return fmt.Errorf("%s %s: %w\nstderr: %s", name, strings.Join(args, " "), err, tail)
		}
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

var RunFn = DefaultRun

func Run(name string, args ...string) error {
	return RunFn(nil, name, args...)
}

func RunWithStdin(stdin io.Reader, name string, args ...string) error {
	return RunFn(stdin, name, args...)
}

// DefaultRunStreamed is DefaultRun with the child's stdout/stderr ALWAYS
// streamed to the parent, regardless of Verbose (only the "+ cmd" trace
// stays Verbose-gated). Exists for steps whose output is the diagnosis —
// above all the live-customize container, which runs consumer scripts doing
// package installs and flatpak pulls: under quiet mode DefaultRun discards
// stdout and sits on stderr until exit, which turned a wedged customize into
// 87 minutes of dead silence ending in a bare job-timeout cancellation
// (tuna-os/tunaOS#1772). A step that can legitimately run for minutes on
// external I/O must narrate, quiet mode or not.
func DefaultRunStreamed(stdin io.Reader, name string, args ...string) error {
	name, args = HostArgs(name, args)
	cmd := exec.Command(name, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var stderrBuf bytes.Buffer
	cmd.Stdout = os.Stdout
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)
	if Verbose {
		fmt.Fprintf(os.Stdout, "+ %s %s\n", name, strings.Join(args, " "))
	}
	if err := cmd.Run(); err != nil {
		tail := truncateStderrTail(stderrBuf.Bytes())
		if tail != "" {
			return fmt.Errorf("%s %s: %w\nstderr: %s", name, strings.Join(args, " "), err, tail)
		}
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

var RunStreamedFn = DefaultRunStreamed

func RunStreamed(name string, args ...string) error {
	return RunStreamedFn(nil, name, args...)
}

var OutputFn = outputImpl

func Output(name string, args ...string) ([]byte, error) {
	return OutputFn(name, args...)
}

func outputImpl(name string, args ...string) ([]byte, error) {
	name, args = HostArgs(name, args)
	cmd := exec.Command(name, args...)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	out, err := cmd.Output()
	if err != nil {
		tail := truncateStderrTail(stderrBuf.Bytes())
		if tail != "" {
			return nil, fmt.Errorf("%s %s: %w\nstderr: %s", name, strings.Join(args, " "), err, tail)
		}
		return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

var RunCombinedFn = runCombinedImpl

// RunCombined runs a command and returns combined stdout+stderr regardless of
// exit status, plus the error. Useful for commands like sgdisk where the
// caller wants to inspect output to decide whether a non-zero exit is fatal.
func RunCombined(name string, args ...string) ([]byte, error) {
	return RunCombinedFn(name, args...)
}

func runCombinedImpl(name string, args ...string) ([]byte, error) {
	name, args = HostArgs(name, args)
	cmd := exec.Command(name, args...)
	if Verbose {
		fmt.Fprintf(os.Stdout, "+ %s %s\n", name, strings.Join(args, " "))
	}
	return cmd.CombinedOutput()
}
