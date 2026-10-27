"""Unit tests for the pure dataclass domain model."""

from __future__ import annotations

import json

import pytest

from ingestion_inspection_service.domain.models import (
    BlackFrameRange,
    ColorSpaceProfile,
    HdrFormat,
    LoudnessAnalysisResult,
    SceneBoundary,
    StructuralMap,
)


def test_scene_boundary_duration_seconds_is_computed_correctly() -> None:
    """A SceneBoundary's duration_seconds property equals end minus start."""
    boundary = SceneBoundary(
        scene_index=0,
        start_timestamp_seconds=1.0,
        end_timestamp_seconds=4.5,
        detection_strategy_name="structural_similarity",
        cut_confidence_score=0.9,
        motion_complexity_score=0.3,
    )
    assert boundary.duration_seconds == pytest.approx(3.5)


def test_scene_boundary_rejects_non_positive_duration() -> None:
    """Constructing a SceneBoundary with end <= start raises ValueError."""
    with pytest.raises(ValueError):
        SceneBoundary(
            scene_index=0,
            start_timestamp_seconds=5.0,
            end_timestamp_seconds=5.0,
            detection_strategy_name="structural_similarity",
            cut_confidence_score=0.9,
            motion_complexity_score=0.3,
        )


def test_black_frame_range_duration_seconds() -> None:
    """A BlackFrameRange's duration_seconds property equals end minus start."""
    black_range = BlackFrameRange(
        start_timestamp_seconds=10.0,
        end_timestamp_seconds=12.5,
        mean_luminance_during_range=3.2,
        sampled_frame_count=3,
    )
    assert black_range.duration_seconds == pytest.approx(2.5)


def test_structural_map_serializes_enums_to_plain_strings() -> None:
    """to_json_serializable_dict converts HdrFormat enum members to strings."""
    structural_map = StructuralMap(
        asset_id="asset-1",
        source_file_path="/mnt/mezzanine/asset-1.mov",
        duration_seconds=120.0,
        video_stream_profile=None,
        audio_stream_profile=None,
        color_space_profile=ColorSpaceProfile(
            color_primaries="bt2020",
            color_transfer_characteristics="smpte2084",
            color_space_matrix="bt2020nc",
            is_bt2020=True,
            detected_hdr_format=HdrFormat.HDR10,
            has_mastering_display_metadata=True,
            has_content_light_level_metadata=True,
        ),
    )

    serialized = structural_map.to_json_serializable_dict()

    assert serialized["color_space_profile"]["detected_hdr_format"] == "HDR10"
    # Must be JSON-round-trippable with no custom encoder required.
    round_tripped = json.loads(json.dumps(serialized))
    assert round_tripped["asset_id"] == "asset-1"


def test_structural_map_to_json_string_round_trips() -> None:
    """to_json_string produces valid, parseable JSON with expected fields."""
    structural_map = StructuralMap(
        asset_id="asset-2",
        source_file_path="/tmp/asset-2.mov",
        duration_seconds=42.0,
        video_stream_profile=None,
        audio_stream_profile=None,
        color_space_profile=None,
        loudness_analysis=LoudnessAnalysisResult(
            integrated_loudness_lufs=-16.2,
            loudness_range_lu=5.1,
            true_peak_estimate_dbtp=-1.0,
            conforms_to_target=True,
            target_lufs_used=-16.0,
        ),
    )

    json_text = structural_map.to_json_string()
    parsed = json.loads(json_text)

    assert parsed["duration_seconds"] == 42.0
    assert parsed["loudness_analysis"]["integrated_loudness_lufs"] == pytest.approx(
        -16.2
    )


def test_structural_map_total_scene_and_black_frame_counts() -> None:
    """total_scene_count and total_black_frame_range_count reflect list lengths."""
    structural_map = StructuralMap(
        asset_id="asset-3",
        source_file_path="/tmp/asset-3.mov",
        duration_seconds=10.0,
        video_stream_profile=None,
        audio_stream_profile=None,
        color_space_profile=None,
        scene_boundaries=[
            SceneBoundary(0, 0.0, 5.0, "structural_similarity", 1.0, 0.1),
            SceneBoundary(1, 5.0, 10.0, "structural_similarity", 0.8, 0.2),
        ],
        black_frame_ranges=[
            BlackFrameRange(0.0, 1.0, 2.0, 2),
        ],
    )

    assert structural_map.total_scene_count == 2
    assert structural_map.total_black_frame_range_count == 1
