"""Boundary/integration adapters: ffprobe subprocess client and frame source.

Everything in this sub-package talks to the outside world (subprocess
execution, filesystem, OpenCV video decode). This isolation is what
lets the rest of the codebase -- domain model, strategies, pipeline
stage logic -- be unit tested with synthetic in-memory data and no real
video file or external binary dependency.
"""

from ingestion_inspection_service.adapters.audio_extraction import (
    AudioExtractionError,
    extract_mono_pcm_via_ffmpeg,
)
from ingestion_inspection_service.adapters.ffprobe_client import (
    FfprobeExecutionError,
    extract_typed_stream_metadata_via_ffprobe,
)
from ingestion_inspection_service.adapters.frame_source import (
    SampledFrame,
    VideoDecodeError,
    sample_frames_at_fixed_rate,
)

__all__ = [
    "AudioExtractionError",
    "extract_mono_pcm_via_ffmpeg",
    "FfprobeExecutionError",
    "extract_typed_stream_metadata_via_ffprobe",
    "SampledFrame",
    "VideoDecodeError",
    "sample_frames_at_fixed_rate",
]
