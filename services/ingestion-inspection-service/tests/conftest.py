"""Shared pytest fixtures: synthetic frame generators for all test modules.

No real video files or the ffprobe/ffmpeg binaries are required to run
the tests in this suite's core logic modules -- every fixture here
builds small synthetic NumPy frame arrays directly, exercising the
actual scene-cut, black-frame, and pipeline-composition code paths
against known, hand-constructed inputs.
"""

from __future__ import annotations

import shutil
import subprocess

import numpy as np
import pytest


def make_solid_color_frame(
    height: int, width: int, bgr_color: tuple[int, int, int]
) -> np.ndarray:
    """Builds a solid-color synthetic BGR frame.

    Args:
        height: Frame height in pixels.
        width: Frame width in pixels.
        bgr_color: ``(blue, green, red)`` channel values, 0-255 each.

    Returns:
        numpy.ndarray: A ``(height, width, 3)`` ``uint8`` array filled
        with the given color.
    """
    frame = np.zeros((height, width, 3), dtype=np.uint8)
    frame[:, :, 0] = bgr_color[0]
    frame[:, :, 1] = bgr_color[1]
    frame[:, :, 2] = bgr_color[2]
    return frame


def make_checkerboard_frame(
    height: int, width: int, square_size: int, seed: int
) -> np.ndarray:
    """Builds a pseudo-random checkerboard-pattern synthetic BGR frame.

    Args:
        height: Frame height in pixels.
        width: Frame width in pixels.
        square_size: Side length, in pixels, of each checkerboard cell.
        seed: RNG seed controlling the specific pattern colors,
            allowing generation of visually distinct frames for
            "different shot" test scenarios.

    Returns:
        numpy.ndarray: A ``(height, width, 3)`` ``uint8`` frame with a
        colorful, high-detail synthetic pattern (useful for exercising
        edge-density / motion-complexity scoring, which needs
        structure, unlike a flat solid-color frame).
    """
    rng = np.random.default_rng(seed)
    frame = np.zeros((height, width, 3), dtype=np.uint8)
    for row_start in range(0, height, square_size):
        for col_start in range(0, width, square_size):
            color = rng.integers(0, 256, size=3, dtype=np.uint8)
            frame[
                row_start : row_start + square_size,
                col_start : col_start + square_size,
            ] = color
    return frame


@pytest.fixture
def frame_dimensions() -> tuple[int, int]:
    """Returns a small, fast-to-process synthetic frame size (height, width)."""
    return 64, 64


def ffmpeg_and_ffprobe_are_available() -> bool:
    """Checks whether both the ffmpeg and ffprobe binaries are on PATH.

    Returns:
        bool: True if both binaries are resolvable via ``shutil.which``.
    """
    return shutil.which("ffmpeg") is not None and shutil.which("ffprobe") is not None


@pytest.fixture(scope="session")
def synthetic_test_video_path(tmp_path_factory: pytest.TempPathFactory):
    """Generates a tiny real MP4 file with a genuine scene cut, via ffmpeg.

    Builds a 2-second, 64x64, 5fps H.264 clip: 1 second of solid red
    followed by 1 second of solid blue (a genuine, unambiguous scene
    cut at t=1.0s), with a 2-second 440Hz sine-wave AAC audio track, so
    tests can exercise the real ffprobe adapter, OpenCV frame source,
    and end-to-end CLI/API paths against an actual decodable file
    rather than only synthetic in-memory arrays.

    Skips (returns None and the calling test should skip) when
    ffmpeg/ffprobe are not available in this environment.

    Args:
        tmp_path_factory: Built-in pytest fixture for session-scoped
            temporary directories.

    Returns:
        str or None: Filesystem path to the generated MP4, or None if
        ffmpeg/ffprobe are unavailable.
    """
    if not ffmpeg_and_ffprobe_are_available():
        return None

    output_dir = tmp_path_factory.mktemp("synthetic_video")
    output_path = output_dir / "synthetic_scene_cut_sample.mp4"

    command = [
        "ffmpeg",
        "-y",
        "-f",
        "lavfi",
        "-i",
        "color=c=red:s=64x64:d=1:r=5",
        "-f",
        "lavfi",
        "-i",
        "color=c=blue:s=64x64:d=1:r=5",
        "-f",
        "lavfi",
        "-i",
        "sine=frequency=440:duration=2",
        "-filter_complex",
        "[0:v][1:v]concat=n=2:v=1:a=0[v]",
        "-map",
        "[v]",
        "-map",
        "2:a",
        "-c:v",
        "libx264",
        "-pix_fmt",
        "yuv420p",
        "-c:a",
        "aac",
        "-shortest",
        str(output_path),
    ]
    subprocess.run(command, capture_output=True, check=True, timeout=60)
    return str(output_path)
