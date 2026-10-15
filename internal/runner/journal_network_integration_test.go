//go:build integration

package runner

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
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
