"""Pluggable, rule-based shot-type classification (Strategy design pattern).

Classifies each detected :class:`~ingestion_inspection_service.domain.models.SceneBoundary`
into a coarse cinematographic category --
:class:`~ingestion_inspection_service.domain.models.ShotType` CLOSE_UP,
WIDE, DIALOGUE, or ACTION -- using a **documented heuristic**, not a
trained classifier. This mirrors the same "no ML training needed, but
document the thresholds and rationale" philosophy already used by
``strategies.scene_detection.compute_motion_complexity_score``.

Two signals feed the classifier, both computed purely with
``cv2``/``numpy`` over the scene's already-sampled frames (no extra
decode work beyond what the pipeline already performs):

1. **Motion magnitude** -- mean absolute grayscale frame-differencing
   delta between consecutive sampled frames within the scene, normalized
   to ``[0, 1]`` by dividing by 255. This approximates how much the
   camera and/or subjects are moving: static interview setups and
   posed close-ups have low motion magnitude; handheld/whip-pans and
   fast action have high motion magnitude.
2. **Edge density** -- mean fraction of Canny-edge-flagged pixels across
   the scene's sampled frames. This approximates spatial/textural
   detail concentration: a close-up fills the frame with
   high-frequency detail (skin texture, hair, small props); a wide
   establishing shot spreads detail thinly across a large, often
   flatter frame area.

Classification is a simple, explicitly ordered decision list (documented
in :class:`RuleBasedShotTypeClassificationStrategy`) rather than a
learned decision boundary -- deliberately interpretable and tunable via
:class:`~ingestion_inspection_service.config.ShotTypeClassificationThresholds`.

This is explicitly a **coarse heuristic**, not a computer-vision shot
classifier grounded in framing/subject detection (no face/person
detector is used at all); see the module-level "Known limitations"
entry added to the README for the practical implications.
"""

from __future__ import annotations

import abc
import typing

import cv2
import numpy as np

from ingestion_inspection_service.config import (
    DEFAULT_SHOT_TYPE_CLASSIFICATION_THRESHOLDS,
    ShotTypeClassificationThresholds,
)
from ingestion_inspection_service.domain.models import ShotType


class ShotTypeClassificationStrategy(abc.ABC):
    """Abstract Strategy interface for pluggable shot-type classifiers.

    Mirrors ``strategies.scene_detection.SceneDetectionStrategy``'s
    shape: a concrete strategy reduces a sequence of BGR frames
    belonging to one scene down to a single :class:`ShotType`.
    """

    @abc.abstractmethod
    def classify_scene_frames(
        self, scene_frames_bgr: typing.Sequence[np.ndarray]
    ) -> ShotType:
        """Classifies the shot type of one scene from its sampled frames.

        Args:
            scene_frames_bgr: Ordered, consecutive BGR ``uint8`` frames
                sampled from within a single scene's time range.

        Returns:
            ShotType: The classified shot type, or
            :data:`ShotType.UNKNOWN` if too little data was supplied to
            classify confidently (e.g. zero or one frame, so no motion
            signal can be computed).
        """
        raise NotImplementedError

    @property
    @abc.abstractmethod
    def strategy_name(self) -> str:
        """Returns a short, stable, machine-readable strategy identifier."""
        raise NotImplementedError


class RuleBasedShotTypeClassificationStrategy(ShotTypeClassificationStrategy):
    """Classifies shot type from frame-differencing motion + Canny edge density.

    Algorithmic notes:
        For each consecutive frame pair in the scene, computes the mean
        absolute grayscale difference (frame differencing), normalized
        by 255, and averages this across all pairs to get the scene's
        ``motion_magnitude`` in ``[0, 1]``-ish range (in practice rarely
        exceeding ~0.3 for real footage). For each frame, computes the
        Canny edge map (`cv2.Canny`, same thresholds as
        ``compute_motion_complexity_score`` for consistency) and its
        edge-pixel fraction, averaged across the scene's frames to get
        ``edge_density``.

        Classification decision list, evaluated top to bottom (first
        match wins):

          1. ``motion_magnitude >= action_motion_magnitude_threshold``
             => :data:`ShotType.ACTION`. Rationale: fast, sustained
             frame-to-frame change is the least ambiguous action signal
             and should dominate regardless of framing/detail level
             (whip pans and fast action can occur in both tight and wide
             framing).
          2. ``edge_density >= close_up_edge_density_threshold and
             motion_magnitude <= low_motion_magnitude_threshold`` =>
             :data:`ShotType.CLOSE_UP`. Rationale: high spatial detail
             concentration with a comparatively static camera/subject is
             the signature of a close framing (face, hands, small
             object) where the subject occupies most of the frame.
          3. ``motion_magnitude <= low_motion_magnitude_threshold``
             (edge density below the close-up threshold) =>
             :data:`ShotType.DIALOGUE`. Rationale: static/low-motion
             scenes that are *not* detail-dense are the typical
             signature of medium/two-shot conversational framing (a
             locked-off or slow-panning camera on people talking,
             without the fine per-pixel detail concentration of a tight
             close-up).
          4. Otherwise => :data:`ShotType.WIDE`. Rationale: moderate
             motion with comparatively low edge density is consistent
             with an establishing/wide shot, where detail is spread
             thinly across a large frame area and some camera/subject
             movement (e.g. a slow crane or subjects walking within a
             large frame) is common without qualifying as "action".

        Complexity: O(n) frame-difference passes plus O(n) Canny passes
        over the scene's sampled frames, each O(H*W); identical
        asymptotic cost profile to the existing motion-complexity scorer.
    """

    def __init__(
        self,
        thresholds: ShotTypeClassificationThresholds = DEFAULT_SHOT_TYPE_CLASSIFICATION_THRESHOLDS,
    ) -> None:
        """Initializes the strategy with classification thresholds.

        Args:
            thresholds: Tunable motion/edge-density thresholds.
        """
        self._thresholds = thresholds

    def classify_scene_frames(
        self, scene_frames_bgr: typing.Sequence[np.ndarray]
    ) -> ShotType:
        """Classifies a scene's shot type from its sampled BGR frames.

        Args:
            scene_frames_bgr: Ordered, consecutive BGR ``uint8`` frames
                sampled from within a single scene's time range.

        Returns:
            ShotType: The classified shot type per the decision list
            documented on this class, or :data:`ShotType.UNKNOWN` if
            fewer than one frame was supplied.
        """
        if len(scene_frames_bgr) == 0:
            return ShotType.UNKNOWN

        edge_density = compute_average_edge_density(scene_frames_bgr)
        motion_magnitude = compute_average_motion_magnitude(scene_frames_bgr)

        if motion_magnitude >= self._thresholds.action_motion_magnitude_threshold:
            return ShotType.ACTION
        if (
            edge_density >= self._thresholds.close_up_edge_density_threshold
            and motion_magnitude <= self._thresholds.low_motion_magnitude_threshold
        ):
            return ShotType.CLOSE_UP
        if motion_magnitude <= self._thresholds.low_motion_magnitude_threshold:
            return ShotType.DIALOGUE
        return ShotType.WIDE

    @property
    def strategy_name(self) -> str:
        """Returns the stable identifier ``"rule_based_motion_edge_density"``."""
        return "rule_based_motion_edge_density"


def compute_average_motion_magnitude(
    frames_bgr: typing.Sequence[np.ndarray],
) -> float:
    """Computes the mean normalized frame-differencing motion magnitude.

    Args:
        frames_bgr: Ordered, consecutive BGR ``uint8`` frames.

    Returns:
        float: Mean of ``mean(|frame[i] - frame[i-1]|) / 255`` over all
        consecutive pairs, in an approximate ``[0, 1]`` range. Returns
        0.0 if fewer than two frames are supplied (no pair to diff).

    Algorithmic notes:
        O(n) grayscale conversions plus O(n-1) absolute-difference
        reductions, each O(H*W).
    """
    if len(frames_bgr) < 2:
        return 0.0

    grayscale_frames = [
        cv2.cvtColor(frame, cv2.COLOR_BGR2GRAY).astype(np.float64)
        for frame in frames_bgr
    ]
    per_pair_magnitudes = [
        float(np.mean(np.abs(grayscale_frames[index] - grayscale_frames[index - 1])))
        / 255.0
        for index in range(1, len(grayscale_frames))
    ]
    return float(np.mean(per_pair_magnitudes))


def compute_average_edge_density(frames_bgr: typing.Sequence[np.ndarray]) -> float:
    """Computes the mean Canny edge-pixel fraction across a set of frames.

    Args:
        frames_bgr: BGR ``uint8`` frames to score.

    Returns:
        float: Mean fraction of Canny-flagged edge pixels across all
        frames, in ``[0, 1]``. Returns 0.0 if ``frames_bgr`` is empty.

    Algorithmic notes:
        O(n) Canny passes, each O(H*W) via Sobel gradients + non-max
        suppression + hysteresis -- the same Canny call/threshold pair
        used by ``compute_motion_complexity_score`` for consistency
        across the codebase's edge-density heuristics.
    """
    if len(frames_bgr) == 0:
        return 0.0

    densities = []
    for frame in frames_bgr:
        grayscale_frame = cv2.cvtColor(frame, cv2.COLOR_BGR2GRAY)
        edge_map = cv2.Canny(grayscale_frame, threshold1=80, threshold2=160)
        densities.append(float(np.count_nonzero(edge_map)) / float(edge_map.size))
    return float(np.mean(densities))
