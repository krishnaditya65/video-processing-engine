"""Unit + integration tests for subtitle detection/extraction/heuristics.

The WebVTT parsing and forced-narrative heuristic are pure string-
processing logic tested directly against hand-built WebVTT text (no
ffmpeg dependency). The ffprobe/ffmpeg-backed detection/extraction
functions are exercised against a real generated fixture and skipped
automatically when ffmpeg/ffprobe are unavailable, or when the
generated fixture (which has no subtitle streams) is used to verify the
"no subtitles present" self-skip/graceful-empty-result behavior.
"""

from __future__ import annotations

import pytest

from ingestion_inspection_service.adapters.subtitle_extraction import (
    SubtitleExtractionError,
    detect_forced_narrative_heuristic,
    detect_subtitle_streams,
    parse_webvtt_cues,
)
from tests.conftest import ffmpeg_and_ffprobe_are_available

_SPARSE_SHORT_CUES_VTT = """WEBVTT

00:00:01.000 --> 00:00:02.500
[speaking French]

00:01:15.000 --> 00:01:16.800
[sign: No Entry]
"""

_DENSE_DIALOGUE_VTT = """WEBVTT

00:00:00.000 --> 00:00:03.000
Hello there, how are you doing today?

00:00:03.500 --> 00:00:07.000
I'm doing well, thanks for asking!
Let's get going, shall we?

00:00:07.500 --> 00:00:11.000
Sure thing, right behind you.
"""


class TestParseWebvttCues:
    """Tests the minimal WebVTT cue parser."""

    def test_parses_expected_cue_count_and_text(self) -> None:
        """Two cues in the sample document are parsed with matching text."""
        cues = parse_webvtt_cues(_SPARSE_SHORT_CUES_VTT)
        assert len(cues) == 2
        assert cues[0].text == "[speaking French]"
        assert cues[1].text == "[sign: No Entry]"

    def test_parses_start_and_end_timestamps_in_seconds(self) -> None:
        """Cue timestamps are converted from HH:MM:SS.mmm into seconds."""
        cues = parse_webvtt_cues(_SPARSE_SHORT_CUES_VTT)
        assert cues[0].start_seconds == pytest.approx(1.0)
        assert cues[0].end_seconds == pytest.approx(2.5)
        assert cues[1].start_seconds == pytest.approx(75.0)

    def test_cue_duration_property(self) -> None:
        """duration_seconds equals end minus start for a parsed cue."""
        cues = parse_webvtt_cues(_SPARSE_SHORT_CUES_VTT)
        assert cues[0].duration_seconds == pytest.approx(1.5)

    def test_multiline_cue_text_is_joined_with_newline(self) -> None:
        """A cue spanning multiple text lines joins them with '\\n'."""
        cues = parse_webvtt_cues(_DENSE_DIALOGUE_VTT)
        assert "\n" in cues[1].text

    def test_header_only_document_parses_to_empty_list(self) -> None:
        """A WebVTT document with no cues parses to an empty list."""
        assert parse_webvtt_cues("WEBVTT\n\n") == []


class TestDetectForcedNarrativeHeuristic:
    """Tests the sparse/short-cue forced-narrative classification heuristic."""

    def test_sparse_short_cues_are_flagged_forced_narrative(self) -> None:
        """A sparse, short-cue track over a long asset is flagged forced-narrative."""
        cues = parse_webvtt_cues(_SPARSE_SHORT_CUES_VTT)
        result = detect_forced_narrative_heuristic(cues, asset_duration_seconds=120.0)
        assert result.is_likely_forced_narrative is True
        assert result.cue_count == 2

    def test_dense_long_dialogue_is_not_flagged_forced_narrative(self) -> None:
        """A dense, full-dialogue track is not classified as forced-narrative."""
        cues = parse_webvtt_cues(_DENSE_DIALOGUE_VTT)
        result = detect_forced_narrative_heuristic(cues, asset_duration_seconds=11.0)
        assert result.is_likely_forced_narrative is False

    def test_empty_cue_list_is_not_flagged(self) -> None:
        """No cues at all is not itself evidence of forced-narrative content."""
        result = detect_forced_narrative_heuristic([], asset_duration_seconds=60.0)
        assert result.is_likely_forced_narrative is False
        assert result.cue_count == 0

    def test_zero_duration_asset_does_not_raise(self) -> None:
        """A degenerate zero-duration asset is handled without a ZeroDivisionError."""
        cues = parse_webvtt_cues(_SPARSE_SHORT_CUES_VTT)
        result = detect_forced_narrative_heuristic(cues, asset_duration_seconds=0.0)
        assert result.is_likely_forced_narrative is False

    def test_sparse_but_long_cues_are_not_flagged(self) -> None:
        """Sparse cues that are individually long-duration fail the 'short' condition."""
        long_sparse_vtt = (
            "WEBVTT\n\n00:00:00.000 --> 00:00:10.000\n"
            "A long sustained caption spanning ten seconds.\n"
        )
        cues = parse_webvtt_cues(long_sparse_vtt)
        result = detect_forced_narrative_heuristic(cues, asset_duration_seconds=120.0)
        assert result.is_likely_forced_narrative is False


@pytest.mark.skipif(
    not ffmpeg_and_ffprobe_are_available(),
    reason="ffmpeg/ffprobe not available in this environment",
)
class TestSubtitleStreamDetectionAgainstRealFile:
    """Integration tests against the shared synthetic video fixture (no subtitles)."""

    def test_synthetic_fixture_reports_no_subtitle_streams(
        self, synthetic_test_video_path: str
    ) -> None:
        """The fixture video (video+audio only) reports zero subtitle streams."""
        streams = detect_subtitle_streams(synthetic_test_video_path)
        assert streams == []

    def test_nonexistent_file_raises_subtitle_extraction_error(self) -> None:
        """Probing a nonexistent file raises SubtitleExtractionError."""
        with pytest.raises(SubtitleExtractionError):
            detect_subtitle_streams("/no/such/file/anywhere.mov")
