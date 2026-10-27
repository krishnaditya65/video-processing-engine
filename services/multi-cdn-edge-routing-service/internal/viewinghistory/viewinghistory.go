// Package viewinghistory synthesizes small, deterministic viewing-history
// datasets standing in for what a real recommendation/analytics data
// warehouse would supply: a per-user sequence of domain.ViewingSession
// records for a series they are binging. It exists purely to give
// internal/prediction and internal/httpapi realistic, reproducible input
// data without depending on a real datastore.
package viewinghistory

import (
	"fmt"
	"hash/fnv"
	"time"

	"multicdnrouting.gormalone.io/multi-cdn-edge-routing-service/internal/domain"
)

// DefaultSeriesIdentifier is the series used by the synthetic dataset
// generator below when the caller doesn't have a specific one in mind (the
// demo HTTP server uses this for every user).
const DefaultSeriesIdentifier = "the-edge-cases-s1"

// DefaultTargetEdgePointOfPresence is the edge PoP the synthetic dataset's
// users are modeled as being served from, used by internal/prediction when
// building CachePreWarmCommand.TargetEdgePointOfPresence for the demo
// server.
const DefaultTargetEdgePointOfPresence = "pop-us-east-1"

// nightlyBaseHour and nightlyBaseMinute define the "usual" nightly viewing
// time-of-day the synthetic dataset clusters around, before per-user,
// per-night jitter is applied.
const (
	nightlyBaseHour   = 20
	nightlyBaseMinute = 0
)

// BuildSyntheticNightlyViewingHistoryForUser generates a deterministic,
// reproducible sequence of numberOfNights domain.ViewingSession records for
// userIdentifier watching consecutive episodes of DefaultSeriesIdentifier,
// one session per calendar night, clustered around 20:00 local time with a
// small amount of per-night jitter (±0-6 minutes) — modeling a real "binges
// an episode most nights around the same time" viewer.
//
// Determinism: userIdentifier is hashed (FNV-1a) to seed a small
// linear-congruential jitter sequence, so the same user identifier always
// produces the same synthetic history (important so the demo API's
// predictions are stable across repeated requests / tests), while
// different users produce visibly different jitter patterns.
//
// endingAt is the timestamp of the most recent (last) synthesized session;
// earlier sessions are placed on the numberOfNights-1 calendar days before
// it. Callers building "as of now" demo data should pass time.Now(); tests
// pass a fixed anchor time for reproducibility.
func BuildSyntheticNightlyViewingHistoryForUser(userIdentifier string, numberOfNights int, endingAt time.Time) []domain.ViewingSession {
	seed := deterministicSeedForUser(userIdentifier)
	sessions := make([]domain.ViewingSession, 0, numberOfNights)

	// Anchor to midnight of endingAt's calendar date so that adding whole
	// days always lands on the same wall-clock date arithmetic regardless
	// of what time-of-day endingAt itself carries.
	anchorMidnight := time.Date(endingAt.Year(), endingAt.Month(), endingAt.Day(), 0, 0, 0, 0, endingAt.Location())

	for nightIndex := 0; nightIndex < numberOfNights; nightIndex++ {
		daysBeforeEnd := numberOfNights - 1 - nightIndex
		sessionDate := anchorMidnight.AddDate(0, 0, -daysBeforeEnd)

		jitterMinutes := jitterMinutesForNight(seed, nightIndex)
		sessionStart := sessionDate.Add(time.Duration(nightlyBaseHour) * time.Hour).
			Add(time.Duration(nightlyBaseMinute) * time.Minute).
			Add(time.Duration(jitterMinutes) * time.Minute)

		sessions = append(sessions, domain.ViewingSession{
			UserIdentifier:   userIdentifier,
			SeriesIdentifier: DefaultSeriesIdentifier,
			AssetIdentifier:  fmt.Sprintf("%s-e%02d", DefaultSeriesIdentifier, nightIndex+1),
			StartedAtUTC:     sessionStart,
		})
	}

	return sessions
}

// deterministicSeedForUser derives a stable numeric seed from a user
// identifier string using FNV-1a, a fast non-cryptographic hash well suited
// to turning arbitrary strings into deterministic seeds for exactly this
// kind of reproducible-synthetic-data use case.
func deterministicSeedForUser(userIdentifier string) uint32 {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(userIdentifier))
	return hasher.Sum32()
}

// jitterMinutesForNight returns a small, deterministic pseudo-random jitter
// in minutes (range -6..+6) for the given night index, derived from seed.
// Uses a trivial multiplicative mix rather than a full PRNG — sufficient
// for generating visually-varied-but-reproducible synthetic demo data; this
// is explicitly not intended as a statistically rigorous random number
// generator.
func jitterMinutesForNight(seed uint32, nightIndex int) int {
	mixed := seed*2654435761 + uint32(nightIndex)*40503
	// Fold into a small range: -6..+6 minutes.
	return int(mixed%13) - 6
}
