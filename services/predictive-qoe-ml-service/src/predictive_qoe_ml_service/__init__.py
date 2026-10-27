"""Predictive QoE ML Service.

A microservice that predicts imminent client-side video buffer degradation
events (rebuffering caused by ISP-side bandwidth degradation) ahead of time,
using an unsupervised Isolation Forest anomaly-detection model trained on
rolling-window statistics of throughput, jitter, and round-trip-time (RTT)
network telemetry.

Package layout:
    feature_engineering -- pure, unit-testable rolling-window feature
        extraction functions and pluggable Strategy-pattern implementations.
    ml -- the Isolation Forest model wrapper, synthetic data generator, and
        the training/evaluation harness.
    pipeline -- the Pipeline-pattern orchestration that chains
        raw samples -> features -> model inference -> decision.
    websocket -- the in-memory session-socket Registry/Repository and the
        downshift-command emitter that pushes structured commands to
        connected client player sockets.
    api -- the FastAPI application exposing the public HTTP prediction
        endpoint.

Naming convention note: this package is written in idiomatic Python, so all
module, function, and variable names use snake_case per PEP 8, even though
the broader platform this microservice belongs to follows a camelCase naming
philosophy for its TypeScript/JavaScript services. See the README's
"Naming Convention" section for the full rationale and the field-name
mapping used at the JSON API boundary (camelCase in/out, snake_case inside).
"""

__version__ = "0.1.0"
