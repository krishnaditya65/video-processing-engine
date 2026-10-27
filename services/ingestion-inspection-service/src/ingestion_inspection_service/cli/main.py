"""Command-line interface for running the inspection pipeline locally.

Usage::

    python -m ingestion_inspection_service.cli.main \\
        --asset-id my-asset-123 \\
        --source-file /path/to/mezzanine.mov \\
        --output-json /path/to/structural_map.json \\
        --scene-detection-strategy structural_similarity \\
        --analysis-fps 1.0

This is a thin wrapper around
:func:`ingestion_inspection_service.pipeline.pipeline.create_inspection_context_from_source_file`
and :class:`ingestion_inspection_service.pipeline.pipeline.InspectionPipeline`,
sharing the exact same code path as the FastAPI HTTP service so both
entry points are guaranteed to produce identical results for identical
inputs.

Beyond the core inspection run, this CLI exposes several optional,
purely additive flags for the newer analysis capabilities -- shot-type
classification, perceptual-hash printing, sprite-sheet/thumbnail
generation, subtitle extraction, and loudness remediation -- all
disabled unless explicitly requested, so the default invocation's
behavior/output is unchanged.
"""

from __future__ import annotations

import argparse
import sys
import typing

from ingestion_inspection_service.pipeline.pipeline import (
    build_default_inspection_pipeline,
    build_inspection_pipeline_with_shot_type_classification,
    create_inspection_context_from_source_file,
)
from ingestion_inspection_service.strategies.scene_detection import (
    HistogramDifferenceSceneDetectionStrategy,
    SceneDetectionStrategy,
    StructuralSimilaritySceneDetectionStrategy,
)

_SCENE_DETECTION_STRATEGY_REGISTRY: typing.Dict[
    str, typing.Callable[[], SceneDetectionStrategy]
] = {
    "histogram_difference": HistogramDifferenceSceneDetectionStrategy,
    "structural_similarity": StructuralSimilaritySceneDetectionStrategy,
}


def build_argument_parser() -> argparse.ArgumentParser:
    """Builds the CLI's ``argparse`` argument parser.

    Returns:
        argparse.ArgumentParser: Configured parser for all CLI flags.
    """
    parser = argparse.ArgumentParser(
        prog="ingestion-inspection-service",
        description=(
            "Deeply inspect a raw mezzanine video asset before "
            "transcoding: scene cuts, black frames, color space/HDR "
            "metadata, and an approximate loudness pass."
        ),
    )
    parser.add_argument(
        "--asset-id",
        required=True,
        help="Opaque identifier for the asset being inspected.",
    )
    parser.add_argument(
        "--source-file",
        required=True,
        help="Filesystem path to the mezzanine video file to analyze.",
    )
    parser.add_argument(
        "--output-json",
        default=None,
        help=(
            "Path to write the structural map JSON artifact to. If "
            "omitted, the JSON is printed to stdout."
        ),
    )
    parser.add_argument(
        "--scene-detection-strategy",
        choices=sorted(_SCENE_DETECTION_STRATEGY_REGISTRY.keys()),
        default="structural_similarity",
        help=(
            "Which pluggable scene-cut detection Strategy to use. "
            "Defaults to structural_similarity (SSIM); "
            "histogram_difference is faster but less discriminative."
        ),
    )
    parser.add_argument(
        "--analysis-fps",
        type=float,
        default=None,
        help=(
            "Analysis frame-sampling rate in frames-per-second of "
            "source-timeline duration. Defaults to the configured "
            "platform default (1.0 fps)."
        ),
    )
    parser.add_argument(
        "--skip-audio-loudness-analysis",
        action="store_true",
        help=(
            "Skip ffmpeg-based audio extraction and the loudness "
            "analysis stage entirely (useful when ffmpeg audio "
            "extraction is unavailable or unnecessary)."
        ),
    )
    parser.add_argument(
        "--enable-shot-type-classification",
        action="store_true",
        help=(
            "Additionally run the rule-based shot-type classification "
            "stage (close-up/wide/dialogue/action) after scene-cut "
            "detection. Off by default to preserve the default "
            "pipeline_stage_names_executed contract."
        ),
    )
    parser.add_argument(
        "--print-perceptual-hash",
        action="store_true",
        help=(
            "Print the asset's dHash (perceptual hash), computed from "
            "the already-sampled analysis frames, as a hex string."
        ),
    )
    parser.add_argument(
        "--sprite-sheet-output",
        default=None,
        help=(
            "Path to write a scene-thumbnail sprite-sheet PNG to "
            "(requires ffmpeg). One representative frame is extracted "
            "per detected scene."
        ),
    )
    parser.add_argument(
        "--sprite-vtt-output",
        default=None,
        help=(
            "Path to write the WebVTT sprite index to; only used "
            "together with --sprite-sheet-output."
        ),
    )
    parser.add_argument(
        "--extract-subtitles-dir",
        default=None,
        help=(
            "Directory to extract any embedded subtitle streams into "
            "as WebVTT files (requires ffmpeg/ffprobe). Silently does "
            "nothing if the asset has no subtitle streams."
        ),
    )
    parser.add_argument(
        "--normalize-loudness-output",
        default=None,
        help=(
            "Path to write a loudness-normalized (ffmpeg loudnorm) "
            "copy of the asset to. Before/after LUFS are printed."
        ),
    )
    parser.add_argument(
        "--target-lufs",
        type=float,
        default=None,
        help=(
            "Target integrated loudness (LUFS) for "
            "--normalize-loudness-output; defaults to the platform's "
            "configured streaming target when omitted."
        ),
    )
    return parser


def run_cli_inspection(argv: typing.Optional[typing.List[str]] = None) -> int:
    """Parses CLI arguments, runs the inspection pipeline, and prints output.

    Args:
        argv: Argument list to parse in place of ``sys.argv[1:]``;
            primarily used by tests to invoke the CLI in-process.

    Returns:
        int: Process exit code (0 on success, 1 on failure).
    """
    parser = build_argument_parser()
    parsed_args = parser.parse_args(argv)

    strategy_factory = _SCENE_DETECTION_STRATEGY_REGISTRY[
        parsed_args.scene_detection_strategy
    ]

    try:
        context = create_inspection_context_from_source_file(
            asset_id=parsed_args.asset_id,
            source_file_path=parsed_args.source_file,
            analysis_frames_per_second=parsed_args.analysis_fps,
            extract_audio_for_loudness_analysis=not parsed_args.skip_audio_loudness_analysis,
        )
        if parsed_args.enable_shot_type_classification:
            pipeline = build_inspection_pipeline_with_shot_type_classification(
                strategy_factory()
            )
        else:
            pipeline = build_default_inspection_pipeline(strategy_factory())
        pipeline.run(context)
        structural_map = pipeline.build_structural_map(context)
    except Exception as error:  # noqa: BLE001 - CLI top-level error boundary
        print(f"Inspection failed: {error}", file=sys.stderr)
        return 1

    output_json_text = structural_map.to_json_string()
    if parsed_args.output_json:
        with open(parsed_args.output_json, "w", encoding="utf-8") as output_file:
            output_file.write(output_json_text)
        print(f"Wrote structural map to {parsed_args.output_json}")
    else:
        print(output_json_text)

    _run_optional_post_inspection_actions(parsed_args, context, structural_map)

    return 0


def _run_optional_post_inspection_actions(
    parsed_args: argparse.Namespace, context, structural_map
) -> None:
    """Runs the CLI's optional, additive post-inspection actions.

    Every action here is best-effort: a failure (e.g. ffmpeg missing,
    no subtitle streams present) is reported to stderr but never turns
    a successful inspection run into a nonzero exit code, since these
    are all opt-in extras layered on top of the core, already-succeeded
    inspection pass.

    Args:
        parsed_args: The parsed CLI arguments.
        context: The fully-processed :class:`InspectionContext`.
        structural_map: The built :class:`StructuralMap`.
    """
    if parsed_args.print_perceptual_hash:
        _print_perceptual_hash(context)

    if parsed_args.sprite_sheet_output:
        _generate_sprite_sheet(parsed_args, structural_map)

    if parsed_args.extract_subtitles_dir:
        _extract_subtitles(parsed_args)

    if parsed_args.normalize_loudness_output:
        _normalize_loudness(parsed_args)


def _print_perceptual_hash(context) -> None:
    """Computes and prints the asset's dHash from already-sampled frames."""
    try:
        from ingestion_inspection_service.hashing.perceptual_hash import (
            compute_asset_representative_perceptual_hash,
        )

        if not context.sampled_frames:
            print("perceptual_hash: unavailable (no sampled frames)")
            return
        perceptual_hash = compute_asset_representative_perceptual_hash(
            [frame.frame_bgr for frame in context.sampled_frames]
        )
        print(f"perceptual_hash: {perceptual_hash:016x}")
    except Exception as error:  # noqa: BLE001 - best-effort CLI extra
        print(f"perceptual_hash: failed ({error})", file=sys.stderr)


def _generate_sprite_sheet(parsed_args: argparse.Namespace, structural_map) -> None:
    """Extracts per-scene thumbnails and writes a sprite sheet + VTT index."""
    try:
        import cv2

        from ingestion_inspection_service.adapters.thumbnail_sprite_generation import (
            build_webvtt_sprite_index,
            composite_sprite_sheet_from_scene_frames,
            extract_representative_frame_via_ffmpeg,
        )

        if not structural_map.scene_boundaries:
            print("sprite_sheet: skipped (no scenes detected)", file=sys.stderr)
            return

        scene_frames = [
            extract_representative_frame_via_ffmpeg(
                parsed_args.source_file, scene.start_timestamp_seconds
            )
            for scene in structural_map.scene_boundaries
        ]
        result = composite_sprite_sheet_from_scene_frames(
            scene_frames,
            [
                scene.start_timestamp_seconds
                for scene in structural_map.scene_boundaries
            ],
            [
                scene.end_timestamp_seconds
                for scene in structural_map.scene_boundaries
            ],
        )
        cv2.imwrite(parsed_args.sprite_sheet_output, result.sprite_sheet_image_bgr)
        print(f"Wrote sprite sheet to {parsed_args.sprite_sheet_output}")

        if parsed_args.sprite_vtt_output:
            import os

            vtt_text = build_webvtt_sprite_index(
                result.thumbnail_rectangles,
                os.path.basename(parsed_args.sprite_sheet_output),
            )
            with open(parsed_args.sprite_vtt_output, "w", encoding="utf-8") as vtt_file:
                vtt_file.write(vtt_text)
            print(f"Wrote sprite VTT index to {parsed_args.sprite_vtt_output}")
    except Exception as error:  # noqa: BLE001 - best-effort CLI extra
        print(f"sprite_sheet: failed ({error})", file=sys.stderr)


def _extract_subtitles(parsed_args: argparse.Namespace) -> None:
    """Detects and extracts embedded subtitle streams to WebVTT files."""
    try:
        import os

        from ingestion_inspection_service.adapters.subtitle_extraction import (
            detect_forced_narrative_heuristic,
            detect_subtitle_streams,
            extract_subtitle_stream_to_webvtt,
            parse_webvtt_cues,
        )

        streams = detect_subtitle_streams(parsed_args.source_file)
        if not streams:
            print("subtitles: none detected")
            return

        os.makedirs(parsed_args.extract_subtitles_dir, exist_ok=True)
        for stream in streams:
            output_path = os.path.join(
                parsed_args.extract_subtitles_dir,
                f"stream_{stream.ffmpeg_stream_index}.vtt",
            )
            extract_subtitle_stream_to_webvtt(
                parsed_args.source_file, stream.ffmpeg_stream_index, output_path
            )
            with open(output_path, "r", encoding="utf-8") as vtt_file:
                cues = parse_webvtt_cues(vtt_file.read())
            print(
                f"Wrote subtitle stream {stream.ffmpeg_stream_index} "
                f"({stream.language_tag or 'unknown language'}) to {output_path} "
                f"({len(cues)} cues)"
            )
    except Exception as error:  # noqa: BLE001 - best-effort CLI extra
        print(f"subtitles: failed ({error})", file=sys.stderr)


def _normalize_loudness(parsed_args: argparse.Namespace) -> None:
    """Runs ffmpeg loudnorm remediation and prints before/after LUFS."""
    try:
        from ingestion_inspection_service.adapters.loudness_remediation import (
            apply_loudness_normalization_via_ffmpeg,
        )
        from ingestion_inspection_service.config import (
            DEFAULT_LOUDNESS_ANALYSIS_CONFIGURATION,
        )

        target_lufs = (
            parsed_args.target_lufs
            if parsed_args.target_lufs is not None
            else DEFAULT_LOUDNESS_ANALYSIS_CONFIGURATION.streaming_platform_target_lufs
        )
        result = apply_loudness_normalization_via_ffmpeg(
            parsed_args.source_file,
            parsed_args.normalize_loudness_output,
            target_lufs=target_lufs,
        )
        print(
            "loudness_remediation: "
            f"before={result.loudness_before.integrated_loudness_lufs:.2f} LUFS, "
            f"after={result.loudness_after.integrated_loudness_lufs:.2f} LUFS, "
            f"target={result.target_lufs:.2f} LUFS, "
            f"output={result.output_file_path}"
        )
    except Exception as error:  # noqa: BLE001 - best-effort CLI extra
        print(f"loudness_remediation: failed ({error})", file=sys.stderr)


def main() -> None:
    """Console-script entry point; exits the process with the run's exit code."""
    sys.exit(run_cli_inspection())


if __name__ == "__main__":
    main()
