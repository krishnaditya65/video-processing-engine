#!/usr/bin/env node
/**
 * aggregate-cost-report.js
 *
 * Standalone, dependency-free Node.js script that reads a per-title cost
 * report fixture (JSON) and prints a formatted per-title unit-economics
 * breakdown: encoding compute cost (CPU/GPU-seconds, priced per
 * distributed-encoding-orchestrator's real CostAccountant model — see
 * services/distributed-encoding-orchestrator/internal/cost/costAccountant.go),
 * origin storage cost, and CDN egress cost, aggregated into a total cost
 * and a cost-per-viewing-hour figure per title.
 *
 * This script is ACTUALLY RUNNABLE and was ACTUALLY RUN in this
 * environment against the fixture at
 * scripts/fixtures/sample-per-title-cost-report.json — see
 * docs/cost-dashboard.md for the real command + real output captured
 * from that run.
 *
 * Usage:
 *   node scripts/aggregate-cost-report.js [path/to/cost-report.json]
 *
 * If no path is given, defaults to
 * scripts/fixtures/sample-per-title-cost-report.json (relative to this
 * script's own directory, so it works regardless of the caller's cwd).
 *
 * Input shape (see the fixture file for a complete real example):
 *   {
 *     "computeResourceRates": { "cpuSecondPriceUsd": number, "gpuSecondPriceUsd": number },
 *     "storageRates": { "originStorageGigabyteMonthPriceUsd": number },
 *     "cdnEgressRates": { "gigabyteEgressPriceUsd": number },
 *     "titles": [
 *       {
 *         "titleId": string,
 *         "displayTitle": string,
 *         "encodingCost": { "chunkCount": number, "totalCpuSeconds": number, "totalGpuSeconds": number, "totalEstimatedCostUsd": number },
 *         "originStorage": { "elementaryStreamsGigabytes": number, "gigabyteMonths": number, "estimatedCostUsd": number },
 *         "cdnEgress": { "gigabytesServed": number, "estimatedCostUsd": number },
 *         "viewingHoursServed": number
 *       },
 *       ...
 *     ]
 *   }
 *
 * The `encodingCost.totalEstimatedCostUsd` / `originStorage.estimatedCostUsd`
 * / `cdnEgress.estimatedCostUsd` fields, if present in the fixture, are
 * cross-checked against a recomputation from the raw quantities and rates
 * (and flagged with a "[recomputed, fixture value differed]" note if they
 * disagree by more than a floating-point rounding tolerance) — this
 * script does not blindly trust pre-computed costs in its input.
 */

'use strict';

const fs = require('node:fs');
const path = require('node:path');

const FLOATING_POINT_COMPARISON_TOLERANCE_USD = 0.005;

function formatUsd(amountUsd) {
  return `$${amountUsd.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
}

function formatUsdPrecise(amountUsd) {
  return `$${amountUsd.toLocaleString('en-US', { minimumFractionDigits: 4, maximumFractionDigits: 4 })}`;
}

function formatNumber(value, fractionDigits = 1) {
  return value.toLocaleString('en-US', { minimumFractionDigits: fractionDigits, maximumFractionDigits: fractionDigits });
}

/**
 * Recomputes a title's encoding cost from raw CPU/GPU-seconds and the
 * report's compute resource rates, mirroring
 * CostAccountant.RecordChunkCompletion's `resourceSecondsConsumed * rate`
 * arithmetic (services/distributed-encoding-orchestrator/internal/cost/costAccountant.go).
 */
function computeEncodingCostUsd(encodingCost, computeResourceRates) {
  const cpuCostUsd = encodingCost.totalCpuSeconds * computeResourceRates.cpuSecondPriceUsd;
  const gpuCostUsd = encodingCost.totalGpuSeconds * computeResourceRates.gpuSecondPriceUsd;
  return cpuCostUsd + gpuCostUsd;
}

function computeStorageCostUsd(originStorage, storageRates) {
  return originStorage.gigabyteMonths * storageRates.originStorageGigabyteMonthPriceUsd;
}

function computeCdnEgressCostUsd(cdnEgress, cdnEgressRates) {
  return cdnEgress.gigabytesServed * cdnEgressRates.gigabyteEgressPriceUsd;
}

function reconcileComputedVsFixtureCost(label, computedUsd, fixtureUsd) {
  const differs = Math.abs(computedUsd - fixtureUsd) > FLOATING_POINT_COMPARISON_TOLERANCE_USD;
  return {
    valueUsd: computedUsd,
    note: differs ? `[recomputed ${formatUsdPrecise(computedUsd)}, fixture value ${formatUsdPrecise(fixtureUsd)} differed]` : null,
  };
}

function buildPerTitleBreakdown(titleEntry, computeResourceRates, storageRates, cdnEgressRates) {
  const recomputedEncodingCostUsd = computeEncodingCostUsd(titleEntry.encodingCost, computeResourceRates);
  const recomputedStorageCostUsd = computeStorageCostUsd(titleEntry.originStorage, storageRates);
  const recomputedCdnEgressCostUsd = computeCdnEgressCostUsd(titleEntry.cdnEgress, cdnEgressRates);

  const encodingReconciliation = reconcileComputedVsFixtureCost(
    'encoding',
    recomputedEncodingCostUsd,
    titleEntry.encodingCost.totalEstimatedCostUsd,
  );
  const storageReconciliation = reconcileComputedVsFixtureCost(
    'storage',
    recomputedStorageCostUsd,
    titleEntry.originStorage.estimatedCostUsd,
  );
  const cdnEgressReconciliation = reconcileComputedVsFixtureCost(
    'cdnEgress',
    recomputedCdnEgressCostUsd,
    titleEntry.cdnEgress.estimatedCostUsd,
  );

  const totalCostUsd =
    encodingReconciliation.valueUsd + storageReconciliation.valueUsd + cdnEgressReconciliation.valueUsd;

  const costPerViewingHourUsd = titleEntry.viewingHoursServed > 0 ? totalCostUsd / titleEntry.viewingHoursServed : null;

  return {
    titleId: titleEntry.titleId,
    displayTitle: titleEntry.displayTitle,
    encodingCostUsd: encodingReconciliation.valueUsd,
    encodingReconciliationNote: encodingReconciliation.note,
    storageCostUsd: storageReconciliation.valueUsd,
    storageReconciliationNote: storageReconciliation.note,
    cdnEgressCostUsd: cdnEgressReconciliation.valueUsd,
    cdnEgressReconciliationNote: cdnEgressReconciliation.note,
    totalCostUsd,
    viewingHoursServed: titleEntry.viewingHoursServed,
    costPerViewingHourUsd,
    chunkCount: titleEntry.encodingCost.chunkCount,
    totalCpuSeconds: titleEntry.encodingCost.totalCpuSeconds,
    totalGpuSeconds: titleEntry.encodingCost.totalGpuSeconds,
  };
}

function printTitleBreakdown(breakdown) {
  const encodingSharePercent = (breakdown.encodingCostUsd / breakdown.totalCostUsd) * 100;
  const storageSharePercent = (breakdown.storageCostUsd / breakdown.totalCostUsd) * 100;
  const cdnEgressSharePercent = (breakdown.cdnEgressCostUsd / breakdown.totalCostUsd) * 100;

  console.log(`\n${breakdown.displayTitle}  (${breakdown.titleId})`);
  console.log('-'.repeat(72));
  console.log(
    `  Encoding compute   ${formatUsdPrecise(breakdown.encodingCostUsd).padStart(12)}  ` +
      `(${formatNumber(encodingSharePercent)}%)   ` +
      `${breakdown.chunkCount} chunks, ${formatNumber(breakdown.totalCpuSeconds)} CPU-s / ${formatNumber(breakdown.totalGpuSeconds)} GPU-s` +
      (breakdown.encodingReconciliationNote ? `\n    ${breakdown.encodingReconciliationNote}` : ''),
  );
  console.log(
    `  Origin storage     ${formatUsdPrecise(breakdown.storageCostUsd).padStart(12)}  (${formatNumber(storageSharePercent)}%)` +
      (breakdown.storageReconciliationNote ? `\n    ${breakdown.storageReconciliationNote}` : ''),
  );
  console.log(
    `  CDN egress         ${formatUsdPrecise(breakdown.cdnEgressCostUsd).padStart(12)}  (${formatNumber(cdnEgressSharePercent)}%)` +
      (breakdown.cdnEgressReconciliationNote ? `\n    ${breakdown.cdnEgressReconciliationNote}` : ''),
  );
  console.log(`  ${'TOTAL'.padEnd(19)}${formatUsd(breakdown.totalCostUsd).padStart(12)}`);
  console.log(
    `  Viewing hours served: ${breakdown.viewingHoursServed.toLocaleString('en-US')}` +
      (breakdown.costPerViewingHourUsd !== null
        ? `   ->  ${formatUsdPrecise(breakdown.costPerViewingHourUsd)} / viewing-hour`
        : ''),
  );
}

function printAggregateSummary(breakdowns) {
  const totalEncodingCostUsd = breakdowns.reduce((sum, b) => sum + b.encodingCostUsd, 0);
  const totalStorageCostUsd = breakdowns.reduce((sum, b) => sum + b.storageCostUsd, 0);
  const totalCdnEgressCostUsd = breakdowns.reduce((sum, b) => sum + b.cdnEgressCostUsd, 0);
  const grandTotalCostUsd = totalEncodingCostUsd + totalStorageCostUsd + totalCdnEgressCostUsd;
  const totalViewingHours = breakdowns.reduce((sum, b) => sum + b.viewingHoursServed, 0);

  console.log(`\n${'='.repeat(72)}`);
  console.log('AGGREGATE — ALL TITLES');
  console.log('='.repeat(72));
  console.log(`  Titles in report:        ${breakdowns.length}`);
  console.log(`  Total encoding cost:     ${formatUsd(totalEncodingCostUsd)}`);
  console.log(`  Total origin storage:    ${formatUsd(totalStorageCostUsd)}`);
  console.log(`  Total CDN egress:        ${formatUsd(totalCdnEgressCostUsd)}`);
  console.log(`  GRAND TOTAL:             ${formatUsd(grandTotalCostUsd)}`);
  console.log(`  Total viewing hours:     ${totalViewingHours.toLocaleString('en-US')}`);
  if (totalViewingHours > 0) {
    console.log(`  Blended cost/view-hour:  ${formatUsdPrecise(grandTotalCostUsd / totalViewingHours)}`);
  }

  const sortedByTotalCostDescending = [...breakdowns].sort((a, b) => b.totalCostUsd - a.totalCostUsd);
  console.log('\n  Ranked by total cost (highest first):');
  for (const [index, breakdown] of sortedByTotalCostDescending.entries()) {
    console.log(
      `    ${index + 1}. ${breakdown.displayTitle.padEnd(40)} ${formatUsd(breakdown.totalCostUsd).padStart(12)}`,
    );
  }
}

function main() {
  const defaultFixturePath = path.join(__dirname, 'fixtures', 'sample-per-title-cost-report.json');
  const inputPath = process.argv[2] ? path.resolve(process.argv[2]) : defaultFixturePath;

  let rawFileContents;
  try {
    rawFileContents = fs.readFileSync(inputPath, 'utf8');
  } catch (readError) {
    console.error(`Failed to read cost report fixture at ${inputPath}: ${readError.message}`);
    process.exitCode = 1;
    return;
  }

  let costReport;
  try {
    costReport = JSON.parse(rawFileContents);
  } catch (parseError) {
    console.error(`Failed to parse cost report fixture as JSON: ${parseError.message}`);
    process.exitCode = 1;
    return;
  }

  console.log('Per-Title Unit-Economics Cost Report');
  console.log(`Source file: ${inputPath}`);
  console.log(`Report generated at (fixture timestamp): ${costReport.reportGeneratedAt}`);
  console.log(
    `Billing window: ${costReport.billingWindow.startDate} .. ${costReport.billingWindow.endDate}`,
  );

  const breakdowns = costReport.titles.map((titleEntry) =>
    buildPerTitleBreakdown(titleEntry, costReport.computeResourceRates, costReport.storageRates, costReport.cdnEgressRates),
  );

  for (const breakdown of breakdowns) {
    printTitleBreakdown(breakdown);
  }

  printAggregateSummary(breakdowns);
}

main();
