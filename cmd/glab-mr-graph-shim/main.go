package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const managedCommand = `exec glab-mr-graph.exe "$@"`

func main() {
	shimPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "glab-mr-graph shell shim: resolve executable: %v\n", err)
		os.Exit(127)
	}
	os.Exit(run(shimPath, os.Getenv("PATH"), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(shimPath, pathValue string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if targetArgs, ok := managedInvocation(args); ok {
		targetPath := filepath.Clean(filepath.Join(filepath.Dir(shimPath), "..", "glab-mr-graph.exe"))
		return runCommand(targetPath, targetArgs, stdin, stdout, stderr)
	}

	fallbackPath, err := findFallbackShell(shimPath, pathValue)
	if err != nil {
		fmt.Fprintln(stderr, "glab-mr-graph shell shim only supports the managed glab mr-graph alias.")
		fmt.Fprintln(stderr, "Install Git for Windows or another POSIX shell to use other glab shell aliases.")
		return 127
	}
	return runCommand(fallbackPath, args, stdin, stdout, stderr)
}

func managedInvocation(args []string) ([]string, bool) {
	if len(args) == 2 && args[0] == "-c" && args[1] == managedCommand {
		return nil, true
	}
	if len(args) >= 3 && args[0] == "-c" && args[1] == managedCommand && args[2] == "--" {
		return args[3:], true
	}
	return nil, false
}

func findFallbackShell(shimPath, pathValue string) (string, error) {
	shimPath, err := filepath.Abs(shimPath)
	if err != nil {
		return "", err
	}
	shimPath = filepath.Clean(shimPath)

	names := []string{"sh.exe"}
	if runtime.GOOS != "windows" {
		names = append(names, "sh")
	}
	for _, directory := range filepath.SplitList(pathValue) {
		if directory == "" {
			continue
		}
		for _, name := range names {
			candidate, err := filepath.Abs(filepath.Join(directory, name))
			if err != nil || samePath(candidate, shimPath) {
				continue
			}
			info, err := os.Stat(candidate)
			if err == nil && !info.IsDir() {
				return candidate, nil
			}
		}
	}
	return "", errors.New("no fallback shell found")
}

func samePath(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func runCommand(path string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	command := newCommand(path, args, stdin, stdout, stderr)
	if err := command.Run(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return exitError.ExitCode()
		}
		fmt.Fprintf(stderr, "glab-mr-graph shell shim: %v\n", err)
		return 127
	}
	return 0
}

func newCommand(path string, args []string, stdin io.Reader, stdout, stderr io.Writer) *exec.Cmd {
	command := exec.Command(path, args...)
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	return command
}
