package storage

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// pgDSN returns a DSN for conformance testing: BOTJE_PG_TEST_DSN if set,
// otherwise a throwaway postgres container that lives for this test run.
func pgDSN(t *testing.T) string {
	t.Helper()
	if dsn := os.Getenv("BOTJE_PG_TEST_DSN"); dsn != "" {
		return dsn
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no BOTJE_PG_TEST_DSN and no docker; skipping postgres conformance")
	}
	out, err := exec.Command("docker", "run", "-d", "--rm",
		"-e", "POSTGRES_PASSWORD=botje-test",
		"-p", "127.0.0.1::5432", "postgres:17-alpine").Output()
	if err != nil {
		t.Skipf("docker run postgres failed: %v", err)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", id).Run() })

	port, err := exec.Command("docker", "port", id, "5432/tcp").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	addr := strings.TrimSpace(strings.Split(string(port), "\n")[0])
	dsn := fmt.Sprintf("postgres://postgres:botje-test@%s/postgres", addr)

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		err = exec.Command("docker", "exec", id, "pg_isready", "-U", "postgres").Run()
		if err == nil {
			return dsn
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("postgres container %s never became ready", id[:12])
	return ""
}

func TestPostgresConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	dsn := pgDSN(t)
	ctx := context.Background()

	pg, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { pg.Close() })

	conformance(t, func(t *testing.T) Store {
		if err := pg.wipe(ctx); err != nil {
			t.Fatal(err)
		}
		return pg
	})
}

func TestPostgresMigrateIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	dsn := pgDSN(t)
	ctx := context.Background()

	for i := range 2 {
		pg, err := OpenPostgres(ctx, dsn)
		if err != nil {
			t.Fatalf("OpenPostgres run %d: %v", i+1, err)
		}
		pg.Close()
	}
}
