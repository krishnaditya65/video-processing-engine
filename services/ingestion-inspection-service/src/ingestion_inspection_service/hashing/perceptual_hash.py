"""Difference-hash (dHash) perceptual hashing and near-duplicate detection.

Implements a **perceptual hash from first principles** using only
``cv2``/``numpy`` primitives already used elsewhere in this codebase --
no external ``imagehash``/``phash`` dependency. The algorithm is the
classic "dHash" (difference hash), chosen over pHash (DCT-based) for its
simplicity and speed while still being robust to the kinds of
transformations this platform cares about (minor re-encode/bitrate
changes, small resizes, slight color/gamma shifts): mild re-compression
changes exact pixel values but rarely flips the *sign* of adjacent-pixel
brightness gradients, which is exactly what dHash encodes.

Algorithm (per Neal Krawetz's "Kind of Like That" dHash description):

  1. Convert the input image to grayscale.
  2. Resize to ``(hash_grid_size + 1, hash_grid_size)`` pixels
     (width = grid + 1, height = grid) using area-averaging
     interpolation (``cv2.INTER_AREA``), which -- unlike nearest/linear
     upsampling -- acts as a low-pass pre-filter and is the standard
     choice for photometric *downscaling*, matching OpenCV's own
     documented recommendation for shrinking images.
  3. For each row, compare each pixel to its immediate right neighbor:
     bit = 1 if ``pixel[x] > pixel[x + 1]`` else 0. This produces
     exactly ``hash_grid_size * hash_grid_size`` bits (one fewer column
     than the resized width, exactly what the ``+1`` column budgeted
     for).
  4. Pack the bits (row-major) into a single Python ``int`` for compact
     storage/comparison.

Two hashes' similarity is measured by **Hamming distance** (popcount of
the XOR) -- the number of differing gradient-direction bits, in
``[0, hash_grid_size**2]``. Two images/assets are considered
"near-duplicates" when their Hamming distance is at or below
:data:`~ingestion_inspection_service.config.PerceptualHashConfiguration.near_duplicate_hamming_distance_threshold`.

Per-asset hashing note: a video asset is reduced to a single hash by
hashing the **elementwise mean** of its representative grayscale frames
(e.g. the pipeline's scene-representative sampled frames) rather than
hashing only the first frame -- this makes the hash more robust to a
single unrepresentative frame (a black flash, a watermark burst) at the
cost of blurring genuine variation across very different frames of the
same asset. Callers wanting frame-level (not asset-level) duplicate
detection should call :func:`compute_difference_hash_for_frame` per
frame directly instead.
"""

from __future__ import annotations

import dataclasses
import typing

import cv2
import numpy as np

from ingestion_inspection_service.config import (
    DEFAULT_PERCEPTUAL_HASH_CONFIGURATION,
    PerceptualHashConfiguration,
)


def compute_difference_hash_for_frame(
    frame_bgr: np.ndarray,
    configuration: PerceptualHashConfiguration = DEFAULT_PERCEPTUAL_HASH_CONFIGURATION,
) -> int:
    """Computes a dHash (difference hash) for a single BGR frame.

    Args:
        frame_bgr: A decoded frame, ``(H, W, 3)`` (or ``(H, W)``
            grayscale) ``uint8``/numeric array.
        configuration: Controls the hash grid size (bit count).

    Returns:
        int: A ``hash_grid_size * hash_grid_size``-bit unsigned integer
        (e.g. 64-bit for the default 8x8 grid) packed row-major, MSB
        first.

    Raises:
        ValueError: If ``frame_bgr`` is empty (zero-sized) or has an
            unsupported number of dimensions.

    Algorithmic notes:
        O(H*W) for the grayscale conversion and O(grid^2) for the
        resize/compare/pack steps -- the resize target is a small fixed
        constant (default 9x8), so this is effectively O(H*W) dominated
        entirely by the initial grayscale conversion of the
        full-resolution input.
    """
    if frame_bgr.size == 0:
        raise ValueError("compute_difference_hash_for_frame received an empty array.")

    if frame_bgr.ndim == 3:
        grayscale_frame = cv2.cvtColor(frame_bgr, cv2.COLOR_BGR2GRAY)
    elif frame_bgr.ndim == 2:
        grayscale_frame = frame_bgr
    else:
        raise ValueError(
            f"compute_difference_hash_for_frame expects a 2-D or 3-D array, "
            f"got shape {frame_bgr.shape}."
        )

    return _difference_hash_from_grayscale_array(
        grayscale_frame.astype(np.float64), configuration
    )


def compute_asset_representative_perceptual_hash(
    representative_frames_bgr: typing.Sequence[np.ndarray],
    configuration: PerceptualHashConfiguration = DEFAULT_PERCEPTUAL_HASH_CONFIGURATION,
) -> int:
    """Computes one dHash summarizing a set of an asset's representative frames.

    Args:
        representative_frames_bgr: One or more BGR frames representing
            an asset (e.g. one frame per detected scene). All frames
            must share the same shape.
        configuration: Controls the hash grid size (bit count).

    Returns:
        int: The dHash of the elementwise-mean grayscale image across
        all supplied frames (see module docstring for the rationale).

    Raises:
        ValueError: If ``representative_frames_bgr`` is empty, or if the
            supplied frames do not all share the same shape.

    Algorithmic notes:
        O(n * H * W) to grayscale-convert and accumulate ``n`` frames,
        plus the O(H*W)-dominated hash computation described in
        :func:`compute_difference_hash_for_frame`.
    """
    if len(representative_frames_bgr) == 0:
        raise ValueError(
            "compute_asset_representative_perceptual_hash requires at least "
            "one representative frame."
        )

    first_shape = representative_frames_bgr[0].shape
    grayscale_accumulator = np.zeros(first_shape[:2], dtype=np.float64)
    for frame in representative_frames_bgr:
        if frame.shape != first_shape:
            raise ValueError(
                "All representative frames must share the same shape; got "
                f"{first_shape} and {frame.shape}."
            )
        grayscale_frame = (
            cv2.cvtColor(frame, cv2.COLOR_BGR2GRAY) if frame.ndim == 3 else frame
        )
        grayscale_accumulator += grayscale_frame.astype(np.float64)

    mean_grayscale_image = grayscale_accumulator / len(representative_frames_bgr)
    return _difference_hash_from_grayscale_array(mean_grayscale_image, configuration)


def _difference_hash_from_grayscale_array(
    grayscale_image: np.ndarray, configuration: PerceptualHashConfiguration
) -> int:
    """Shared dHash core: resize, compare-right-neighbor, pack bits.

    Args:
        grayscale_image: A single-channel ``float64`` image of any
            input resolution.
        configuration: Controls the hash grid size.

    Returns:
        int: The packed dHash value.
    """
    grid_size = configuration.hash_grid_size
    resized = cv2.resize(
        grayscale_image,
        (grid_size + 1, grid_size),
        interpolation=cv2.INTER_AREA,
    )

    bits = resized[:, :-1] > resized[:, 1:]
    hash_value = 0
    for bit in bits.flatten():
        hash_value = (hash_value << 1) | int(bit)
    return hash_value


def compute_hamming_distance(hash_a: int, hash_b: int) -> int:
    """Computes the Hamming distance (differing-bit count) between two hashes.

    Args:
        hash_a: First packed perceptual hash.
        hash_b: Second packed perceptual hash.

    Returns:
        int: The number of bit positions at which ``hash_a`` and
        ``hash_b`` differ, i.e. ``popcount(hash_a XOR hash_b)``.

    Algorithmic notes:
        O(bit_count) via Python's built-in ``int.bit_count()``
        (constant-factor-efficient CPython popcount, available since
        Python 3.10, which matches this service's ``requires-python``
        floor).
    """
    return (hash_a ^ hash_b).bit_count()


@dataclasses.dataclass(frozen=True)
class NearDuplicateAssetMatch:
    """One near-duplicate match result.

    Attributes:
        asset_id: The candidate asset's identifier.
        hamming_distance: The Hamming distance between the query hash
            and this candidate's registered hash; lower means more
            similar.
    """

    asset_id: str
    hamming_distance: int


class InMemoryPerceptualHashRegistry:
    """A small in-memory registry mapping asset IDs to perceptual hashes.

    This is intentionally a plain in-process dict-backed registry (not
    backed by a database/index), matching the task's "small in-memory
    hash registry" scope -- suitable for a single service instance's
    working set or as the storage abstraction a real persistent-index
    implementation would sit behind later.
    """

    def __init__(self) -> None:
        """Initializes an empty registry."""
        self._asset_id_to_hash: typing.Dict[str, int] = {}

    def register_asset_hash(self, asset_id: str, perceptual_hash: int) -> None:
        """Registers (or overwrites) an asset's perceptual hash.

        Args:
            asset_id: Opaque identifier for the asset.
            perceptual_hash: The asset's packed dHash value, as
                produced by :func:`compute_asset_representative_perceptual_hash`.
        """
        self._asset_id_to_hash[asset_id] = perceptual_hash

    def get_asset_hash(self, asset_id: str) -> typing.Optional[int]:
        """Looks up a previously registered asset's hash.

        Args:
            asset_id: The asset identifier to look up.

        Returns:
            int or None: The registered hash, or ``None`` if
            ``asset_id`` has not been registered.
        """
        return self._asset_id_to_hash.get(asset_id)

    def find_near_duplicate_assets(
        self,
        query_asset_id: str,
        *,
        hamming_distance_threshold: typing.Optional[int] = None,
        configuration: PerceptualHashConfiguration = DEFAULT_PERCEPTUAL_HASH_CONFIGURATION,
    ) -> typing.List[NearDuplicateAssetMatch]:
        """Finds all other registered assets within a Hamming-distance threshold.

        Args:
            query_asset_id: The asset to find near-duplicates of; must
                already be registered.
            hamming_distance_threshold: Maximum Hamming distance to
                consider a match; defaults to
                ``configuration.near_duplicate_hamming_distance_threshold``
                when not given.
            configuration: Supplies the default threshold when
                ``hamming_distance_threshold`` is not explicitly given.

        Returns:
            list[NearDuplicateAssetMatch]: Matches (excluding
            ``query_asset_id`` itself), sorted by ascending Hamming
            distance (most similar first).

        Raises:
            KeyError: If ``query_asset_id`` has not been registered.

        Algorithmic notes:
            O(n) linear scan over all registered assets, each a O(1)
            XOR + popcount comparison -- appropriate for the "small
            in-memory registry" scope; a production-scale duplicate
            index would instead use a BK-tree or LSH bucket structure
            to avoid the linear scan, which is out of scope here.
        """
        if query_asset_id not in self._asset_id_to_hash:
            raise KeyError(
                f"query_asset_id '{query_asset_id}' is not registered in this hash registry."
            )

        effective_threshold = (
            hamming_distance_threshold
            if hamming_distance_threshold is not None
            else configuration.near_duplicate_hamming_distance_threshold
        )
        query_hash = self._asset_id_to_hash[query_asset_id]

        matches = [
            NearDuplicateAssetMatch(
                asset_id=candidate_asset_id,
                hamming_distance=compute_hamming_distance(query_hash, candidate_hash),
            )
            for candidate_asset_id, candidate_hash in self._asset_id_to_hash.items()
            if candidate_asset_id != query_asset_id
            and compute_hamming_distance(query_hash, candidate_hash)
            <= effective_threshold
        ]
        matches.sort(key=lambda match: match.hamming_distance)
        return matches

    def __len__(self) -> int:
        """Returns the number of assets currently registered."""
        return len(self._asset_id_to_hash)


def find_near_duplicate_assets(
    query_asset_id: str,
    hash_registry: typing.Mapping[str, int],
    *,
    hamming_distance_threshold: int = DEFAULT_PERCEPTUAL_HASH_CONFIGURATION.near_duplicate_hamming_distance_threshold,
) -> typing.List[NearDuplicateAssetMatch]:
    """Functional-style near-duplicate search over a plain ``{asset_id: hash}`` map.

    A free-function alternative to
    :meth:`InMemoryPerceptualHashRegistry.find_near_duplicate_assets` for
    callers that already have a plain mapping (e.g. constructed directly
    in a test or passed in from elsewhere) rather than an
    :class:`InMemoryPerceptualHashRegistry` instance.

    Args:
        query_asset_id: The asset to find near-duplicates of; must be a
            key in ``hash_registry``.
        hash_registry: Mapping of asset ID to packed perceptual hash.
        hamming_distance_threshold: Maximum Hamming distance to
            consider a match.

    Returns:
        list[NearDuplicateAssetMatch]: Matches (excluding
        ``query_asset_id`` itself), sorted by ascending Hamming
        distance.

    Raises:
        KeyError: If ``query_asset_id`` is not a key in ``hash_registry``.

    Algorithmic notes:
        O(n) linear scan, same as the registry method above.
    """
    if query_asset_id not in hash_registry:
        raise KeyError(
            f"query_asset_id '{query_asset_id}' is not present in hash_registry."
        )

    query_hash = hash_registry[query_asset_id]
    matches = [
        NearDuplicateAssetMatch(
            asset_id=candidate_asset_id,
            hamming_distance=compute_hamming_distance(query_hash, candidate_hash),
        )
        for candidate_asset_id, candidate_hash in hash_registry.items()
        if candidate_asset_id != query_asset_id
        and compute_hamming_distance(query_hash, candidate_hash)
        <= hamming_distance_threshold
    ]
    matches.sort(key=lambda match: match.hamming_distance)
    return matches
