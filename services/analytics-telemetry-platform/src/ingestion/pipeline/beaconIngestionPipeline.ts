/**
 * @file Pipeline pattern orchestrator wiring the four beacon-ingestion
 * stages together: validate -> normalize -> enrich -> batch-insert.
 *
 * This is the single entry point the Kafka/Redpanda consumer calls per
 * fetched message batch. Each stage is independently unit-tested
 * (validation and normalization are pure; enrichment and batch-insert
 * involve I/O and are covered structurally, per README "Testing
 * Strategy"), and this orchestrator composes them without adding business
 * logic of its own -- keeping the Pipeline pattern's stage boundaries
 * clean and each stage independently swappable/testable.
 */
import {
  validateRawPlaybackBeaconWireMessageBatch,
} from './beaconValidationStage';
import { normalizeValidatedPlaybackBeaconBatch } from './beaconNormalizationStage';
import { enrichNormalizedPlaybackBeaconBatch } from './beaconEnrichmentStage';
import { BeaconMicroBatchAccumulator } from './beaconBatchInsertStage';
import type { ChunkProvenanceLookup } from '../../tracing/chunkProvenanceLookup';
import type { RawPlaybackBeaconWireMessage } from '../../types/beaconTypes';

/** Outcome summary of running one raw message batch through the pipeline. */
export interface BeaconIngestionPipelineRunResult {
  /** Count of beacons that passed validation and were enriched/enqueued. */
  acceptedCount: number;
  /** Count of beacons rejected at the validation stage, with reasons. */
  rejectedCount: number;
  /** Validation error details for rejected beacons, for dead-lettering/logging. */
  rejections: Array<{ rawMessage: RawPlaybackBeaconWireMessage; validationErrors: string[] }>;
}

/**
 * Runs one fetched batch of raw Kafka/Redpanda message values through the
 * full validate -> normalize -> enrich -> batch-insert pipeline.
 *
 * @param rawMessages - Raw, untrusted beacon wire messages fetched from
 * Kafka/Redpanda in this poll cycle.
 * @param chunkProvenanceLookup - Strategy implementation for the
 * enrichment stage's traceId -> encoding-worker resolution.
 * @param microBatchAccumulator - Shared accumulator that buffers enriched
 * beacons and flushes to ClickHouse per its own size/time triggers.
 * @returns Summary counts of accepted/rejected beacons for this run, plus
 * rejection detail for dead-lettering.
 */
export async function runPlaybackBeaconIngestionPipeline(
  rawMessages: RawPlaybackBeaconWireMessage[],
  chunkProvenanceLookup: ChunkProvenanceLookup,
  microBatchAccumulator: BeaconMicroBatchAccumulator,
): Promise<BeaconIngestionPipelineRunResult> {
  const { validBeacons, invalidBeacons } = validateRawPlaybackBeaconWireMessageBatch(rawMessages);

  const normalizedBeacons = normalizeValidatedPlaybackBeaconBatch(validBeacons);
  const enrichedBeacons = await enrichNormalizedPlaybackBeaconBatch(normalizedBeacons, chunkProvenanceLookup);

  for (const beacon of enrichedBeacons) {
    await microBatchAccumulator.addBeaconToBatch(beacon);
  }

  return {
    acceptedCount: enrichedBeacons.length,
    rejectedCount: invalidBeacons.length,
    rejections: invalidBeacons,
  };
}
