//go:build integration

package runner

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	postgresstore "themisy/internal/store/postgres"
	"themisy/pkg/protocol"
)

// This cuts real TCP connections while leaving PostgreSQL and the pool alive.
// It never stops the shared database used by other integration packages.
func TestJournalNetworkDisconnectReconnect(t *testing.T) {
	if os.Getenv("THEMISY_INTEGRATION") != "1" {
		t.Fatal("THEMISY_INTEGRATION=1 required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := postgresstore.MigrateRunner(ctx, recoveryURL()); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(recoveryURL())
	if err != nil {
		t.Fatal(err)
	}
	host := u.Hostname()
	if host == "" {
		t.Fatal("network test requires a PostgreSQL TCP URL")
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	proxy := newJournalProxy(t, net.JoinHostPort(host, port))
	u.Host = proxy.listener.Addr().String()
	journal, err := postgresstore.NewRunnerJournal(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { journal.Close() }()
	f := newRunnerFixture(t)
	id := fmt.Sprintf("network-%d", time.Now().UnixNano())
	f.grant.GrantID, f.grant.Nonce, f.grant.IdempotencyKey = id, id, id
	f.resignWithPolicyHash(t)
	f.runner.Journal = journal
	assertUnavailable := func(want int) {
		t.Helper()
		attempt, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		result, err := f.runner.Execute(attempt, f.grant)
		if err == nil || result.ReasonCode != string(ReasonJournalUnavailable) {
			t.Fatalf("outage result=%#v err=%v", result, err)
		}
		if f.credentials.Calls() != want || f.adapter.Calls() != want {
			t.Fatalf("outage acquired credentials or wrote: credentials=%d writes=%d want=%d", f.credentials.Calls(), f.adapter.Calls(), want)
		}
	}
	proxy.setAvailable(false)
	assertUnavailable(0)
	proxy.setAvailable(true)
	result, err := f.runner.Execute(ctx, f.grant)
	if err != nil || result.Status != protocol.ResultSucceeded {
		t.Fatalf("reconnected execution=%#v err=%v", result, err)
	}
	proxy.setAvailable(false)
	assertUnavailable(1)
	proxy.setAvailable(true)
	// A fresh pool must recover the persisted success without acquiring another
	// credential or performing another provider write.
	journal.Close()
	journal, err = postgresstore.NewRunnerJournal(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	f.runner.Journal = journal
	saved, err := f.runner.Execute(ctx, f.grant)
	if err != nil || saved != result || f.credentials.Calls() != 1 || f.adapter.Calls() != 1 {
		t.Fatalf("replay=%#v err=%v credentials=%d writes=%d", saved, err, f.credentials.Calls(), f.adapter.Calls())
	}
	events, err := journal.AuditEvents(id)
	if err != nil || len(events) != 3 {
		t.Fatalf("durable journal audits=%#v err=%v", events, err)
	}
	for i, action := range []string{"runner.action.reserve", "runner.action.dispatch", "runner.action.complete"} {
		if events[i].Action != action {
			t.Fatalf("audit[%d]=%s want=%s", i, events[i].Action, action)
		}
	}
}

// This test stops the actual Compose PostgreSQL service. It is kept behind an
// explicit flag and runs after the ordinary integration suite, so it cannot
// interrupt unrelated package tests or a developer's arbitrary database.
func TestPostgresServiceStopFailsClosed(t *testing.T) {
	if os.Getenv("THEMISY_INTEGRATION") != "1" || os.Getenv("THEMISY_TEST_POSTGRES_SERVICE_STOP") != "1" {
		t.Skip("requires THEMISY_INTEGRATION=1 and THEMISY_TEST_POSTGRES_SERVICE_STOP=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	composeFile, err := filepath.Abs("../../deploy/compose/compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	runCompose := func(runCtx context.Context, args ...string) (string, error) {
		t.Helper()
		base := []string{"compose", "-f", composeFile}
		cmd := exec.CommandContext(runCtx, "docker", append(base, args...)...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return string(output), fmt.Errorf("docker %v: %w: %s", args, err, output)
		}
		return string(output), nil
	}
	compose := func(args ...string) error {
		_, err := runCompose(ctx, args...)
		return err
	}
	databaseURL := recoveryURL()
	dbURL, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	port := dbURL.Port()
	if port == "" {
		port = "5432"
	}
	password, hasPassword := dbURL.User.Password()
	if host := dbURL.Hostname(); (host != "localhost" && host != "127.0.0.1") || port != "5432" || strings.TrimPrefix(dbURL.Path, "/") != "themisy" || dbURL.User.Username() != "themisy" || !hasPassword || password != "themisy" || dbURL.RawQuery != "sslmode=disable" {
		t.Fatalf("service-stop test requires the exact Compose connection settings, got %s", dbURL.Redacted())
	}
	runningServices, err := runCompose(ctx, "ps", "--status", "running", "--services", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	wasRunning := strings.TrimSpace(runningServices) == "postgres"
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if wasRunning {
			if _, err := runCompose(cleanupCtx, "start", "postgres"); err != nil {
				t.Errorf("restore PostgreSQL running state: %v", err)
			} else if err := waitRunnerPostgres(cleanupCtx, databaseURL); err != nil {
				t.Errorf("wait for PostgreSQL during cleanup: %v", err)
			}
		} else {
			if _, err := runCompose(cleanupCtx, "stop", "-t", "1", "postgres"); err != nil {
				t.Errorf("restore PostgreSQL stopped state: %v", err)
			}
		}
	}()
	if !wasRunning {
		if err := compose("start", "postgres"); err != nil {
			t.Fatal(err)
		}
	}
	if err := waitRunnerPostgres(ctx, databaseURL); err != nil {
		t.Fatal(err)
	}
	if err := postgresstore.MigrateRunner(ctx, databaseURL); err != nil {
		t.Fatal(err)
	}
	journal, err := postgresstore.NewRunnerJournal(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	f := newRunnerFixture(t)
	id := fmt.Sprintf("service-stop-%d", time.Now().UnixNano())
	f.grant.GrantID, f.grant.Nonce, f.grant.IdempotencyKey = id, id, id
	f.resignWithPolicyHash(t)
	f.runner.Journal = journal
	failClosed := func(wantCredentials, wantWrites int) {
		t.Helper()
		attempt, stop := context.WithTimeout(ctx, 3*time.Second)
		defer stop()
		result, err := f.runner.Execute(attempt, f.grant)
		if err == nil || result.ReasonCode != string(ReasonJournalUnavailable) {
			t.Fatalf("stopped PostgreSQL result=%#v err=%v", result, err)
		}
		if f.credentials.Calls() != wantCredentials || f.adapter.Calls() != wantWrites {
			t.Fatalf("stopped PostgreSQL acquired credentials or wrote: credentials=%d writes=%d want=%d/%d", f.credentials.Calls(), f.adapter.Calls(), wantCredentials, wantWrites)
		}
	}
	if err := compose("stop", "-t", "1", "postgres"); err != nil {
		t.Fatal(err)
	}
	failClosed(0, 0)
	if err := compose("start", "postgres"); err != nil {
		t.Fatal(err)
	}
	if err := waitRunnerPostgres(ctx, databaseURL); err != nil {
		t.Fatal(err)
	}
	result, err := f.runner.Execute(ctx, f.grant)
	if err != nil || result.Status != protocol.ResultSucceeded {
		t.Fatalf("execution after PostgreSQL restart=%#v err=%v", result, err)
	}
	if f.credentials.Calls() != 1 || f.adapter.Calls() != 1 {
		t.Fatalf("recovered execution credentials=%d writes=%d want=1/1", f.credentials.Calls(), f.adapter.Calls())
	}
	if err := compose("stop", "-t", "1", "postgres"); err != nil {
		t.Fatal(err)
	}
	failClosed(1, 1)
	if err := compose("start", "postgres"); err != nil {
		t.Fatal(err)
	}
	if err := waitRunnerPostgres(ctx, databaseURL); err != nil {
		t.Fatal(err)
	}
}

func waitRunnerPostgres(ctx context.Context, databaseURL string) error {
	var lastErr error
	for {
		journal, err := postgresstore.NewRunnerJournal(ctx, databaseURL)
		if err == nil {
			journal.Close()
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("PostgreSQL did not become ready: %w (last error: %v)", ctx.Err(), lastErr)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

type journalProxy struct {
	listener    net.Listener
	target      string
	mu          sync.Mutex
	available   bool
	connections map[net.Conn]struct{}
	workers     sync.WaitGroup
}

func newJournalProxy(t *testing.T, target string) *journalProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &journalProxy{listener: listener, target: target, available: true, connections: make(map[net.Conn]struct{})}
	p.workers.Add(1)
	go func() {
		defer p.workers.Done()
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			if !p.available {
				p.mu.Unlock()
				client.Close()
				continue
			}
			p.connections[client] = struct{}{}
			p.workers.Add(1)
			p.mu.Unlock()
			go p.forward(client)
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		p.setAvailable(false)
		p.workers.Wait()
	})
	return p
}

func (p *journalProxy) setAvailable(available bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.available = available
	if !available {
		for conn := range p.connections {
			conn.Close()
		}
	}
}

func (p *journalProxy) forward(client net.Conn) {
	defer p.workers.Done()
	defer client.Close()
	defer func() { p.mu.Lock(); delete(p.connections, client); p.mu.Unlock() }()
	upstream, err := net.DialTimeout("tcp", p.target, time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	p.mu.Lock()
	if !p.available {
		p.mu.Unlock()
		return
	}
	p.connections[upstream] = struct{}{}
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.connections, upstream); p.mu.Unlock() }()
	done := make(chan struct{})
	go func() { _, _ = io.Copy(upstream, client); upstream.Close(); close(done) }()
	_, _ = io.Copy(client, upstream)
	client.Close()
	<-done
}
