// Package buildinfo exposes release metadata injected by the build pipeline.
package buildinfo

import "runtime/debug"

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// Info returns the configured release version, falling back to Go module build
// information for locally installed development builds.
func Info() (version, commit, date string) {
	version, commit, date = Version, Commit, Date
	if Version != "dev" {
		return version, commit, date
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version, commit, date
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			commit = setting.Value
		case "vcs.time":
			date = setting.Value
		case "vcs.modified":
			if setting.Value == "true" {
				commit += "+dirty"
			}
		}
	}
	return version, commit, date
}
