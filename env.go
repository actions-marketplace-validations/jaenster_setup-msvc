package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// winPaths converts host paths into the form the Windows tool will understand.
//
// Native: the path already is a Windows path, so it passes through.
//
// wibo: only C: and Z: exist and both map to the host root, so a host path
// becomes a Z: path. That is why the drive-letter schemes the older wine-based
// harnesses use do not survive a move to wibo: a path like R:\src\foo.cpp
// becomes a literal directory named "R:" and every compile dies with C1083.
func winPaths(inst *Installed, paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, winPath(inst, p))
	}
	return out
}

func winPath(inst *Installed, p string) string {
	if inst.Runner != "wibo" || inst.Wibo == "" {
		return p
	}
	if o, err := exec.Command(inst.Wibo, "path", "-w", p).Output(); err == nil {
		if s := strings.TrimSpace(string(o)); s != "" {
			return s
		}
	}
	// Same answer without the subprocess: wibo maps Z: to the host root.
	return "Z:" + strings.ReplaceAll(p, "/", "\\")
}

// export sets a variable for this process and, in Actions, for later steps.
func export(name, value string) {
	logf("  %s=%s", name, value)
	os.Setenv(name, value)
	appendToFile(os.Getenv("GITHUB_ENV"), fmt.Sprintf("%s=%s\n", name, value))
}

func output(name, value string) {
	appendToFile(os.Getenv("GITHUB_OUTPUT"), fmt.Sprintf("%s=%s\n", name, value))
}

func addPath(dirs ...string) {
	path := os.Getenv("GITHUB_PATH")
	for _, d := range dirs {
		appendToFile(path, d+"\n")
	}
}

func appendToFile(path, line string) {
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(line)
}
