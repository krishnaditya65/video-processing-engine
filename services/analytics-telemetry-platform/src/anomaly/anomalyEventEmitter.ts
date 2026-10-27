/**
 * @file Observer pattern implementation for `AnomalyDetectedEvent`
 * consumers.
 *
 * `FirmwareCohortAnomalyDetector` (see `firmwareCohortAnomalyDetector.ts`)
 * publishes `AnomalyDetectedEvent`s through this typed EventEmitter
 * wrapper rather than calling consumers directly, decoupling detection
 * from response -- e.g. a paging/alerting integration, an auto-rollback
 * trigger for a canary firmware push, and a metrics/dashboard sink can all
 * subscribe independently without the detector knowing about any of them.
 */
import { EventEmitter } from 'node:events';

/** Payload emitted when a firmware cohort's decode-error rate crosses the anomaly threshold. */
export interface AnomalyDetectedEvent {
  /** The device firmware version identifying the affected cohort. */
  firmwareCohortId: string;
  /** Observed decode-error rate (errors per session) for the cohort in the current window. */
  observedErrorRate: number;
  /** Rolling baseline mean error rate the observation was compared against. */
  baselineMeanErrorRate: number;
  /** Rolling baseline sample standard deviation of error rate. */
  baselineStandardDeviationErrorRate: number;
  /** Computed z-score of the observation against the baseline. */
  zScore: number;
  /** The z-score threshold that was crossed to trigger this event. */
  thresholdCrossed: number;
  /** Epoch-milliseconds timestamp at which the anomaly was detected. */
  detectedAtEpochMillis: number;
}

/** Strongly typed event name -> payload map for `AnomalyEventEmitter`. */
interface AnomalyEventMap {
  anomalyDetected: [event: AnomalyDetectedEvent];
}

/**
 * Thin, strongly-typed wrapper around Node's `EventEmitter` restricted to
 * the single `anomalyDetected` event channel, implementing the
 * Observer/EventEmitter pattern referenced in README "Design Patterns".
 */
export class AnomalyEventEmitter extends EventEmitter {
  /**
   * Emits an `AnomalyDetectedEvent` to all subscribed observers.
   *
   * @param event - The anomaly event payload to broadcast.
   */
  public emitAnomalyDetected(event: AnomalyDetectedEvent): void {
    this.emit('anomalyDetected', event);
  }

  /**
   * Subscribes a listener to `AnomalyDetectedEvent`s.
   *
   * @param listener - Callback invoked with each emitted event.
   * @returns `this`, for chaining, matching `EventEmitter.on`'s contract.
   */
  public onAnomalyDetected(listener: (event: AnomalyDetectedEvent) => void): this {
    return this.on('anomalyDetected', listener);
  }
}

// Re-export the event map type for consumers that want compile-time
// checking of custom emitter subclasses; not used directly above because
// Node's EventEmitter typings vary by TS lib version, but documents intent.
export type { AnomalyEventMap };
