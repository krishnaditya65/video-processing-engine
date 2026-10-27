"""Domain model sub-package.

Contains only pure ``dataclasses`` describing the shape of an asset
inspection result. Nothing here performs I/O or imports OpenCV/FFprobe
directly, so the domain model can be constructed and unit-tested in
complete isolation from real video files or the ``ffprobe`` binary.
"""

from ingestion_inspection_service.domain.models import (
    AudioStreamProfile,
    BlackFrameRange,
    ColorSpaceProfile,
    HdrFormat,
    InspectionRequest,
    LoudnessAnalysisResult,
    SceneBoundary,
    StructuralMap,
    VideoStreamProfile,
)

__all__ = [
    "AudioStreamProfile",
    "BlackFrameRange",
    "ColorSpaceProfile",
    "HdrFormat",
    "InspectionRequest",
    "LoudnessAnalysisResult",
    "SceneBoundary",
    "StructuralMap",
    "VideoStreamProfile",
]
