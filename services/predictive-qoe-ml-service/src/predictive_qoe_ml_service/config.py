"""Service-wide configuration constants.

Centralizes the handful of paths and tunables the API layer needs, kept
separate from business logic so deployment-specific overrides (e.g. via
environment variables in a real deployment) have one obvious place to
live.
"""

from __future__ import annotations

import os
from pathlib import Path

#: Root of the service (two levels up from this file:
#: src/predictive_qoe_ml_service/config.py -> service root).
SERVICE_ROOT_DIRECTORY = Path(__file__).resolve().parent.parent.parent

#: Default filesystem path for the persisted trained Isolation Forest
#: model bundle produced by the training harness
#: (``IsolationForestBandwidthDegradationModel.save_trained_model_to_disk``).
DEFAULT_TRAINED_MODEL_FILE_PATH = str(
    SERVICE_ROOT_DIRECTORY / "models" / "isolation_forest_bandwidth_degradation_model.joblib"
)

#: Overridable via the PREDICTIVE_QOE_MODEL_PATH environment variable so a
#: deployment can point at a differently versioned/located model artifact
#: without a code change.
TRAINED_MODEL_FILE_PATH = os.environ.get(
    "PREDICTIVE_QOE_MODEL_PATH", DEFAULT_TRAINED_MODEL_FILE_PATH
)
