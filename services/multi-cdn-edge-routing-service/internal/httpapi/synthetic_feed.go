package httpapi

import (
	"time"

	"multicdnrouting.gormalone.io/multi-cdn-edge-routing-service/internal/domain"
)

// BuildSyntheticCdnHealthFeed returns a small, fixed set of plausible
// domain.CdnHealthReport samples for every mock CDN provider, standing in
// for what a real periodic health-check/synthetic-monitoring pipeline would
// push into the cdnhealth.CdnHealthRegistry. cmd/server ingests this at
// startup (and periodically re-ingests slightly jittered values — see
// StartPeriodicSyntheticHealthFeed) so the demo server always has
// reasonably realistic health data to route against.
func BuildSyntheticCdnHealthFeed(now time.Time) []domain.CdnHealthReport {
	return []domain.CdnHealthReport{
		{
			Provider:                domain.CdnProviderCloudFront,
			EdgeLatencyMilliseconds: 95,
			ErrorRatePercent:        0.4,
			CostPerGigabyteUSD:      0.085,
			ReportedAtUTC:           now,
		},
		{
			Provider:                domain.CdnProviderAkamai,
			EdgeLatencyMilliseconds: 110,
			ErrorRatePercent:        0.6,
			CostPerGigabyteUSD:      0.070,
			ReportedAtUTC:           now,
		},
		{
			Provider:                domain.CdnProviderFastly,
			EdgeLatencyMilliseconds: 80,
			ErrorRatePercent:        1.2,
			CostPerGigabyteUSD:      0.060,
			ReportedAtUTC:           now,
		},
	}
}

// StartPeriodicSyntheticHealthFeed starts a background goroutine that
// re-ingests a freshly jittered BuildSyntheticCdnHealthFeed into
// server.HealthRegistry every interval, simulating a real CDN
// synthetic-monitoring pipeline's periodic push of updated rolling metrics.
// It returns a stop function; callers (cmd/server) should defer it (or tie
// it to context cancellation) to stop the goroutine on shutdown.
func StartPeriodicSyntheticHealthFeed(server *Server, interval time.Duration) (stop func()) {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case tick := <-ticker.C:
				server.HealthRegistry.IngestHealthReportFeed(jitterFeed(BuildSyntheticCdnHealthFeed(tick), tick.UnixNano()))
			}
		}
	}()
	return func() { close(done) }
}

// jitterFeed applies small deterministic-per-tick jitter to a health feed
// so the periodic re-ingestion isn't perfectly static, without depending on
// math/rand's global state.
func jitterFeed(reports []domain.CdnHealthReport, seed int64) []domain.CdnHealthReport {
	jittered := make([]domain.CdnHealthReport, len(reports))
	for i, r := range reports {
		swing := float64((seed>>uint(i))%7) - 3 // -3..+3
		r.EdgeLatencyMilliseconds += swing
		if r.EdgeLatencyMilliseconds < 1 {
			r.EdgeLatencyMilliseconds = 1
		}
		jittered[i] = r
	}
	return jittered
}
