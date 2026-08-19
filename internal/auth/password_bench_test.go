package auth

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// The argon2id cost measurement.
//
// This phase's brief: "Choose m, t and p deliberately against the REAL deploy
// target: 2 OCPU / 12 GB Ampere ARM. Show the reasoning and MEASURE the actual
// cost per hash on that budget — a parameter set that takes 2 seconds on 2
// cores is a denial-of-service on your own login endpoint. Record the
// measurement."
//
// The reasoning is in DefaultParams' doc comment. This file is the measurement,
// and it is a benchmark rather than a number in a comment so that it can be
// re-run on the real hardware instead of trusted.
//
// # RECORDED MEASUREMENT
//
// Run inside the project's Go container (`make test`'s image, golang:1.26-alpine),
// on the author's Apple Silicon Mac, with the container's CPU allocation pinned
// to two cores to stand in for the 2-OCPU Ampere A1 — `docker run --cpus=2`, and
// GOMAXPROCS=2 so the argon2 lanes see the same limit the scheduler does.
//
//	goos: linux  goarch: arm64  pkg: github.com/anpl1623/sharpline/internal/auth
//	BenchmarkHashCostOnDeployTarget-2     20    53682365 ns/op   53.00 ms/hash    67113935 B/op   59 allocs/op
//	BenchmarkVerifyCostOnDeployTarget-2   20    54825175 ns/op   54.00 ms/verify  67114194 B/op   58 allocs/op
//
//	TestDefaultParamsAreNotADenialOfServiceOnOurOwnLoginEndpoint:
//	  MEASUREMENT argon2id m=65536,t=3,p=2 at GOMAXPROCS=2 (host has 18 CPUs): 64.377847ms per hash
//
// So: ~54 ms per hash and per verification, and 67 MB allocated per hash —
// which is the 64 MiB block plus change, exactly as m=65536 promises. The
// allocation figure is the useful cross-check on the operational ceiling in
// DefaultParams' comment: two concurrent hashes are ~134 MB, about 1% of the
// 12 GB target.
//
// The figure to look at is ns/op divided by 1e6, i.e. milliseconds per hash.
// The interpretation:
//
//	< 100ms   comfortable. Two cores sustain >10 logins/s with the limiter at
//	          GOMAXPROCS, which is far above anything this system will see and
//	          well below the point where a login feels slow.
//	100-250ms the intended operating point for m=64MiB, t=3, p=2.
//	> 500ms   too expensive. An attacker with a modest request rate saturates
//	          both cores and the box stops serving odds, which is the actual
//	          product. Lower t before lowering m — m is what costs the attacker.
//	> 2s      the failure mode the brief names. Do not ship.
//
// # The Mac is FASTER than the target, and that matters
//
// An M-series performance core is roughly 1.5-2x an Ampere Neoverse N1 at the
// same clock on this workload, which is memory-bandwidth-bound. So a figure
// measured here is a LOWER BOUND on the Ampere cost: read it as "at least this
// slow in production, probably up to twice". The bands above already allow for
// that — the 100-250ms band on this machine lands inside the 500ms ceiling on
// the target.
//
// # Why the benchmark constrains GOMAXPROCS rather than trusting the host
//
// argon2's p lanes are goroutines. On a 10-core laptop, p=2 runs both lanes on
// separate cores with no contention; on the 2-core target the same p=2 competes
// with Postgres, Kafka and the Go runtime itself. Measuring at GOMAXPROCS=2 is
// the closest honest approximation available without the hardware, and the
// container CPU cap makes the approximation real rather than advisory.

// BenchmarkHashCostOnDeployTarget measures one argon2id hash under
// [DefaultParams] with the deploy target's core count.
//
// Run it in the project's Go container with the CPU allocation pinned:
//
//	docker run --rm --user $(id -u):$(id -g) --cpus=2 -e GOMAXPROCS=2 \
//	  -v "$PWD":/src -w /src \
//	  -v sharpline-go-mod-cache:/go/pkg -v sharpline-go-build-cache:/gocache \
//	  -e HOME=/tmp -e GOPATH=/go -e GOMODCACHE=/go/pkg/mod -e GOCACHE=/gocache \
//	  -e CGO_ENABLED=0 golang:1.26-alpine \
//	  go test -run '^$' -bench BenchmarkHashCostOnDeployTarget -benchtime 20x ./internal/auth
//
// CLAUDE.md §12 asks that every developer action be a make target, and this is
// not one yet: the Makefile has no `bench` target at all. Adding one — `bench:`
// wrapping the invocation above with a CPUS variable — is named in this phase's
// handoff as the one Makefile change the auth work needs.
func BenchmarkHashCostOnDeployTarget(b *testing.B) {
	const targetCores = 2

	prev := runtime.GOMAXPROCS(targetCores)
	b.Cleanup(func() { runtime.GOMAXPROCS(prev) })

	h, err := NewHasher(HasherOptions{Params: DefaultParams, Concurrency: targetCores})
	if err != nil {
		b.Fatalf("NewHasher: %v", err)
	}

	ctx := context.Background()
	password := NewSecret("correct horse battery staple")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := h.Hash(ctx, password); err != nil {
			b.Fatalf("Hash: %v", err)
		}
	}
	b.StopTimer()

	perOp := b.Elapsed() / time.Duration(max(b.N, 1))
	b.ReportMetric(float64(perOp.Milliseconds()), "ms/hash")
	b.Logf("argon2id %s at GOMAXPROCS=%d: %s per hash (host reports %d CPUs)",
		DefaultParams, targetCores, perOp, runtime.NumCPU())
}

// BenchmarkVerifyCostOnDeployTarget measures the verification half, which is
// what the login endpoint actually pays on every request — including the ones
// with a wrong password, and including the decoy path for an unknown email.
func BenchmarkVerifyCostOnDeployTarget(b *testing.B) {
	const targetCores = 2

	prev := runtime.GOMAXPROCS(targetCores)
	b.Cleanup(func() { runtime.GOMAXPROCS(prev) })

	h, err := NewHasher(HasherOptions{Params: DefaultParams, Concurrency: targetCores})
	if err != nil {
		b.Fatalf("NewHasher: %v", err)
	}

	ctx := context.Background()
	password := NewSecret("correct horse battery staple")
	encoded, err := h.Hash(ctx, password)
	if err != nil {
		b.Fatalf("Hash: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := h.Verify(ctx, encoded, password); err != nil {
			b.Fatalf("Verify: %v", err)
		}
	}
	b.StopTimer()

	perOp := b.Elapsed() / time.Duration(max(b.N, 1))
	b.ReportMetric(float64(perOp.Milliseconds()), "ms/verify")
	b.Logf("argon2id verify %s at GOMAXPROCS=%d: %s per verification",
		DefaultParams, targetCores, perOp)
}

// TestDefaultParamsAreNotADenialOfServiceOnOurOwnLoginEndpoint is the guard the
// benchmark cannot be: a benchmark reports, a test fails.
//
// The ceiling is DELIBERATELY generous — one second per hash at the target's
// core count — because this is not a performance regression test and must not
// go red on a loaded CI runner. It is the specific failure the brief names: "a
// parameter set that takes 2 seconds on 2 cores is a denial-of-service on your
// own login endpoint". Anything under a second on this machine is comfortably
// under two on the Ampere; anything over it means DefaultParams was raised
// without re-reading the deploy target, and the build should say so.
func TestDefaultParamsAreNotADenialOfServiceOnOurOwnLoginEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("runs argon2id at full production cost")
	}

	const (
		targetCores = 2
		ceiling     = time.Second
		samples     = 3
	)

	prev := runtime.GOMAXPROCS(targetCores)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })

	h, err := NewHasher(HasherOptions{Params: DefaultParams, Concurrency: targetCores})
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}

	ctx := context.Background()
	password := NewSecret("correct horse battery staple")

	start := time.Now()
	for i := 0; i < samples; i++ {
		if _, err := h.Hash(ctx, password); err != nil {
			t.Fatalf("Hash: %v", err)
		}
	}
	perHash := time.Since(start) / samples

	// Logged unconditionally: this line IS the recorded measurement, and it
	// appears in `make test` output on whatever machine ran it.
	t.Logf("MEASUREMENT argon2id %s at GOMAXPROCS=%d (host has %d CPUs): %s per hash",
		DefaultParams, targetCores, runtime.NumCPU(), perHash)

	if perHash > ceiling {
		t.Fatalf("one argon2id hash under DefaultParams costs %s at %d cores, over the %s ceiling. "+
			"On the 2-OCPU Ampere deploy target this is a denial of service on the login endpoint. "+
			"Lower t before lowering m: m is the parameter that costs the attacker.",
			perHash, targetCores, ceiling)
	}
}
