// Package buildinfo carries the identity of the running binary: which version,
// which commit, built when, with which toolchain, for which platform.
//
// # Why this package exists
//
// deploy/docker/go.Dockerfile has stamped `-X` linker flags at these exact
// symbol paths since phase 0, and they were INERT: the Go linker silently
// ignores `-X` for a symbol that does not exist, so every image built between
// phase 0 and phase 3 reported nothing at all about what it was. `grep -a` on a
// built binary found no trace of the version that was passed to the build. The
// stamps cost nothing and did nothing.
//
// The symbol paths below are therefore NOT free to rename. The Dockerfile passes
//
//	-X github.com/anpl1623/sharpline/internal/platform/buildinfo.Version=…
//	-X github.com/anpl1623/sharpline/internal/platform/buildinfo.Commit=…
//	-X github.com/anpl1623/sharpline/internal/platform/buildinfo.BuildDate=…
//
// and a rename here reverts the stamps to being inert without breaking any
// build — which is the failure mode that made this package necessary in the
// first place. If these names ever change, the Dockerfile changes in the same
// commit.
//
// # The global-variable exception
//
// CLAUDE.md §12 says "no global mutable state". These three variables are
// package-level and writable, which is the one shape `-ldflags -X` can reach:
// the linker rewrites the string data of a package-level `var` at link time and
// has no way to call a constructor. The exception is bounded rather than open:
//
//   - Nothing in this repository assigns to them. They are written once, by the
//     linker, before main runs.
//   - Everything else in the package is a pure function over a VALUE type.
//     [Read] takes a snapshot and every consumer holds an [Info], so no caller
//     depends on a variable that could change under it.
//
// Read the snapshot; do not read the variables. That is the whole convention.
package buildinfo

import (
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"time"
)

// Stamped by the linker. See the package comment before touching these — the
// names are a contract with deploy/docker/go.Dockerfile.
//
//nolint:gochecknoglobals // -ldflags -X can only write a package-level var.
var (
	// Version is the release identity: `git describe --tags --always --dirty`.
	Version string
	// Commit is the full git revision the image was built from.
	Commit string
	// BuildDate is the build instant in RFC 3339, UTC.
	BuildDate string
)

// Unknown is what an unstamped field reads as.
//
// A `go run` or `go test` binary carries no stamps at all, and that is the
// common case during development — every field is empty and every one of them
// reports Unknown. Reporting the empty string instead would produce log lines
// and metric labels with silently missing values, which reads as a collection
// bug rather than as "this binary was not built by the release path".
const Unknown = "unknown"

// shortCommitLen is how much of a revision is enough to identify it by eye in a
// log line. Git's own default abbreviation is 7; 12 is what its documentation
// recommends for a repository expected to outgrow 7, and it is what the log
// line and the metric label both use.
const shortCommitLen = 12

// Info is an immutable snapshot of the binary's identity.
type Info struct {
	// Version, Commit and BuildDate are the linker stamps, normalized: an
	// unstamped field reads Unknown rather than empty.
	Version   string
	Commit    string
	BuildDate string

	// ShortCommit is Commit abbreviated to shortCommitLen, or Commit itself
	// when it is already shorter (or unstamped).
	ShortCommit string

	// GoVersion, OS and Arch come from the runtime rather than from a stamp,
	// so they are correct even in an unstamped binary. They answer the
	// question a multi-arch build makes worth asking: the deploy target is
	// arm64 (see the ledger's deploy-target section) and an amd64 binary
	// running under emulation is a performance mystery nobody wants to debug
	// twice.
	GoVersion string
	OS        string
	Arch      string

	// Stamped reports whether the release path built this binary — true when
	// every one of the three linker stamps carried a value.
	Stamped bool
}

// Read returns the snapshot. It is safe to call from anywhere and returns the
// same value every time within one process.
func Read() Info {
	version := orUnknown(Version)
	commit := orUnknown(Commit)

	return Info{
		Version:     version,
		Commit:      commit,
		BuildDate:   normalizeDate(BuildDate),
		ShortCommit: shorten(commit),
		GoVersion:   runtime.Version(),
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		Stamped:     Version != "" && Commit != "" && BuildDate != "",
	}
}

// Platform is the GOOS/GOARCH pair the binary was compiled for, as one string.
func (i Info) Platform() string { return i.OS + "/" + i.Arch }

// BuildTime parses BuildDate. The second return is false when the field is
// unstamped or is not RFC 3339 — the raw string is still reported by [Info.String]
// and by the metric either way, because a malformed stamp is information about
// the build pipeline and hiding it would not help anyone.
func (i Info) BuildTime() (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, i.BuildDate)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// String renders the one-line human form, for `docker logs` and for an error
// message that needs to say which build produced it.
func (i Info) String() string {
	return fmt.Sprintf("%s (%s) built %s with %s for %s",
		i.Version, i.ShortCommit, i.BuildDate, i.GoVersion, i.Platform())
}

// LogValue implements [slog.LogValuer], so a caller writes
// slog.Any("build", buildinfo.Read()) and gets a nested group rather than a
// stringified struct. The group is what makes `version` and `commit` queryable
// fields in a log backend instead of substrings of a message.
//
// `stamped` is in the group on purpose. Without it a reader cannot tell an
// unstamped development binary from a release build whose stamps went missing,
// and those two need very different responses.
func (i Info) LogValue() slog.Value {
	attrs := []slog.Attr{
		slog.String("version", i.Version),
		slog.String("commit", i.Commit),
		slog.String("build_date", i.BuildDate),
		slog.String("go_version", i.GoVersion),
		slog.String("platform", i.Platform()),
		slog.Bool("stamped", i.Stamped),
	}
	return slog.GroupValue(attrs...)
}

func orUnknown(s string) string {
	if s == "" {
		return Unknown
	}
	return s
}

// normalizeDate reports a parseable stamp in one canonical spelling — RFC 3339
// in UTC — so two images built in different timezones do not produce two label
// values for the same instant, which would make the metric's series count grow
// with the build machine's locale. An unparseable stamp is passed through
// verbatim rather than replaced with Unknown: "2026-08-17" is wrong for this
// field but it is still evidence about the pipeline that produced it.
func normalizeDate(s string) string {
	if s == "" {
		return Unknown
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.UTC().Format(time.RFC3339)
}

func shorten(commit string) string {
	if commit == Unknown || len(commit) <= shortCommitLen {
		return commit
	}
	// Only abbreviate something that looks like a hex object name. `git
	// describe --always --dirty` output and a branch name are not improved by
	// being cut in half.
	if strings.TrimLeft(strings.ToLower(commit), "0123456789abcdef") != "" {
		return commit
	}
	return commit[:shortCommitLen]
}
