"""Chain-of-Responsibility inspection pipeline sub-package.

Design pattern: **Chain of Responsibility** (also describable as a
linear **Pipeline**). Each concrete ``InspectionStage`` performs one
kind of analysis (black-frame detection, scene-cut detection,
color-space extraction, loudness analysis), mutates a shared
``InspectionContext`` with its findings, and forwards the context to
the next stage in the chain. ``InspectionPipeline`` assembles the
default four-stage chain in the required order:

    black-frame detection -> scene-cut detection -> color-space
    extraction -> loudness analysis

New stages can be inserted, removed, or reordered without touching the
stages themselves, since each stage only depends on the shared
``InspectionContext`` contract, not on its neighbors directly.
"""

from ingestion_inspection_service.pipeline.context import InspectionContext
from ingestion_inspection_service.pipeline.pipeline import (
    InspectionPipeline,
    build_default_inspection_pipeline,
)
from ingestion_inspection_service.pipeline.stages import (
    BlackFrameDetectionStage,
    ColorSpaceExtractionStage,
    InspectionStage,
    LoudnessAnalysisStage,
    SceneCutDetectionStage,
)

__all__ = [
    "InspectionContext",
    "InspectionPipeline",
    "build_default_inspection_pipeline",
    "BlackFrameDetectionStage",
    "ColorSpaceExtractionStage",
    "InspectionStage",
    "LoudnessAnalysisStage",
    "SceneCutDetectionStage",
]
