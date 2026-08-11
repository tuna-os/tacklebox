package runner

import (
	"strings"
	"testing"
)

// These tests exercise Output/RunCombined with real child processes
// (echo/sh only — no sudo, no flatpak) to cover the exec wrappers the
// earlier tests only exercised via HostArgs.

func TestOutput_SuccessReturnsStdout(t *testing.T) {
	out, err := Output("echo", "hello")
	if err != nil {
		t.Fatalf("Output(echo) error: %v", err)
	}
	if string(out) != "hello\n" {
		t.Errorf("Output = %q, want %q", out, "hello\n")
	}
}

func TestOutput_ErrorIncludesStderrTail(t *testing.T) {
	_, err := Output("sh", "-c", "echo boom >&2; exit 3")
	if err == nil {
		t.Fatal("expected error for failing command")
	}
	msg := err.Error()
	if !strings.Contains(msg, "sh -c") {
		t.Errorf("error should include command, got: %v", msg)
	}
	if !strings.Contains(msg, "boom") {
		t.Errorf("error should include stderr tail, got: %v", msg)
	}
	if !strings.Contains(msg, "exit status 3") {
		t.Errorf("error should include exit status, got: %v", msg)
	}
}

func TestOutput_FullStderrPreserved(t *testing.T) {
	// Contract as implemented: outputImpl embeds the WHOLE stderr tail in
	// the error, unlike DefaultRun which truncates to 400 chars. This test
	// pins that behavior; see tuna-os/tacklebox#196 (DefaultRun truncates,
	// outputImpl does not) for the proposed unification.
	long := strings.Repeat("x", 600)
	_, err := Output("sh", "-c", "echo "+long+" >&2; exit 1")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), strings.Repeat("x", 600)) {
		t.Errorf("outputImpl should include the full stderr tail, got: %.140s", err)
	}
}

func TestRunCombined_Success(t *testing.T) {
	out, err := RunCombined("echo", "combined")
	if err != nil {
		t.Fatalf("RunCombined(echo) error: %v", err)
	}
	if string(out) != "combined\n" {
		t.Errorf("RunCombined = %q, want %q", out, "combined\n")
	}
}

func TestRunCombined_NonZeroExitReturnsOutput(t *testing.T) {
	out, err := RunCombined("sh", "-c", "echo on-stdout; echo on-stderr >&2; exit 4")
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	if !strings.Contains(string(out), "on-stdout") {
		t.Errorf("combined output missing stdout, got: %q", out)
	}
	if !strings.Contains(string(out), "on-stderr") {
		t.Errorf("combined output missing stderr, got: %q", out)
	}
	if !strings.Contains(err.Error(), "exit status 4") {
		t.Errorf("error = %v, want exit status 4", err)
	}
}
