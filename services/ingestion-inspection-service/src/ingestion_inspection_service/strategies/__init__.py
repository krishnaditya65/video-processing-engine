"""Strategy-pattern implementations of pluggable scene-cut detection.

Design pattern: **Strategy**. ``SceneDetectionStrategy`` defines a common
interface (``compute_frame_to_frame_dissimilarity`` +
``get_cut_threshold`` + ``get_strategy_name``) so the pipeline stage that
uses it does not need to know whether it is running histogram-difference
analysis or structural-similarity (SSIM) analysis. New algorithms can be
added by implementing the interface without modifying
``pipeline.stages.SceneCutDetectionStage``.
"""

from ingestion_inspection_service.strategies.scene_detection import (
    HistogramDifferenceSceneDetectionStrategy,
    SceneDetectionStrategy,
    StructuralSimilaritySceneDetectionStrategy,
    detect_scene_boundaries_via_adaptive_thresholding,
)

__all__ = [
    "HistogramDifferenceSceneDetectionStrategy",
    "SceneDetectionStrategy",
    "StructuralSimilaritySceneDetectionStrategy",
    "detect_scene_boundaries_via_adaptive_thresholding",
]
