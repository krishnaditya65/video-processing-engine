#!/usr/bin/env python3
"""CLI entry point: run the training/evaluation harness and persist the model.

Usage:
    python scripts/run_training_harness.py

Generates the synthetic labeled dataset, trains the Isolation Forest,
prints real precision/recall/F1 metrics computed on a held-out synthetic
test split, and saves the trained model bundle to
``models/isolation_forest_bandwidth_degradation_model.joblib`` so the
FastAPI service can load it at startup instead of training on the fly.
"""

from __future__ import annotations

import sys
from pathlib import Path

# Allow running this script directly (``python scripts/run_training_harness.py``)
# without first installing the package, by putting src/ on sys.path.
_SERVICE_ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(_SERVICE_ROOT / "src"))

from predictive_qoe_ml_service import config  # noqa: E402
from predictive_qoe_ml_service.ml.training_evaluation_harness import (  # noqa: E402
    _print_training_evaluation_report,
    run_isolation_forest_training_and_evaluation_harness,
)


def main() -> None:
    """Run the harness end to end and persist the resulting trained model."""
    report = run_isolation_forest_training_and_evaluation_harness()
    _print_training_evaluation_report(report)

    models_directory = Path(config.TRAINED_MODEL_FILE_PATH).parent
    models_directory.mkdir(parents=True, exist_ok=True)
    report.trained_model.save_trained_model_to_disk(config.TRAINED_MODEL_FILE_PATH)
    print(f"\nSaved trained model to {config.TRAINED_MODEL_FILE_PATH}")


if __name__ == "__main__":
    main()
