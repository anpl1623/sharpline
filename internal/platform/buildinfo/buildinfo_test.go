package buildinfo

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// stamp sets the linker-stamped variables for the duration of one test and
// restores them afterwards.
//
// This is the ONLY writer to those variables in the repository, and it is a test
// helper on purpose: the production convention is that the linker writes them
// once before main and nothing else ever does. t.Cleanup restores the previous
// values so the tests do not leak state into each other, which matters because
// these are package-level (see the package comment's global-variable exception).
func stamp(t *testing.T, version, commit, buildDate string) {
	t.Helper()
	prevVersion, prevCommit, prevDate := Version, Commit, BuildDate
	t.Cleanup(func() { Version, Commit, BuildDate = prevVersion, prevCommit, prevDate })
	Version, Commit, BuildDate = version, commit, buildDate
}

func TestReadNormalizesTheStamps(t *testing.T) {
	tests := []struct {
		name        string
		version     string
		commit      string
		buildDate   string
		wantVersion string
		wantCommit  string
		wantShort   string
		wantDate    string
		wantStamped bool
	}{
		{
			name:      "a release build",
			version:   "v0.3.0-phase3",
			commit:    "07796dc4f1a2b3c4d5e6f708192a3b4c5d6e7f80",
			buildDate: "2026-08-17T09:15:00Z",

			wantVersion: "v0.3.0-phase3",
			wantCommit:  "07796dc4f1a2b3c4d5e6f708192a3b4c5d6e7f80",
			wantShort:   "07796dc4f1a2",
			wantDate:    "2026-08-17T09:15:00Z",
			wantStamped: true,
		},
		{
			// `go run` / `go test` / a `go build` with no ldflags. Every field
			// is empty and every one must read Unknown rather than "".
			name:        "an unstamped development binary",
			wantVersion: Unknown,
			wantCommit:  Unknown,
			wantShort:   Unknown,
			wantDate:    Unknown,
			wantStamped: false,
		},
		{
			// The Dockerfile's own ARG defaults. Every field carries a value, so
			// the binary reports as stamped — which is correct and is exactly
			// what a `docker build` with no --build-arg produces.
			name:        "the Dockerfile defaults",
			version:     "dev",
			commit:      "unknown",
			buildDate:   "1970-01-01T00:00:00Z",
			wantVersion: "dev",
			wantCommit:  "unknown",
			wantShort:   "unknown",
			wantDate:    "1970-01-01T00:00:00Z",
			wantStamped: true,
		},
		{
			// One stamp missing is not "stamped". Partial stamping is a broken
			// build pipeline and must not read as a release build.
			name:        "a partial stamp",
			version:     "v0.3.0",
			commit:      "",
			buildDate:   "2026-08-17T09:15:00Z",
			wantVersion: "v0.3.0",
			wantCommit:  Unknown,
			wantShort:   Unknown,
			wantDate:    "2026-08-17T09:15:00Z",
			wantStamped: false,
		},
		{
			// A non-UTC stamp is canonicalized so two build machines in two
			// timezones do not produce two label values for one instant.
			name:        "a build date in a non-UTC offset",
			version:     "v0.3.0",
			commit:      "abc123",
			buildDate:   "2026-08-17T11:15:00+02:00",
			wantVersion: "v0.3.0",
			wantCommit:  "abc123",
			wantShort:   "abc123",
			wantDate:    "2026-08-17T09:15:00Z",
			wantStamped: true,
		},
		{
			// Not RFC 3339, so passed through verbatim. It is wrong for this
			// field, and that is precisely why it must stay visible.
			name:        "a malformed build date survives verbatim",
			version:     "v0.3.0",
			commit:      "abc123",
			buildDate:   "2026-08-17",
			wantVersion: "v0.3.0",
			wantCommit:  "abc123",
			wantShort:   "abc123",
			wantDate:    "2026-08-17",
			wantStamped: true,
		},
		{
			// `git describe --dirty` is not a hex object name, so abbreviating
			// it would cut off the part that carries the meaning.
			name:        "a describe-style revision is not abbreviated",
			version:     "v0.3.0-4-gdeadbeef-dirty",
			commit:      "v0.3.0-4-gdeadbeef-dirty",
			buildDate:   "2026-08-17T09:15:00Z",
			wantVersion: "v0.3.0-4-gdeadbeef-dirty",
			wantCommit:  "v0.3.0-4-gdeadbeef-dirty",
			wantShort:   "v0.3.0-4-gdeadbeef-dirty",
			wantDate:    "2026-08-17T09:15:00Z",
			wantStamped: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stamp(t, tc.version, tc.commit, tc.buildDate)

			got := Read()
			if got.Version != tc.wantVersion {
				t.Errorf("Version = %q, want %q", got.Version, tc.wantVersion)
			}
			if got.Commit != tc.wantCommit {
				t.Errorf("Commit = %q, want %q", got.Commit, tc.wantCommit)
			}
			if got.ShortCommit != tc.wantShort {
				t.Errorf("ShortCommit = %q, want %q", got.ShortCommit, tc.wantShort)
			}
			if got.BuildDate != tc.wantDate {
				t.Errorf("BuildDate = %q, want %q", got.BuildDate, tc.wantDate)
			}
			if got.Stamped != tc.wantStamped {
				t.Errorf("Stamped = %t, want %t", got.Stamped, tc.wantStamped)
			}

			// The runtime fields are correct in an unstamped binary too, which
			// is the reason they are not stamps.
			if got.GoVersion != runtime.Version() {
				t.Errorf("GoVersion = %q, want %q", got.GoVersion, runtime.Version())
			}
			if want := runtime.GOOS + "/" + runtime.GOARCH; got.Platform() != want {
				t.Errorf("Platform() = %q, want %q", got.Platform(), want)
			}

			// Nothing in Read may write back to the stamped variables.
			if Version != tc.version || Commit != tc.commit || BuildDate != tc.buildDate {
				t.Errorf("Read() mutated the stamped variables: %q/%q/%q", Version, Commit, BuildDate)
			}
		})
	}
}

func TestBuildTime(t *testing.T) {
	stamp(t, "v1", "abc", "2026-08-17T09:15:00+02:00")
	got, ok := Read().BuildTime()
	if !ok {
		t.Fatalf("BuildTime() reported the stamp unparseable")
	}
	want := time.Date(2026, 8, 17, 7, 15, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("BuildTime() = %s, want %s", got, want)
	}
	if got.Location() != time.UTC {
		t.Errorf("BuildTime() location = %s, want UTC", got.Location())
	}

	for _, bad := range []string{"", "2026-08-17", "not a date"} {
		stamp(t, "v1", "abc", bad)
		if _, ok := Read().BuildTime(); ok {
			t.Errorf("BuildTime() accepted %q", bad)
		}
	}
}

func TestStringAndLogValueCarryEveryField(t *testing.T) {
	stamp(t, "v0.3.0-phase3", "07796dc4f1a2b3c4d5e6f708192a3b4c5d6e7f80", "2026-08-17T09:15:00Z")
	info := Read()

	// String is the human line. It must name the build, not merely exist.
	s := info.String()
	for _, want := range []string{"v0.3.0-phase3", "07796dc4f1a2", "2026-08-17T09:15:00Z", runtime.Version()} {
		if !strings.Contains(s, want) {
			t.Errorf("String() = %q, missing %q", s, want)
		}
	}

	// LogValue must be a GROUP, because the point of it is that `version` and
	// `commit` are queryable fields in a log backend rather than substrings.
	v := info.LogValue()
	attrs := v.Group()
	if len(attrs) == 0 {
		t.Fatalf("LogValue() = %v, want a group", v)
	}
	seen := make(map[string]string, len(attrs))
	for _, a := range attrs {
		seen[a.Key] = a.Value.String()
	}
	for key, want := range map[string]string{
		"version":    "v0.3.0-phase3",
		"commit":     "07796dc4f1a2b3c4d5e6f708192a3b4c5d6e7f80",
		"build_date": "2026-08-17T09:15:00Z",
		"go_version": runtime.Version(),
		"platform":   runtime.GOOS + "/" + runtime.GOARCH,
		"stamped":    "true",
	} {
		got, ok := seen[key]
		if !ok {
			t.Errorf("LogValue() has no %q attribute; got %v", key, seen)
			continue
		}
		if got != want {
			t.Errorf("LogValue()[%q] = %q, want %q", key, got, want)
		}
	}
}

// TestCollectorExportsOneSeriesPerService pins the metric NAME and the LABEL SET,
// both of which are a contract: deploy/observability scrapes by name, and a query
// like `count by (version) (sharpline_build_info)` breaks if a label is renamed.
func TestCollectorExportsOneSeriesPerService(t *testing.T) {
	stamp(t, "v0.3.0-phase3", "07796dc4f1a2b3c4d5e6f708192a3b4c5d6e7f80", "2026-08-17T09:15:00Z")

	reg := prometheus.NewRegistry()
	if err := Register(reg, "ingest"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if got := testutil.CollectAndCount(reg, "sharpline_build_info"); got != 1 {
		t.Fatalf("sharpline_build_info has %d series, want exactly 1", got)
	}

	want := `# HELP sharpline_build_info Identity of the running binary, as label values on a constant 1. ` +
		`Panels: count by (version) (sharpline_build_info) during a rolling deploy; ` +
		"`* on(service) group_left(version) sharpline_build_info` to annotate any other series with its build. " +
		`stamped="false" means the binary was not produced by deploy/docker/go.Dockerfile, so version and commit are "unknown".
# TYPE sharpline_build_info gauge
sharpline_build_info{build_date="2026-08-17T09:15:00Z",commit="07796dc4f1a2b3c4d5e6f708192a3b4c5d6e7f80",go_version="` +
		runtime.Version() + `",platform="` + runtime.GOOS + "/" + runtime.GOARCH +
		`",service="ingest",stamped="true",version="v0.3.0-phase3"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "sharpline_build_info"); err != nil {
		t.Errorf("sharpline_build_info exposition differs: %v", err)
	}
}

func TestRegisterIsFailFastAndNilTolerant(t *testing.T) {
	// migrate has no registry: a nil Registerer must be a no-op, not a panic.
	if err := Register(nil, "migrate"); err != nil {
		t.Errorf("Register(nil, …) = %v, want nil", err)
	}

	// Two collectors for one service on one registry is a programming error and
	// must surface at startup rather than produce a silently doubled series.
	reg := prometheus.NewRegistry()
	if err := Register(reg, "api"); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := Register(reg, "api"); err == nil {
		t.Errorf("second Register on the same registry succeeded; want a duplicate-registration error")
	}
}

// TestUnstampedBinaryStillExportsAMetric is the case every locally built binary
// hits. The series must exist with stamped="false" rather than be omitted: a
// missing series is indistinguishable from a service that is not running, and
// "which of my pods is on an unstamped build" is exactly the question this metric
// is here to answer.
func TestUnstampedBinaryStillExportsAMetric(t *testing.T) {
	stamp(t, "", "", "")

	reg := prometheus.NewRegistry()
	if err := Register(reg, "stream"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := testutil.CollectAndCount(reg, "sharpline_build_info"); got != 1 {
		t.Fatalf("sharpline_build_info has %d series, want exactly 1", got)
	}
	if got := testutil.ToFloat64(NewCollector("stream")); got != 1 {
		t.Errorf("sharpline_build_info value = %v, want the constant 1", got)
	}
}
