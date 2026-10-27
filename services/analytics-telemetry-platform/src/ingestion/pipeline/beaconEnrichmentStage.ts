/**
 * @file Enrichment stage of the beacon ingestion Pipeline pattern.
 *
 * Third stage of validate -> normalize -> enrich -> batch-insert. Takes a
 * `NormalizedPlaybackBeacon` and attaches derived fields: chunk provenance
 * resolution (which encoding worker produced the underlying chunk, via
 * `ChunkProvenanceLookup`) and the ingestion timestamp. This stage does
 * perform I/O (an async lookup against the provenance store), unlike the
 * validation and normalization stages, so it is structurally correct but
 * exercised only implicitly by higher-level integration tests /
 * live-cluster runs -- its logic is otherwise a thin, deterministic
 * mapping and is safe to reason about in isolation.
 */
import type { ChunkProvenanceLookup } from '../../tracing/chunkProvenanceLookup';
import type { EnrichedPlaybackBeacon, NormalizedPlaybackBeacon } from '../../types/beaconTypes';

/**
 * Enriches a single normalized beacon with resolved chunk-provenance
 * information and an ingestion timestamp.
 *
 * @param normalizedBeacon - A beacon that has passed validation and
 * normalization.
 * @param chunkProvenanceLookup - Strategy implementation used to resolve
 * `traceId` -> encoding worker id.
 * @param clock - Injectable clock function (defaults to `Date.now`),
 * enabling deterministic testing of the ingestion timestamp.
 * @returns The enriched beacon, ready for batch insertion into ClickHouse.
 */
export async function enrichNormalizedPlaybackBeacon(
  normalizedBeacon: NormalizedPlaybackBeacon,
  chunkProvenanceLookup: ChunkProvenanceLookup,
  clock: () => number = Date.now,
): Promise<EnrichedPlaybackBeacon> {
  const provenanceRecord = normalizedBeacon.traceId
    ? await chunkProvenanceLookup.resolveChunkProvenanceForTraceId(normalizedBeacon.traceId)
    : null;

  return {
    ...normalizedBeacon,
    sourceEncodingWorkerId: provenanceRecord?.encodingWorkerId ?? null,
    ingestedAtEpochMillis: clock(),
  };
}

/**
 * Enriches a batch of normalized beacons concurrently.
 *
 * @param normalizedBeacons - Array of validated+normalized beacons.
 * @param chunkProvenanceLookup - Strategy implementation for provenance
 * resolution.
 * @param clock - Injectable clock function (defaults to `Date.now`).
 * @returns Array of enriched beacons, same order as input.
 */
export async function enrichNormalizedPlaybackBeaconBatch(
  normalizedBeacons: NormalizedPlaybackBeacon[],
  chunkProvenanceLookup: ChunkProvenanceLookup,
  clock: () => number = Date.now,
): Promise<EnrichedPlaybackBeacon[]> {
  return Promise.all(
    normalizedBeacons.map((beacon) => enrichNormalizedPlaybackBeacon(beacon, chunkProvenanceLookup, clock)),
  );
}
