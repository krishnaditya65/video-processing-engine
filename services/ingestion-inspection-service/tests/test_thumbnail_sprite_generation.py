"""Unit + integration tests for sprite-sheet compositing and WebVTT indexing.

Sprite-sheet coordinate math and compositing are pure ``numpy``/``cv2``
operations tested directly against small synthetic frames. The
ffmpeg-backed representative-frame extraction is exercised against the
shared synthetic video fixture and skipped automatically when
ffmpeg/ffprobe are unavailable.
"""

from __future__ import annotations

import numpy as np
import pytest

from ingestion_inspection_service.adapters.thumbnail_sprite_generation import (
    ThumbnailExtractionError,
    build_webvtt_sprite_index,
    composite_sprite_sheet_from_scene_frames,
    extract_representative_frame_via_ffmpeg,
)
from ingestion_inspection_service.config import SpriteSheetGenerationConfiguration
from tests.conftest import ffmpeg_and_ffprobe_are_available, make_solid_color_frame


class TestCompositeSpriteSheetFromSceneFrames:
    """Tests the pure sprite-sheet grid compositing and coordinate math."""

    def test_single_scene_produces_one_tile_sized_sheet(self) -> None:
        """One input frame produces a sheet exactly one thumbnail in size."""
        configuration = SpriteSheetGenerationConfiguration(
            thumbnail_width_pixels=20, thumbnail_height_pixels=10, sprite_sheet_columns=5
        )
        frame = make_solid_color_frame(64, 64, (10, 20, 30))

        result = composite_sprite_sheet_from_scene_frames(
            [frame], [0.0], [1.0], configuration
        )

        assert result.sprite_sheet_image_bgr.shape == (10, 20, 3)
        assert len(result.thumbnail_rectangles) == 1
        rect = result.thumbnail_rectangles[0]
        assert (rect.x_pixels, rect.y_pixels) == (0, 0)
        assert (rect.width_pixels, rect.height_pixels) == (20, 10)

    def test_grid_wraps_to_new_row_after_column_count(self) -> None:
        """The (columns + 1)-th thumbnail wraps to row index 1."""
        configuration = SpriteSheetGenerationConfiguration(
            thumbnail_width_pixels=10, thumbnail_height_pixels=10, sprite_sheet_columns=3
        )
        frames = [make_solid_color_frame(16, 16, (i, i, i)) for i in range(4)]
        timestamps = [float(i) for i in range(4)]

        result = composite_sprite_sheet_from_scene_frames(
            frames, timestamps, [t + 1.0 for t in timestamps], configuration
        )

        # 4 thumbnails, 3 columns -> 2 rows -> sheet is 30x20.
        assert result.sprite_sheet_image_bgr.shape == (20, 30, 3)
        fourth_rect = result.thumbnail_rectangles[3]
        assert fourth_rect.x_pixels == 0
        assert fourth_rect.y_pixels == 10  # wrapped to the second row

    def test_thumbnail_rectangles_do_not_overlap(self) -> None:
        """No two thumbnail rectangles in the same sheet overlap in pixel space."""
        configuration = SpriteSheetGenerationConfiguration(
            thumbnail_width_pixels=8, thumbnail_height_pixels=8, sprite_sheet_columns=4
        )
        frames = [make_solid_color_frame(8, 8, (0, 0, 0)) for _ in range(9)]
        timestamps = [float(i) for i in range(9)]

        result = composite_sprite_sheet_from_scene_frames(
            frames, timestamps, [t + 1.0 for t in timestamps], configuration
        )

        seen_cells = set()
        for rect in result.thumbnail_rectangles:
            cell = (rect.x_pixels // 8, rect.y_pixels // 8)
            assert cell not in seen_cells
            seen_cells.add(cell)

    def test_scene_metadata_is_preserved_in_rectangles(self) -> None:
        """Each rectangle records the originating scene's index and timestamps."""
        frame = make_solid_color_frame(16, 16, (1, 2, 3))
        result = composite_sprite_sheet_from_scene_frames(
            [frame, frame.copy()], [0.0, 5.0], [5.0, 12.5]
        )
        assert result.thumbnail_rectangles[1].scene_index == 1
        assert result.thumbnail_rectangles[1].start_timestamp_seconds == 5.0
        assert result.thumbnail_rectangles[1].end_timestamp_seconds == 12.5

    def test_empty_scene_frames_raises_value_error(self) -> None:
        """Zero input frames raises ValueError rather than an empty sheet."""
        with pytest.raises(ValueError):
            composite_sprite_sheet_from_scene_frames([], [], [])

    def test_mismatched_lengths_raise_value_error(self) -> None:
        """Mismatched timestamp/frame list lengths raise ValueError."""
        frame = make_solid_color_frame(8, 8, (0, 0, 0))
        with pytest.raises(ValueError):
            composite_sprite_sheet_from_scene_frames([frame], [0.0, 1.0], [1.0])


class TestBuildWebvttSpriteIndex:
    """Tests the WebVTT sprite index text generation."""

    def test_produces_webvtt_header(self) -> None:
        """The generated document starts with the mandatory WEBVTT header line."""
        frame = make_solid_color_frame(8, 8, (0, 0, 0))
        result = composite_sprite_sheet_from_scene_frames([frame], [0.0], [1.0])
        vtt_text = build_webvtt_sprite_index(
            result.thumbnail_rectangles, "sheet.png"
        )
        assert vtt_text.startswith("WEBVTT")

    def test_cue_references_media_fragment_with_correct_coordinates(self) -> None:
        """Each cue's text references sheet.png with an xywh media fragment."""
        configuration = SpriteSheetGenerationConfiguration(
            thumbnail_width_pixels=10, thumbnail_height_pixels=10, sprite_sheet_columns=2
        )
        frames = [make_solid_color_frame(8, 8, (0, 0, 0)) for _ in range(2)]
        result = composite_sprite_sheet_from_scene_frames(
            frames, [0.0, 3.0], [3.0, 6.0], configuration
        )
        vtt_text = build_webvtt_sprite_index(result.thumbnail_rectangles, "sheet.png")

        assert "sheet.png#xywh=0,0,10,10" in vtt_text
        assert "sheet.png#xywh=10,0,10,10" in vtt_text

    def test_cue_count_matches_rectangle_count(self) -> None:
        """One timing line is emitted per thumbnail rectangle."""
        frames = [make_solid_color_frame(8, 8, (0, 0, 0)) for _ in range(3)]
        result = composite_sprite_sheet_from_scene_frames(
            frames, [0.0, 1.0, 2.0], [1.0, 2.0, 3.0]
        )
        vtt_text = build_webvtt_sprite_index(result.thumbnail_rectangles, "sheet.png")
        assert vtt_text.count("-->") == 3


@pytest.mark.skipif(
    not ffmpeg_and_ffprobe_are_available(),
    reason="ffmpeg/ffprobe not available in this environment",
)
class TestExtractRepresentativeFrameAgainstRealFile:
    """Integration tests for the ffmpeg-backed single-frame extraction."""

    def test_extracts_a_decodable_frame_at_a_timestamp(
        self, synthetic_test_video_path: str
    ) -> None:
        """A frame extracted from the real fixture decodes to the expected shape."""
        frame = extract_representative_frame_via_ffmpeg(
            synthetic_test_video_path, 0.2
        )
        assert frame.shape == (64, 64, 3)
        assert frame.dtype == np.uint8

    def test_nonexistent_file_raises_thumbnail_extraction_error(self) -> None:
        """Extracting from a nonexistent file raises ThumbnailExtractionError."""
        with pytest.raises(ThumbnailExtractionError):
            extract_representative_frame_via_ffmpeg(
                "/no/such/file/anywhere.mov", 0.0
            )
