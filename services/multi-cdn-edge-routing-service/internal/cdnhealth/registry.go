// Package cdnhealth implements the Registry (a specialization of the
// Repository pattern for in-memory, keyed collections) that ingests
// periodic CDN health reports and answers "what is the latest known health
// of provider X" / "what is the latest known health of every provider we
// track". It has no opinion about scoring or routing — that logic lives in
// internal/routing, which depends on this package, never the reverse.
package cdnhealth

import (
	"sort"
	"sync"

	"multicdnrouting.gormalone.io/multi-cdn-edge-routing-service/internal/domain"
)

// CdnHealthRegistry is an in-memory Registry of the most recently ingested
// domain.CdnHealthReport for each domain.CdnProviderIdentifier it has heard
// from. It is the single point of truth the routing decision engine
// consults when it needs "current" CDN health.
//
// Design pattern: Registry / Repository. CdnHealthRegistry decouples
// "how health data arrives" (periodic push from a monitoring feed, a
// synthetic test feed, or — in a real deployment — a Kafka consumer or
// polling agent) from "how health data is consumed" (the routing strategy
// in internal/routing only ever calls LatestReportForProvider /
// LatestReportsForAllProviders and never needs to know where reports came
// from). Only the single latest report per provider is retained; ingesting
// a new report for a provider replaces its previous entry, which mirrors
// how a real health-check system's "current status" table behaves (rolling
// averages are computed upstream, by whatever produces the report, not by
// this registry).
//
// CdnHealthRegistry is safe for concurrent use by multiple goroutines: a
// production deployment would have one goroutine ingesting reports from a
// monitoring feed while many request-handling goroutines read from it
// concurrently, so all methods take an internal mutex.
type CdnHealthRegistry struct {
	mutex                  sync.RWMutex
	latestReportByProvider map[domain.CdnProviderIdentifier]domain.CdnHealthReport
}

// NewCdnHealthRegistry constructs an empty CdnHealthRegistry ready to
// receive health reports via IngestHealthReport.
func NewCdnHealthRegistry() *CdnHealthRegistry {
	return &CdnHealthRegistry{
		latestReportByProvider: make(map[domain.CdnProviderIdentifier]domain.CdnHealthReport),
	}
}

// IngestHealthReport records report as the latest known health sample for
// its Provider, replacing any previously ingested report for that same
// provider. Reports are not required to arrive in timestamp order — this
// registry always stores whatever was ingested most recently by call order,
// matching how a real push-based monitoring pipeline behaves (a registry
// consuming from a message queue processes messages in arrival order, which
// may occasionally be out of strict timestamp order under network jitter).
func (registry *CdnHealthRegistry) IngestHealthReport(report domain.CdnHealthReport) {
	registry.mutex.Lock()
	defer registry.mutex.Unlock()
	registry.latestReportByProvider[report.Provider] = report
}

// IngestHealthReportFeed ingests every report in reports, in order. This is
// the entry point the test suite (and the demo HTTP server's startup
// seeding) uses to simulate "a periodic health-check feed reported these N
// samples over time" without standing up a real scheduler.
func (registry *CdnHealthRegistry) IngestHealthReportFeed(reports []domain.CdnHealthReport) {
	for _, report := range reports {
		registry.IngestHealthReport(report)
	}
}

// LatestReportForProvider returns the most recently ingested health report
// for the given provider, and whether one exists at all (a provider this
// registry has never heard from returns ok == false).
func (registry *CdnHealthRegistry) LatestReportForProvider(provider domain.CdnProviderIdentifier) (report domain.CdnHealthReport, ok bool) {
	registry.mutex.RLock()
	defer registry.mutex.RUnlock()
	report, ok = registry.latestReportByProvider[provider]
	return report, ok
}

// LatestReportsForAllProviders returns the most recently ingested health
// report for every provider this registry has heard from, sorted by
// Provider identifier for deterministic ordering (important for tests and
// for stable API response ordering).
func (registry *CdnHealthRegistry) LatestReportsForAllProviders() []domain.CdnHealthReport {
	registry.mutex.RLock()
	defer registry.mutex.RUnlock()

	reports := make([]domain.CdnHealthReport, 0, len(registry.latestReportByProvider))
	for _, report := range registry.latestReportByProvider {
		reports = append(reports, report)
	}
	sort.Slice(reports, func(i, j int) bool {
		return reports[i].Provider < reports[j].Provider
	})
	return reports
}

// KnownProviderCount returns how many distinct providers this registry has
// ever ingested a report for.
func (registry *CdnHealthRegistry) KnownProviderCount() int {
	registry.mutex.RLock()
	defer registry.mutex.RUnlock()
	return len(registry.latestReportByProvider)
}
