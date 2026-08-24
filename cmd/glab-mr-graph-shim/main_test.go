package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestManagedInvocation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
		ok   bool
	}{
		{name: "no arguments", args: []string{"-c", managedCommand}, ok: true},
		{name: "forward arguments", args: []string{"-c", managedCommand, "--", "two words", `quote"inside`, "日本語", "--hostname", "gitlab.example.com"}, want: []string{"two words", `quote"inside`, "日本語", "--hostname", "gitlab.example.com"}, ok: true},
		{name: "different command", args: []string{"-c", `echo unsafe`}},
		{name: "missing separator", args: []string{"-c", managedCommand, "argument"}},
		{name: "extra no-argument token", args: []string{"-c", managedCommand, "--", ""}, want: []string{""}, ok: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := managedInvocation(test.args)
			if ok != test.ok {
				t.Fatalf("managedInvocation() ok = %v, want %v", ok, test.ok)
			}
			if strings.Join(got, "\x00") != strings.Join(test.want, "\x00") {
				t.Fatalf("managedInvocation() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestRunManagedCommandForwardsIOArgumentsAndExitCode(t *testing.T) {
	shimPath := installHelperBinary(t)
	t.Setenv("GLAB_MR_GRAPH_SHIM_HELPER", "1")
	t.Setenv("GLAB_MR_GRAPH_SHIM_HELPER_EXIT", "23")

	forwarded := []string{"-test.run=TestShimHelperProcess", "--", "two words", `quote"inside`, "日本語", "--hostname", "gitlab.example.com"}
	args := append([]string{"-c", managedCommand, "--"}, forwarded...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(shimPath, filepath.Dir(shimPath), args, strings.NewReader("stdin-data"), &stdout, &stderr)

	if exitCode != 23 {
		t.Fatalf("run() exit code = %d, want 23; stderr: %s", exitCode, stderr.String())
	}
	var payload helperPayload
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode helper output: %v; output: %q", err, stdout.String())
	}
	if strings.Join(payload.Args, "\x00") != strings.Join(forwarded[2:], "\x00") {
		t.Fatalf("forwarded arguments = %#v, want %#v", payload.Args, forwarded[2:])
	}
	if payload.Stdin != "stdin-data" {
		t.Fatalf("forwarded stdin = %q, want stdin-data", payload.Stdin)
	}
	if !strings.Contains(stderr.String(), "helper-stderr") {
		t.Fatalf("stderr was not forwarded: %q", stderr.String())
	}
}

func TestNewCommandPreservesConsoleAndIO(t *testing.T) {
	stdin := strings.NewReader("stdin")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command := newCommand("fixture", []string{"argument"}, stdin, &stdout, &stderr)
	if command.Stdin != stdin || command.Stdout != &stdout || command.Stderr != &stderr {
		t.Fatal("newCommand() did not attach the parent standard streams")
	}
	if command.SysProcAttr != nil {
		t.Fatal("newCommand() detached the child from the parent console process group")
	}
}

func TestRunRejectsUnknownCommandWithoutFallback(t *testing.T) {
	shimPath := filepath.Join(t.TempDir(), "runtime", "sh.exe")
	var stderr bytes.Buffer
	exitCode := run(shimPath, filepath.Dir(shimPath), []string{"-c", "echo unsafe"}, strings.NewReader(""), io.Discard, &stderr)
	if exitCode != 127 {
		t.Fatalf("run() exit code = %d, want 127", exitCode)
	}
	if !strings.Contains(stderr.String(), "only supports the managed") {
		t.Fatalf("unexpected error: %q", stderr.String())
	}
}

func TestRunForwardsUnknownCommandToLaterShell(t *testing.T) {
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "runtime")
	fallbackDir := filepath.Join(root, "fallback")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fallbackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shimPath := filepath.Join(runtimeDir, "sh.exe")
	if err := os.WriteFile(shimPath, []byte("shim"), 0o755); err != nil {
		t.Fatal(err)
	}
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	copyExecutable(t, testExecutable, filepath.Join(fallbackDir, "sh.exe"))
	t.Setenv("GLAB_MR_GRAPH_SHIM_HELPER", "1")
	t.Setenv("GLAB_MR_GRAPH_SHIM_HELPER_EXIT", "19")

	args := []string{"-test.run=TestShimHelperProcess", "--", "fallback argument"}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run(shimPath, strings.Join([]string{runtimeDir, fallbackDir}, string(os.PathListSeparator)), args, strings.NewReader("fallback-stdin"), &stdout, &stderr)
	if exitCode != 19 {
		t.Fatalf("run() exit code = %d, want 19; stderr: %s", exitCode, stderr.String())
	}
	var payload helperPayload
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode helper output: %v; output: %q", err, stdout.String())
	}
	if strings.Join(payload.Args, "\x00") != "fallback argument" || payload.Stdin != "fallback-stdin" {
		t.Fatalf("fallback forwarding = %#v, want argument and stdin preserved", payload)
	}
}

func TestFindFallbackShellSkipsShim(t *testing.T) {
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "runtime")
	fallbackDir := filepath.Join(root, "fallback")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fallbackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shimPath := filepath.Join(runtimeDir, "sh.exe")
	fallbackPath := filepath.Join(fallbackDir, "sh.exe")
	for _, path := range []string{shimPath, fallbackPath} {
		if err := os.WriteFile(path, []byte("fixture"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	got, err := findFallbackShell(shimPath, strings.Join([]string{runtimeDir, fallbackDir}, string(os.PathListSeparator)))
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(got, fallbackPath) {
		t.Fatalf("findFallbackShell() = %q, want %q", got, fallbackPath)
	}
}

type helperPayload struct {
	Args  []string `json:"args"`
	Stdin string   `json:"stdin"`
}

func TestShimHelperProcess(t *testing.T) {
	if os.Getenv("GLAB_MR_GRAPH_SHIM_HELPER") != "1" {
		return
	}
	separator := 0
	for separator < len(os.Args) && os.Args[separator] != "--" {
		separator++
	}
	args := []string{}
	if separator < len(os.Args) {
		args = os.Args[separator+1:]
	}
	stdin, _ := io.ReadAll(os.Stdin)
	_ = json.NewEncoder(os.Stdout).Encode(helperPayload{Args: args, Stdin: string(stdin)})
	_, _ = io.WriteString(os.Stderr, "helper-stderr\n")
	exitCode, _ := strconv.Atoi(os.Getenv("GLAB_MR_GRAPH_SHIM_HELPER_EXIT"))
	os.Exit(exitCode)
}

func installHelperBinary(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shimPath := filepath.Join(runtimeDir, "sh.exe")
	if err := os.WriteFile(shimPath, []byte("shim"), 0o755); err != nil {
		t.Fatal(err)
	}
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(root, "glab-mr-graph.exe")
	copyExecutable(t, testExecutable, targetPath)
	return shimPath
}

func copyExecutable(t *testing.T, source, target string) {
	t.Helper()
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(target, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}
