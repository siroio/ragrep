package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
)

// Set by release tooling with -ldflags; defaults keep local builds usable.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

type buildMetadata struct {
	Version     string `json:"version"`
	Commit      string `json:"commit"`
	BuildDate   string `json:"build_date"`
	GoVersion   string `json:"go_version"`
	VCSModified string `json:"vcs_modified,omitempty"`
}

func currentBuildMetadata() buildMetadata {
	m := buildMetadata{Version: version, Commit: commit, BuildDate: buildDate, GoVersion: runtime.Version()}
	if info, ok := debug.ReadBuildInfo(); ok && info != nil {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if m.Commit == "unknown" && setting.Value != "" {
					m.Commit = setting.Value
				}
			case "vcs.modified":
				m.VCSModified = setting.Value
			}
		}
	}
	return m
}

func cmdVersion(args []string) int {
	if len(args) != 0 {
		return fail(fmt.Errorf("usage: ragrep version"))
	}
	m := currentBuildMetadata()
	fmt.Fprintf(os.Stdout, "ragrep %s\nversion: %s\ncommit: %s\nbuild date: %s\ngo: %s\n", m.Version, m.Version, m.Commit, m.BuildDate, m.GoVersion)
	if m.VCSModified != "" {
		fmt.Fprintf(os.Stdout, "vcs modified: %s\n", m.VCSModified)
	}
	return 0
}
