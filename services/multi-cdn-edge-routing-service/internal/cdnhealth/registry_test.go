package cdnhealth

import (
	"sync"
	"testing"
	"time"

	"multicdnrouting.gormalone.io/multi-cdn-edge-routing-service/internal/domain"
)

func TestLatestReportForProvider_UnknownProviderReturnsNotOk(t *testing.T) {
	registry := NewCdnHealthRegistry()
	_, ok := registry.LatestReportForProvider(domain.CdnProviderFastly)
	if ok {
		t.Fatalf("expected ok=false for a provider that never reported")
	}
}

func TestIngestHealthReport_ReplacesPreviousReportForSameProvider(t *testing.T) {
	registry := NewCdnHealthRegistry()
	older := domain.CdnHealthReport{
		Provider:                domain.CdnProviderAkamai,
		EdgeLatencyMilliseconds: 120,
		ErrorRatePercent:        1.0,
		CostPerGigabyteUSD:      0.02,
		ReportedAtUTC:           time.Unix(1000, 0),
	}
	newer := domain.CdnHealthReport{
		Provider:                domain.CdnProviderAkamai,
		EdgeLatencyMilliseconds: 80,
		ErrorRatePercent:        0.5,
		CostPerGigabyteUSD:      0.02,
		ReportedAtUTC:           time.Unix(2000, 0),
	}

	registry.IngestHealthReport(older)
	registry.IngestHealthReport(newer)

	got, ok := registry.LatestReportForProvider(domain.CdnProviderAkamai)
	if !ok {
		t.Fatalf("expected a report to be present")
	}
	if got.EdgeLatencyMilliseconds != 80 {
		t.Fatalf("expected the most recently ingested report to win, got latency=%v", got.EdgeLatencyMilliseconds)
	}
}

func TestIngestHealthReportFeed_IngestsAllReportsInOrder(t *testing.T) {
	registry := NewCdnHealthRegistry()
	feed := []domain.CdnHealthReport{
		{Provider: domain.CdnProviderCloudFront, EdgeLatencyMilliseconds: 100},
		{Provider: domain.CdnProviderAkamai, EdgeLatencyMilliseconds: 110},
		{Provider: domain.CdnProviderFastly, EdgeLatencyMilliseconds: 90},
		{Provider: domain.CdnProviderCloudFront, EdgeLatencyMilliseconds: 95}, // supersedes the first CloudFront entry
	}
	registry.IngestHealthReportFeed(feed)

	if registry.KnownProviderCount() != 3 {
		t.Fatalf("expected 3 distinct providers, got %d", registry.KnownProviderCount())
	}

	cloudFrontReport, ok := registry.LatestReportForProvider(domain.CdnProviderCloudFront)
	if !ok || cloudFrontReport.EdgeLatencyMilliseconds != 95 {
		t.Fatalf("expected CloudFront's latest report to have latency 95, got %+v (ok=%v)", cloudFrontReport, ok)
	}
}

func TestLatestReportsForAllProviders_IsSortedByProviderIdentifier(t *testing.T) {
	registry := NewCdnHealthRegistry()
	registry.IngestHealthReportFeed([]domain.CdnHealthReport{
		{Provider: domain.CdnProviderFastly},
		{Provider: domain.CdnProviderCloudFront},
		{Provider: domain.CdnProviderAkamai},
	})

	reports := registry.LatestReportsForAllProviders()
	if len(reports) != 3 {
		t.Fatalf("expected 3 reports, got %d", len(reports))
	}
	for i := 1; i < len(reports); i++ {
		if reports[i-1].Provider >= reports[i].Provider {
			t.Fatalf("expected ascending provider order, got %v then %v", reports[i-1].Provider, reports[i].Provider)
		}
	}
}

func TestCdnHealthRegistry_ConcurrentIngestAndReadIsRaceFree(t *testing.T) {
	registry := NewCdnHealthRegistry()
	var waitGroup sync.WaitGroup

	for i := 0; i < 50; i++ {
		waitGroup.Add(2)
		go func(i int) {
			defer waitGroup.Done()
			registry.IngestHealthReport(domain.CdnHealthReport{
				Provider:                domain.CdnProviderCloudFront,
				EdgeLatencyMilliseconds: float64(i),
				ReportedAtUTC:           time.Now(),
			})
		}(i)
		go func() {
			defer waitGroup.Done()
			registry.LatestReportsForAllProviders()
		}()
	}
	waitGroup.Wait()
}
