package contextsnapshot

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"themisy/adapters/datadog"
	"themisy/adapters/githubactions"
	"themisy/internal/contract"
	"themisy/internal/domain"
	"themisy/internal/planner"
	"themisy/internal/profile"
	"themisy/pkg/adapter"
	"time"
)

// Collect all parts through their real typed adapters/brokers, using bounded
// synthetic HTTP/token sources. Only the assembler clock is frozen afterward.
func sourceContextParts(t *testing.T, leaf bool) (CISnapshot, DependencySnapshot, MetadataSnapshot) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	_, _, http, _, _, _ := ciCollectorFixture(t)
	http.runBody = strings.ReplaceAll(http.runBody, "2026-10-19T23:59:00Z", now.Add(-time.Minute).Format(time.RFC3339))
	ciConfig := githubactions.CIConfig{Targets: map[githubactions.CITargetKey]githubactions.CITarget{{TenantID: "tenant-a", Service: "payment-api", Environment: "staging"}: {TenantID: "tenant-a", Owner: "example", Repository: "themisy-demo", Workflow: "ci.yml", MaximumAge: time.Hour}}}
	ciCollector, err := NewCICollector(ciConfig, &ciTokenSource{expiry: now.Add(time.Hour)}, http)
	if err != nil {
		t.Fatal(err)
	}
	ci, err := ciCollector.Collect(ctx, githubactions.CIRequest{TenantID: "tenant-a", Target: adapter.Target{Service: "payment-api", Environment: "staging"}, CommitSHA: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	metadataCollector, token, metadataHTTP, _, metadataConfig, key, _ := metadataFixture(t)
	target := metadataConfig.Targets[key]
	target.Revision = strings.Repeat("b", 40)
	metadataConfig.Targets[key] = target
	metadataCollector, err = NewMetadataCollector(metadataConfig, token, metadataHTTP, &ciAuditSink{})
	if err != nil {
		t.Fatal(err)
	}
	metadataCollector.now = time.Now
	token.expiry = now.Add(time.Hour)
	if leaf {
		metadataHTTP.files["contracts/payment-api.yaml"] = regexp.MustCompile(`(?s)\ndependencies:.*?\nverification:`).ReplaceAllString(metadataHTTP.files["contracts/payment-api.yaml"], "\nverification:")
	}
	metadata, err := metadataCollector.Collect(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	var dependencies DependencySnapshot
	if !leaf {
		_, _, _, _, healthConfig, _ := dependencyFixture(t)
		var target datadog.HealthTarget
		for _, configured := range healthConfig.Health.Targets {
			target = configured
			break
		}
		config := DependencyConfig{Sets: map[DependencyKey][]string{key: {"identity-api"}}, Health: datadog.HealthConfig{Targets: map[datadog.HealthTargetKey]datadog.HealthTarget{{TenantID: key.TenantID, Service: "identity-api", Environment: key.Environment}: target}}}
		collector, err := NewDependencyCollector(config, &dependencySource{}, &dependencyHTTP{}, &ciAuditSink{})
		if err != nil {
			t.Fatal(err)
		}
		dependencies, err = collector.Collect(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
	}
	return ci, dependencies, metadata
}
func TestSourceContextBindsScopeCompleteDependenciesAndEarliestExpiry(t *testing.T) {
	for _, leaf := range []bool{false, true} {
		t.Run(map[bool]string{false: "dependent", true: "leaf"}[leaf], func(t *testing.T) {
			ci, dependencies, metadata := sourceContextParts(t, leaf)
			sink := &ciAuditSink{}
			assembler, _ := NewSourceContextAssembler(sink)
			now := time.Now().UTC()
			assembler.now = func() time.Time { return now }
			snapshot, err := assembler.Assemble(context.Background(), ci, dependencies, metadata)
			if err != nil || snapshot.RequireFreshSources(now) != nil {
				t.Fatal("assembly", err)
			}
			record := snapshot.Record()
			if record.Metadata.Contract.Revision == record.CommitSHA {
				t.Fatal("fixture must exercise distinct metadata/application revisions")
			}
			hash, _ := sourceContextHash(record)
			if record.Mode != "SOURCE_OBSERVATIONS_ONLY" || record.CommitSHA != ci.Record().CommitSHA || hash != snapshot.Hash() || len(sink.events) != 1 || sink.events[0].ResourceID != hash || sink.events[0].Result != "OBSERVED" {
				t.Fatal("context/audit binding")
			}
			if leaf {
				if record.Dependencies != nil || len(record.DependencyServices) != 0 {
					t.Fatal("leaf invented dependencies")
				}
			} else {
				if len(record.DependencyServices) != 1 || record.DependencyServices[0] != "identity-api" || record.Dependencies == nil {
					t.Fatal("dependency set lost")
				}
				until := dependencies.Record().Evidence[0].SourceUpdatedAt.Add(dependencies.Record().Evidence[0].MaximumAge)
				if !record.ExpiresAt.Equal(until) {
					t.Fatal("earliest source expiry not retained")
				}
				record.Dependencies.Evidence[0].Status = adapter.VerificationFail
				record.DependencyServices[0] = "caller-mutated"
			}
			record.CI.Evidence.Artifacts[0].Name = "changed"
			if snapshot.RequireFreshSources(now) != nil || snapshot.Record().CI.Evidence.Artifacts[0].Name == "changed" {
				t.Fatal("caller aliases private snapshot")
			}
			stored := sink.events[0].Details["record"].(SourceContextRecord)
			stored.CI.Evidence.Artifacts[0].Name = "sink-mutated"
			if snapshot.Record().CI.Evidence.Artifacts[0].Name == "sink-mutated" {
				t.Fatal("sink aliases private snapshot")
			}
			another, err := assembler.Assemble(context.Background(), ci, dependencies, metadata)
			if err != nil || another.Hash() != snapshot.Hash() {
				t.Fatal("same observations/time not deterministic")
			}
			raw, _ := json.Marshal(snapshot.Record())
			var imported SourceContextSnapshot
			if json.Unmarshal(raw, &imported) == nil || imported.RequireFreshSources(now) == nil {
				t.Fatal("JSON minted context trust")
			}
			if snapshot.RequireFreshSources(snapshot.Record().ExpiresAt) == nil {
				t.Fatal("expiry equality accepted")
			}
		})
	}
}
func TestSourceContextRefusesMixedOrIncompleteSourcesBeforeAudit(t *testing.T) {
	for _, mode := range []string{"tenant", "environment", "repository", "commit", "missing-dependency", "other-dependency", "nonpass", "stale"} {
		t.Run(mode, func(t *testing.T) {
			ci, dependencies, metadata := sourceContextParts(t, false)
			now := time.Now().UTC()
			switch mode {
			case "tenant":
				ci.record.TenantID = "other"
				ci.record.Hash, _ = hashRecord(ci.record)
			case "environment":
				ci.record.Target.Environment = "production"
				ci.record.Hash, _ = hashRecord(ci.record)
			case "repository":
				ci.record.Evidence.Repository = "other/repo"
				ci.record.Hash, _ = hashRecord(ci.record)
			case "commit":
				ci.record.CommitSHA = strings.Repeat("b", 40)
				ci.record.Hash, _ = hashRecord(ci.record)
			case "missing-dependency":
				dependencies = DependencySnapshot{}
			case "other-dependency":
				dependencies.record.Evidence[0].Target.Service = "other"
				dependencies.record.Hash, _ = dependencyHash(dependencies.record)
			case "nonpass":
				dependencies.record.Evidence[0].Status = adapter.VerificationInconclusive
				dependencies.record.Hash, _ = dependencyHash(dependencies.record)
			case "stale":
				now = now.Add(2 * time.Hour)
			}
			sink := &ciAuditSink{}
			assembler, _ := NewSourceContextAssembler(sink)
			assembler.now = func() time.Time { return now }
			if snapshot, err := assembler.Assemble(context.Background(), ci, dependencies, metadata); err == nil || snapshot.Hash() != "" || len(sink.events) != 0 {
				t.Fatal("mixed/incomplete observations accepted")
			}
		})
	}
}
func TestSourceContextDiscardsOnFailedOrAmbiguousAudit(t *testing.T) {
	for _, mode := range []string{"failed", "cancel", "expiry", "clock-reversal"} {
		t.Run(mode, func(t *testing.T) {
			ci, dependencies, metadata := sourceContextParts(t, false)
			sink := &ciAuditSink{}
			assembler, _ := NewSourceContextAssembler(sink)
			now := time.Now().UTC()
			assembler.now = func() time.Time { return now }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "failed":
				sink.err = errors.New("response lost secret-canary")
			case "cancel":
				sink.hook = func(context.Context, domain.AuditEvent) { cancel() }
			case "expiry":
				sink.hook = func(context.Context, domain.AuditEvent) {
					assembler.now = func() time.Time { return now.Add(2 * time.Hour) }
				}
			case "clock-reversal":
				sink.hook = func(context.Context, domain.AuditEvent) {
					assembler.now = func() time.Time { return now.Add(-time.Second) }
				}
			}
			if snapshot, err := assembler.Assemble(ctx, ci, dependencies, metadata); err == nil || snapshot.Hash() != "" || len(sink.events) != 1 {
				t.Fatal("failed append returned context or retried")
			}
		})
	}
}

func TestSourceContextChangesCanonicalPlannerBinding(t *testing.T) {
	ci, dependencies, metadata := sourceContextParts(t, true)
	sink := &ciAuditSink{}
	assembler, _ := NewSourceContextAssembler(sink)
	now := time.Now().UTC()
	assembler.now = func() time.Time { return now }
	first, err := assembler.Assemble(context.Background(), ci, dependencies, metadata)
	if err != nil {
		t.Fatal(err)
	}
	changedMetadata := metadata
	changedMetadata.record.Contract.Revision = strings.Repeat("c", 40)
	changedMetadata.record.Profile.Revision = strings.Repeat("c", 40)
	changedMetadata.record.Hash, _ = metadataHash(changedMetadata.record)
	second, err := assembler.Assemble(context.Background(), ci, dependencies, changedMetadata)
	if err != nil || second.Hash() == first.Hash() {
		t.Fatal("new source revision did not change context", err)
	}
	serviceContract, _ := metadata.Contract()
	releaseProfile, _ := metadata.Profile()
	intent := domain.ReleaseIntent{APIVersion: domain.ReleaseIntentAPIVersion, Kind: domain.ReleaseIntentKind, RequestID: "source-plan", TenantID: "tenant-a", ReleaseVersion: "v1", Environment: domain.EnvironmentStaging, RequestedBy: "synthetic-user", Agent: domain.AgentIdentity{ID: "synthetic-agent", Scopes: []string{"release:deploy"}}, Changes: []domain.Change{{Service: "payment-api", DesiredVersion: "build-1", CISuccess: true, DependenciesHealthy: true}}}
	plan := func(snapshot SourceContextSnapshot) domain.ReleasePlan {
		p, err := planner.NewFromContracts([]contract.ServiceContract{serviceContract}, []profile.ReleaseProfile{releaseProfile}, planner.Options{ContextSnapshotRef: snapshot.Hash(), Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		result, err := p.PlanIntent(intent)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	one, repeat, two := plan(first), plan(first), plan(second)
	if one.PlanHash != repeat.PlanHash || one.PlanHash == two.PlanHash || one.ContextSnapshotRef != first.Hash() {
		t.Fatal("canonical Plan context binding lost")
	}
}
