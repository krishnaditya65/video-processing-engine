/**
 * @file remediationActionReceivedEvent.ts
 * A structured (rather than free-text) log event describing receipt of a remediation
 * action from an internal caller. Modelled as a small value class (rather than just a
 * `logger.log('some string')` call) so the shape is documented and reusable — a future
 * addition (e.g. shipping these to a structured log sink / metrics pipeline) has a
 * single well-typed event to serialise instead of parsing free-text log lines.
 */

import { RemediationActionType } from '../registry/remediationActionRecord.interface';

/** Structured event emitted whenever {@link InternalRemediationController} accepts a remediation action. */
export class RemediationActionReceivedEvent {
  public constructor(
    /** Which kind of remediation action was received. */
    public readonly actionType: RemediationActionType,
    /** The registry-assigned identifier for this action (see {@link ActiveRemediationActionsRegistry}). */
    public readonly remediationActionIdentifier: string,
    /** Human-readable reason supplied by the caller. */
    public readonly reason: string,
    /** ISO-8601 timestamp this service received the action. */
    public readonly receivedAtIsoTimestamp: string,
  ) {}

  /**
   * Renders this event as a flat, JSON-serialisable object suitable for structured
   * logging (e.g. `logger.log(event.toStructuredLogPayload())`), so log aggregation
   * tooling can index on `actionType`/`remediationActionIdentifier` without parsing a
   * free-text message.
   * @returns A plain object representation of this event.
   */
  public toStructuredLogPayload(): Record<string, string> {
    return {
      event: 'RemediationActionReceivedEvent',
      actionType: this.actionType,
      remediationActionIdentifier: this.remediationActionIdentifier,
      reason: this.reason,
      receivedAtIsoTimestamp: this.receivedAtIsoTimestamp,
    };
  }
}
