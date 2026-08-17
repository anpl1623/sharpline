package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

// TestDockerSocketIsUsableFromInsideTheTestContainer proves the substrate the rest
// of this package stands on, and it is deliberately the cheapest test here so
// that a broken socket mount fails with THIS message rather than as a mysterious
// dial error inside a container start forty seconds later.
//
// Phase 0's Makefile resolves the socket path from `docker context inspect`
// instead of assuming /var/run/docker.sock, because on the author's machine
// Docker Desktop's "Allow the default Docker socket to be used" setting is off and
// the only socket is ~/.docker/run/docker.sock. That resolution is invisible from
// in here — all the test container sees is the mount target — so what this test
// checks is the RESULT: that DOCKER_HOST names a real socket at the path
// testcontainers will dial, that the daemon answers, and that the host override
// which makes a sibling's published port reachable is actually in force.
func TestDockerSocketIsUsableFromInsideTheTestContainer(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. The socket the Makefile mounted is a socket, not a directory.
	//
	// This matters more than it sounds: docker silently CREATES A DIRECTORY at a
	// missing bind source, so a wrong host path arrives in here as a directory at
	// the mount target and every subsequent failure points somewhere else.
	dockerHost := os.Getenv("DOCKER_HOST")
	if dockerHost == "" {
		t.Fatal("DOCKER_HOST is unset; `make test` sets it to unix:///var/run/docker.sock inside the container (see DOCKER_TESTCONTAINERS_FLAGS)")
	}
	socketPath, ok := strings.CutPrefix(dockerHost, "unix://")
	if !ok {
		t.Fatalf("DOCKER_HOST = %q is not a unix socket URL; testcontainers cannot spawn siblings over anything else in this setup", dockerHost)
	}
	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("stat %s: %v\nThe docker socket is not mounted into the test container. Run the suite with `make test`, which discovers the host socket path from `docker context inspect` and mounts it.", socketPath, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s exists but is a %s, not a socket. This is the failure mode where docker created a DIRECTORY at a missing bind source: the host path the Makefile resolved does not exist.", socketPath, info.Mode().Type())
	}

	// 2. The daemon on the other end answers.
	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		t.Fatalf("build docker provider: %v", err)
	}
	t.Cleanup(func() { _ = provider.Close() })

	if err := provider.Health(ctx); err != nil {
		t.Fatalf("docker daemon is not healthy through the mounted socket: %v", err)
	}

	// 3. The host override is in force.
	//
	// Sibling containers publish their ports on the HOST, not inside the test
	// container, so 127.0.0.1 here is the wrong machine. TESTCONTAINERS_HOST_OVERRIDE
	// plus `--add-host host.docker.internal:host-gateway` is what makes
	// Container.Host() return an address that resolves to the host from in here.
	const wantHost = "host.docker.internal"
	if got := os.Getenv("TESTCONTAINERS_HOST_OVERRIDE"); got != wantHost {
		t.Errorf("TESTCONTAINERS_HOST_OVERRIDE = %q, want %q; without it Container.Host() returns 127.0.0.1, which inside the test container is the test container", got, wantHost)
	}

	// 4. And the shared database, which was reached over exactly that path,
	//    is genuinely alive — the end-to-end proof that all of the above works.
	db := sharedDatabase(t)
	if !strings.Contains(db.dsn, wantHost) {
		t.Errorf("shared DSN %q does not go through %s; the host override did not reach Container.Host()", db.dsn, wantHost)
	}

	pool, _ := sharedPool(t)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping the shared database over the published port: %v", err)
	}
}
