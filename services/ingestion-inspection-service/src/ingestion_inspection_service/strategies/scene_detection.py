"""Pluggable scene-cut detection algorithms (Strategy design pattern).

Two concrete strategies are provided:

1. :class:`HistogramDifferenceSceneDetectionStrategy` -- fast, coarse.
   Computes a normalized luma histogram per sampled frame and measures
   the Pearson-correlation-based distance (``cv2.HISTCMP_CORREL``)
   between consecutive frames' histograms. Cheap (O(width*height) per
   frame to build the histogram, O(bins) to compare) and robust to
   small camera/object motion, but can miss cuts between two shots that
   happen to share a similar overall luma distribution (e.g. two
   different indoor scenes with similar lighting).

2. :class:`StructuralSimilaritySceneDetectionStrategy` -- slower, more
   discriminative. Implements the Structural Similarity Index (SSIM),
   as introduced by Wang et al., "Image Quality Assessment: From Error
   Visibility to Structural Similarity" (IEEE TIP, 2004), computed from
   first principles with NumPy/OpenCV (no ``scikit-image`` dependency).
   SSIM compares local luminance, contrast, and structure and is
   markedly better than raw pixel or histogram difference at
   distinguishing "same shot, camera moved" from "genuinely different
   shot", at the cost of a per-pixel windowed computation
   (O(width*height) per frame with a constant-size Gaussian window, in
   practice several times slower than the histogram approach due to the
   convolution constant factor).

Both strategies expose a uniform interface
(:class:`SceneDetectionStrategy`) so
``pipeline.stages.SceneCutDetectionStage`` can drive either one -- or a
future third algorithm -- without any conditional logic of its own.
This is the textbook **Strategy** design pattern: the algorithm is
selected and injected by the caller rather than hard-coded into the
consumer.

The module also provides
:func:`detect_scene_boundaries_via_adaptive_thresholding`, which turns a
raw sequence of per-frame-pair dissimilarity scores into a final,
merged, confidence-scored list of :class:`SceneBoundary` objects using
an adaptive (rolling mean/std) threshold rather than one single global
cutoff, so the detector self-calibrates to each asset's own baseline
level of frame-to-frame motion/noise.
"""

from __future__ import annotations

import abc
import typing

import cv2
import numpy as np

from ingestion_inspection_service.config import (
    DEFAULT_SCENE_CUT_DETECTION_THRESHOLDS,
    SceneCutDetectionThresholds,
)
from ingestion_inspection_service.domain.models import SceneBoundary


class SceneDetectionStrategy(abc.ABC):
    """Abstract Strategy interface for pluggable scene-cut algorithms.

    Concrete strategies must be able to (a) reduce a pair of consecutive
    sampled frames to a single scalar "dissimilarity" score in the
    closed interval ``[0.0, 1.0]`` (0 = identical, 1 = maximally
    different), and (b) advertise the fixed dissimilarity threshold
    above which a lone frame-pair delta is, on its own and without
    adaptive context, considered a plausible hard cut.
    """

    @abc.abstractmethod
    def compute_frame_pair_dissimilarity_score(
        self, previous_frame_bgr: np.ndarray, current_frame_bgr: np.ndarray
    ) -> float:
        """Computes a [0, 1] dissimilarity score between two BGR frames.

        Args:
            previous_frame_bgr: The earlier of the two consecutive
                sampled frames, as an ``(H, W, 3)`` ``uint8`` NumPy
                array in OpenCV's default BGR channel order.
            current_frame_bgr: The later of the two consecutive sampled
                frames, same shape/dtype as ``previous_frame_bgr``.

        Returns:
            float: 0.0 means the frames are (algorithm-specific)
            identical; 1.0 means maximally dissimilar.

        Raises:
            ValueError: If the two frames do not share the same shape.
        """
        raise NotImplementedError

    @property
    @abc.abstractmethod
    def fixed_cut_dissimilarity_threshold(self) -> float:
        """Returns the strategy's fixed (non-adaptive) cut threshold."""
        raise NotImplementedError

    @property
    @abc.abstractmethod
    def strategy_name(self) -> str:
        """Returns a short, stable, machine-readable strategy identifier."""
        raise NotImplementedError

    @staticmethod
    def _assert_matching_frame_shapes(
        previous_frame_bgr: np.ndarray, current_frame_bgr: np.ndarray
    ) -> None:
        """Validates that two frames share identical array shapes.

        Args:
            previous_frame_bgr: First frame array.
            current_frame_bgr: Second frame array.

        Raises:
            ValueError: If ``previous_frame_bgr.shape`` does not equal
                ``current_frame_bgr.shape``.
        """
        if previous_frame_bgr.shape != current_frame_bgr.shape:
            raise ValueError(
                "Cannot compare frames of differing shape: "
                f"{previous_frame_bgr.shape} vs {current_frame_bgr.shape}. "
                "Frame source must decode at a consistent resolution."
            )


class HistogramDifferenceSceneDetectionStrategy(SceneDetectionStrategy):
    """Scene-cut detection via consecutive-frame luma histogram distance.

    Algorithmic notes:
        For each frame, converts BGR -> grayscale luma and builds a
        256-bin normalized histogram of pixel intensities via
        ``cv2.calcHist``. The Pearson-correlation-style similarity
        between the previous and current frame's histograms is computed
        with ``cv2.compareHist(..., cv2.HISTCMP_CORREL)``, which returns
        a value in ``[-1, 1]`` (1.0 = identical distribution). We map
        this to a dissimilarity score via ``max(0.0, (1 - correlation) / 2)``
        so the resulting range is ``[0, 1]`` with 0 meaning identical.

        Complexity: building each histogram is O(H*W); comparing two
        256-bin histograms is O(1) relative to frame size. Overall
        O(H*W) per frame pair, with a very small constant factor,
        making this the cheaper of the two strategies.
    """

    def __init__(self, histogram_bin_count: int = 256) -> None:
        """Initializes the strategy.

        Args:
            histogram_bin_count: Number of luma histogram bins to use.
                Defaults to 256 (one bin per possible 8-bit intensity
                value), which maximizes sensitivity at the cost of a
                slightly noisier histogram for low-detail frames.
        """
        self._histogram_bin_count = histogram_bin_count

    def compute_frame_pair_dissimilarity_score(
        self, previous_frame_bgr: np.ndarray, current_frame_bgr: np.ndarray
    ) -> float:
        """Computes histogram-correlation-based dissimilarity between frames.

        Args:
            previous_frame_bgr: Earlier sampled frame, BGR ``uint8``.
            current_frame_bgr: Later sampled frame, BGR ``uint8``.

        Returns:
            float: Dissimilarity in ``[0, 1]``; 0 means the two frames'
            luma histograms are perfectly correlated.

        Raises:
            ValueError: If frame shapes differ.
        """
        self._assert_matching_frame_shapes(previous_frame_bgr, current_frame_bgr)

        previous_grayscale = cv2.cvtColor(previous_frame_bgr, cv2.COLOR_BGR2GRAY)
        current_grayscale = cv2.cvtColor(current_frame_bgr, cv2.COLOR_BGR2GRAY)

        previous_histogram = cv2.calcHist(
            [previous_grayscale], [0], None, [self._histogram_bin_count], [0, 256]
        )
        current_histogram = cv2.calcHist(
            [current_grayscale], [0], None, [self._histogram_bin_count], [0, 256]
        )
        cv2.normalize(previous_histogram, previous_histogram)
        cv2.normalize(current_histogram, current_histogram)

        correlation = float(
            cv2.compareHist(previous_histogram, current_histogram, cv2.HISTCMP_CORREL)
        )
        dissimilarity_score = max(0.0, min(1.0, (1.0 - correlation) / 2.0))
        return dissimilarity_score

    @property
    def fixed_cut_dissimilarity_threshold(self) -> float:
        """Returns the fixed histogram-correlation cut threshold.

        Returns:
            float: ``1 - histogram_correlation_cut_threshold`` mapped
            into dissimilarity space via the same transform used in
            :meth:`compute_frame_pair_dissimilarity_score`.
        """
        correlation_threshold = (
            DEFAULT_SCENE_CUT_DETECTION_THRESHOLDS.histogram_correlation_cut_threshold
        )
        return max(0.0, min(1.0, (1.0 - correlation_threshold) / 2.0))

    @property
    def strategy_name(self) -> str:
        """Returns the stable identifier ``"histogram_difference"``."""
        return "histogram_difference"


class StructuralSimilaritySceneDetectionStrategy(SceneDetectionStrategy):
    """Scene-cut detection via the Structural Similarity Index (SSIM).

    Algorithmic notes:
        Implements SSIM from first principles (Wang et al., 2004),
        without relying on ``scikit-image``. For two grayscale, float64
        images ``x`` and ``y``:

            SSIM(x, y) = ((2*mu_x*mu_y + C1) * (2*sigma_xy + C2)) /
                         ((mu_x^2 + mu_y^2 + C1) * (sigma_x^2 + sigma_y^2 + C2))

        where ``mu``, ``sigma^2`` and ``sigma_xy`` are the *local*
        (windowed) mean, variance, and covariance, estimated here with
        an 11x11 Gaussian window (``sigma=1.5``) via
        ``cv2.GaussianBlur`` -- the same windowing choice as the
        original paper's reference implementation. ``C1 = (K1*L)^2``
        and ``C2 = (K2*L)^2`` with the paper's default stabilization
        constants ``K1=0.01``, ``K2=0.03`` and dynamic range ``L=255``
        for 8-bit imagery, which keep the denominator away from zero in
        flat (zero-variance) image regions.

        The scalar SSIM index reported is the mean of the per-pixel
        SSIM map. Dissimilarity is then ``1 - mean_ssim``, clamped to
        ``[0, 1]``.

        Complexity: each Gaussian blur pass is O(H*W) with a constant
        window size; we perform 5 blur passes (mu_x, mu_y, and the
        three second-moment terms), so overall this strategy costs a
        small constant multiple of the histogram strategy's O(H*W) --
        typically several times slower in wall-clock terms due to the
        larger constant factor, which is why it is offered as an
        alternative rather than the sole default.
    """

    _GAUSSIAN_WINDOW_SIZE_PIXELS = 11
    _GAUSSIAN_WINDOW_SIGMA = 1.5
    _STABILIZATION_CONSTANT_K1 = 0.01
    _STABILIZATION_CONSTANT_K2 = 0.03
    _EIGHT_BIT_DYNAMIC_RANGE = 255.0

    def compute_frame_pair_dissimilarity_score(
        self, previous_frame_bgr: np.ndarray, current_frame_bgr: np.ndarray
    ) -> float:
        """Computes ``1 - mean_SSIM`` between two BGR frames.

        Args:
            previous_frame_bgr: Earlier sampled frame, BGR ``uint8``.
            current_frame_bgr: Later sampled frame, BGR ``uint8``.

        Returns:
            float: Dissimilarity in ``[0, 1]``; 0 means the frames are
            structurally identical under the SSIM model.

        Raises:
            ValueError: If frame shapes differ.
        """
        self._assert_matching_frame_shapes(previous_frame_bgr, current_frame_bgr)

        previous_grayscale = cv2.cvtColor(
            previous_frame_bgr, cv2.COLOR_BGR2GRAY
        ).astype(np.float64)
        current_grayscale = cv2.cvtColor(
            current_frame_bgr, cv2.COLOR_BGR2GRAY
        ).astype(np.float64)

        mean_ssim = self._compute_mean_structural_similarity_index(
            previous_grayscale, current_grayscale
        )
        dissimilarity_score = max(0.0, min(1.0, 1.0 - mean_ssim))
        return dissimilarity_score

    def _compute_mean_structural_similarity_index(
        self, image_x: np.ndarray, image_y: np.ndarray
    ) -> float:
        """Computes the mean SSIM index between two grayscale float images.

        Args:
            image_x: First grayscale image as ``float64`` array, shape
                ``(H, W)``.
            image_y: Second grayscale image, same shape as ``image_x``.

        Returns:
            float: The mean of the per-pixel SSIM map, in
            (approximately) ``[-1, 1]`` with 1.0 indicating identical
            local structure everywhere.
        """
        window_size = self._GAUSSIAN_WINDOW_SIZE_PIXELS
        sigma = self._GAUSSIAN_WINDOW_SIGMA

        def gaussian_blur(image: np.ndarray) -> np.ndarray:
            return cv2.GaussianBlur(image, (window_size, window_size), sigma)

        mu_x = gaussian_blur(image_x)
        mu_y = gaussian_blur(image_y)

        mu_x_squared = mu_x * mu_x
        mu_y_squared = mu_y * mu_y
        mu_x_mu_y = mu_x * mu_y

        sigma_x_squared = gaussian_blur(image_x * image_x) - mu_x_squared
        sigma_y_squared = gaussian_blur(image_y * image_y) - mu_y_squared
        sigma_xy = gaussian_blur(image_x * image_y) - mu_x_mu_y

        stabilization_c1 = (
            self._STABILIZATION_CONSTANT_K1 * self._EIGHT_BIT_DYNAMIC_RANGE
        ) ** 2
        stabilization_c2 = (
            self._STABILIZATION_CONSTANT_K2 * self._EIGHT_BIT_DYNAMIC_RANGE
        ) ** 2

        numerator = (2 * mu_x_mu_y + stabilization_c1) * (
            2 * sigma_xy + stabilization_c2
        )
        denominator = (mu_x_squared + mu_y_squared + stabilization_c1) * (
            sigma_x_squared + sigma_y_squared + stabilization_c2
        )
        ssim_map = numerator / denominator
        return float(np.mean(ssim_map))

    @property
    def fixed_cut_dissimilarity_threshold(self) -> float:
        """Returns ``1 - structural_similarity_cut_threshold``.

        Returns:
            float: The configured SSIM cut threshold, mapped into
            dissimilarity space.
        """
        ssim_threshold = (
            DEFAULT_SCENE_CUT_DETECTION_THRESHOLDS.structural_similarity_cut_threshold
        )
        return max(0.0, min(1.0, 1.0 - ssim_threshold))

    @property
    def strategy_name(self) -> str:
        """Returns the stable identifier ``"structural_similarity"``."""
        return "structural_similarity"


def compute_motion_complexity_score(frame_bgr: np.ndarray) -> float:
    """Computes a heuristic motion/visual-complexity score for one frame.

    Combines two signals into a single non-negative scalar used to
    approximate how "encoding-expensive" a scene is likely to be:

      1. Edge density -- fraction of pixels flagged by a Canny edge
         detector, as a proxy for spatial detail (more edges => more
         high-frequency content => generally harder to compress well
         at a given bitrate).
      2. Local intensity variance -- the frame's grayscale pixel
         variance, as a coarse proxy for overall contrast/texture.

    Args:
        frame_bgr: A single decoded frame, ``(H, W, 3)`` BGR ``uint8``.

    Returns:
        float: A non-negative complexity score with no fixed upper
        bound; only meaningful in relative (scene-to-scene) comparison
        within the same asset, not as an absolute/calibrated metric.

    Raises:
        ValueError: If ``frame_bgr`` is not a 3-channel image array.

    Algorithmic notes:
        O(H*W): one grayscale conversion, one Canny edge pass (itself
        O(H*W) via Sobel gradients + non-max suppression + hysteresis),
        and one variance reduction.
    """
    if frame_bgr.ndim != 3 or frame_bgr.shape[2] != 3:
        raise ValueError(
            f"compute_motion_complexity_score expects a (H, W, 3) BGR frame, "
            f"got shape {frame_bgr.shape}."
        )

    grayscale_frame = cv2.cvtColor(frame_bgr, cv2.COLOR_BGR2GRAY)
    edge_map = cv2.Canny(grayscale_frame, threshold1=80, threshold2=160)
    edge_density_ratio = float(np.count_nonzero(edge_map)) / float(edge_map.size)
    intensity_variance = float(np.var(grayscale_frame.astype(np.float64)))

    # Normalize variance (0..~16256 max for 8-bit) into a comparable-ish
    # [0, 1]-ish range before blending with edge density so neither term
    # dominates purely due to unit scale.
    normalized_intensity_variance = min(1.0, intensity_variance / 4096.0)
    motion_complexity_score = (
        0.6 * edge_density_ratio + 0.4 * normalized_intensity_variance
    )
    return motion_complexity_score


def detect_scene_boundaries_via_adaptive_thresholding(
    frame_pair_dissimilarity_scores: typing.Sequence[float],
    frame_timestamps_seconds: typing.Sequence[float],
    per_frame_motion_complexity_scores: typing.Sequence[float],
    detection_strategy_name: str,
    fixed_cut_dissimilarity_threshold: float,
    asset_duration_seconds: float,
    thresholds: SceneCutDetectionThresholds = DEFAULT_SCENE_CUT_DETECTION_THRESHOLDS,
) -> typing.List[SceneBoundary]:
    """Turns per-frame-pair dissimilarity scores into merged scene boundaries.

    This function implements the "adaptive threshold strategy" required
    on top of whichever :class:`SceneDetectionStrategy` produced the raw
    dissimilarity scores. Rather than a single global cutoff, a cut
    candidate at sample index ``i`` is only accepted when its
    dissimilarity score exceeds *both*:

      * the strategy's fixed threshold (a sanity floor), and
      * ``rolling_mean + sensitivity_multiplier * rolling_std`` computed
        over the preceding ``adaptive_threshold_window_size`` samples
        (so the detector self-calibrates to each asset's own baseline
        noise level -- a grainy film-grain-heavy asset with generally
        higher frame-to-frame delta will not trigger false cuts on
        every frame the way a fixed global threshold would).

    Candidate cuts closer together than
    ``thresholds.minimum_scene_duration_seconds`` are merged (the later
    candidate is dropped) to suppress flicker/false positives from
    strobing content or single noisy samples.

    Args:
        frame_pair_dissimilarity_scores: Dissimilarity score for each
            consecutive sampled-frame pair, i.e. element ``i``
            corresponds to the transition from sampled frame ``i`` to
            sampled frame ``i + 1``. Length must be
            ``len(frame_timestamps_seconds) - 1``.
        frame_timestamps_seconds: Presentation timestamp, in seconds,
            of each sampled frame, strictly increasing.
        per_frame_motion_complexity_scores: Per-sampled-frame motion
            complexity score (see
            :func:`compute_motion_complexity_score`), same length as
            ``frame_timestamps_seconds``, used to compute each
            resulting scene's average complexity.
        detection_strategy_name: Name recorded on each produced
            :class:`SceneBoundary` for auditability.
        fixed_cut_dissimilarity_threshold: The strategy's non-adaptive
            floor threshold; see class docs above.
        asset_duration_seconds: Total asset duration, used as the final
            scene's end timestamp.
        thresholds: Tunable adaptive-threshold parameters.

    Returns:
        list[SceneBoundary]: Ordered, gap-free, non-overlapping scene
        boundaries covering ``[0, asset_duration_seconds)``. Always
        contains at least one scene (the whole asset, if no cuts are
        detected).

    Raises:
        ValueError: If input sequence lengths are inconsistent, or if
            ``asset_duration_seconds`` is not positive.

    Algorithmic notes:
        Single linear pass, O(n) in the number of sampled frames, plus
        an O(window_size) rolling statistics update per sample (using a
        simple deque-backed window rather than a full recompute), so
        overall O(n * window_size) worst case with a small constant
        window (default 15).
    """
    frame_count = len(frame_timestamps_seconds)
    if len(frame_pair_dissimilarity_scores) != max(0, frame_count - 1):
        raise ValueError(
            "frame_pair_dissimilarity_scores must have exactly "
            "len(frame_timestamps_seconds) - 1 elements; got "
            f"{len(frame_pair_dissimilarity_scores)} scores for "
            f"{frame_count} timestamps."
        )
    if len(per_frame_motion_complexity_scores) != frame_count:
        raise ValueError(
            "per_frame_motion_complexity_scores must have the same length "
            "as frame_timestamps_seconds."
        )
    if asset_duration_seconds <= 0:
        raise ValueError(
            f"asset_duration_seconds must be positive, got {asset_duration_seconds}."
        )

    candidate_cut_sample_indices = _identify_candidate_cut_sample_indices(
        frame_pair_dissimilarity_scores, fixed_cut_dissimilarity_threshold, thresholds
    )
    merged_cut_timestamps = _merge_nearby_cut_candidates(
        candidate_cut_sample_indices,
        frame_timestamps_seconds,
        thresholds.minimum_scene_duration_seconds,
    )

    boundary_timestamps = [0.0] + merged_cut_timestamps + [asset_duration_seconds]
    scene_boundaries: typing.List[SceneBoundary] = []
    for scene_index in range(len(boundary_timestamps) - 1):
        scene_start = boundary_timestamps[scene_index]
        scene_end = boundary_timestamps[scene_index + 1]
        if scene_end <= scene_start:
            continue

        average_complexity = _average_motion_complexity_within_range(
            frame_timestamps_seconds,
            per_frame_motion_complexity_scores,
            scene_start,
            scene_end,
        )
        confidence_score = _estimate_cut_confidence_score(
            scene_index,
            frame_pair_dissimilarity_scores,
            frame_timestamps_seconds,
            scene_start,
            fixed_cut_dissimilarity_threshold,
        )

        scene_boundaries.append(
            SceneBoundary(
                scene_index=len(scene_boundaries),
                start_timestamp_seconds=scene_start,
                end_timestamp_seconds=scene_end,
                detection_strategy_name=detection_strategy_name,
                cut_confidence_score=confidence_score,
                motion_complexity_score=average_complexity,
            )
        )

    return scene_boundaries


def _identify_candidate_cut_sample_indices(
    dissimilarity_scores: typing.Sequence[float],
    fixed_threshold: float,
    thresholds: SceneCutDetectionThresholds,
) -> typing.List[int]:
    """Identifies sample indices whose dissimilarity exceeds both thresholds.

    Args:
        dissimilarity_scores: Per-frame-pair dissimilarity scores.
        fixed_threshold: Strategy-specific fixed floor threshold.
        thresholds: Adaptive-threshold configuration (window size and
            sensitivity multiplier).

    Returns:
        list[int]: Indices ``i`` into ``dissimilarity_scores`` (i.e.
        the transition from sampled frame ``i`` to ``i + 1``) flagged
        as candidate cuts.
    """
    window_size = thresholds.adaptive_threshold_window_size
    sensitivity = thresholds.adaptive_threshold_sensitivity_multiplier

    candidate_indices: typing.List[int] = []
    for index, score in enumerate(dissimilarity_scores):
        window_start = max(0, index - window_size)
        preceding_window = dissimilarity_scores[window_start:index]

        if len(preceding_window) >= 3:
            rolling_mean = float(np.mean(preceding_window))
            rolling_std = float(np.std(preceding_window))
            adaptive_threshold = rolling_mean + sensitivity * rolling_std
        else:
            # Not enough history yet: fall back to the fixed threshold
            # alone so the very start of the asset can still register
            # obvious hard cuts.
            adaptive_threshold = fixed_threshold

        effective_threshold = max(fixed_threshold, adaptive_threshold)
        if score >= effective_threshold and score >= fixed_threshold:
            candidate_indices.append(index)

    return candidate_indices


def _merge_nearby_cut_candidates(
    candidate_sample_indices: typing.Sequence[int],
    frame_timestamps_seconds: typing.Sequence[float],
    minimum_scene_duration_seconds: float,
) -> typing.List[float]:
    """Merges candidate cuts closer together than the minimum scene duration.

    Args:
        candidate_sample_indices: Indices into
            ``frame_timestamps_seconds`` flagged as candidate cuts
            (the cut is placed at ``frame_timestamps_seconds[index + 1]``,
            i.e. the start of the *new* scene).
        frame_timestamps_seconds: Timestamp of each sampled frame.
        minimum_scene_duration_seconds: Minimum allowed gap between two
            accepted cut timestamps.

    Returns:
        list[float]: Ordered, de-duplicated, minimum-spaced cut
        timestamps (excluding the implicit 0.0 and final-duration
        boundaries, which the caller adds separately).
    """
    merged_timestamps: typing.List[float] = []
    last_accepted_timestamp: typing.Optional[float] = None

    for index in candidate_sample_indices:
        cut_timestamp = frame_timestamps_seconds[index + 1]
        if (
            last_accepted_timestamp is not None
            and (cut_timestamp - last_accepted_timestamp)
            < minimum_scene_duration_seconds
        ):
            continue
        merged_timestamps.append(cut_timestamp)
        last_accepted_timestamp = cut_timestamp

    return merged_timestamps


def _average_motion_complexity_within_range(
    frame_timestamps_seconds: typing.Sequence[float],
    per_frame_motion_complexity_scores: typing.Sequence[float],
    range_start_seconds: float,
    range_end_seconds: float,
) -> float:
    """Averages motion complexity scores for samples within a time range.

    Args:
        frame_timestamps_seconds: Timestamp of each sampled frame.
        per_frame_motion_complexity_scores: Per-sample complexity score.
        range_start_seconds: Inclusive start of the range.
        range_end_seconds: Exclusive end of the range.

    Returns:
        float: Mean complexity score of samples in
        ``[range_start_seconds, range_end_seconds)``, or 0.0 if no
        samples fall in that range (degenerate/very short scene).
    """
    scores_in_range = [
        score
        for timestamp, score in zip(
            frame_timestamps_seconds, per_frame_motion_complexity_scores
        )
        if range_start_seconds <= timestamp < range_end_seconds
    ]
    if not scores_in_range:
        return 0.0
    return float(np.mean(scores_in_range))


def _estimate_cut_confidence_score(
    scene_index: int,
    dissimilarity_scores: typing.Sequence[float],
    frame_timestamps_seconds: typing.Sequence[float],
    scene_start_seconds: float,
    fixed_threshold: float,
) -> float:
    """Estimates a [0, 1] confidence that a scene's opening cut is genuine.

    Args:
        scene_index: Index of the scene whose opening-cut confidence is
            being estimated; scene 0 (the asset's very first scene) has
            no preceding cut and is assigned full confidence by
            definition.
        dissimilarity_scores: Per-frame-pair dissimilarity scores.
        frame_timestamps_seconds: Timestamp of each sampled frame.
        scene_start_seconds: This scene's start timestamp.
        fixed_threshold: The strategy's fixed floor threshold, used as
            the normalization denominator.

    Returns:
        float: Confidence in ``[0, 1]``, higher when the triggering
        dissimilarity score exceeded the fixed threshold by a wider
        margin.
    """
    if scene_index == 0:
        return 1.0

    try:
        matching_index = frame_timestamps_seconds.index(scene_start_seconds)  # type: ignore[attr-defined]
    except (AttributeError, ValueError):
        matching_index = None
        for candidate_index, timestamp in enumerate(frame_timestamps_seconds):
            if timestamp == scene_start_seconds:
                matching_index = candidate_index
                break

    if matching_index is None or matching_index == 0:
        return 0.5

    triggering_score = dissimilarity_scores[matching_index - 1]
    if fixed_threshold <= 0:
        return 1.0
    margin_ratio = (triggering_score - fixed_threshold) / max(fixed_threshold, 1e-6)
    confidence = 0.5 + 0.5 * max(0.0, min(1.0, margin_ratio))
    return max(0.0, min(1.0, confidence))
