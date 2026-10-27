"""Unit tests for the from-first-principles dHash implementation and duplicate search.

All tests operate on small synthetic NumPy frames; no video file or
external phash library is required.
"""

from __future__ import annotations

import numpy as np
import pytest

from ingestion_inspection_service.config import PerceptualHashConfiguration
from ingestion_inspection_service.hashing.perceptual_hash import (
    InMemoryPerceptualHashRegistry,
    compute_asset_representative_perceptual_hash,
    compute_difference_hash_for_frame,
    compute_hamming_distance,
    find_near_duplicate_assets,
)
from tests.conftest import make_checkerboard_frame, make_solid_color_frame


class TestComputeDifferenceHashForFrame:
    """Tests the core dHash computation."""

    def test_identical_frames_produce_identical_hashes(self) -> None:
        """Hashing the same frame twice yields the exact same hash value."""
        frame = make_checkerboard_frame(64, 64, 8, seed=3)
        hash_a = compute_difference_hash_for_frame(frame)
        hash_b = compute_difference_hash_for_frame(frame.copy())
        assert hash_a == hash_b

    def test_hash_fits_in_configured_bit_count(self) -> None:
        """The default 8x8 grid produces a hash that fits in 64 bits."""
        frame = make_checkerboard_frame(32, 32, 4, seed=1)
        hash_value = compute_difference_hash_for_frame(frame)
        assert 0 <= hash_value < (1 << 64)

    def test_smaller_grid_size_produces_fewer_bits(self) -> None:
        """A 4x4 grid configuration produces a hash fitting in 16 bits."""
        frame = make_checkerboard_frame(32, 32, 4, seed=2)
        configuration = PerceptualHashConfiguration(hash_grid_size=4)
        hash_value = compute_difference_hash_for_frame(frame, configuration)
        assert 0 <= hash_value < (1 << 16)

    def test_solid_color_frame_hashes_to_zero(self) -> None:
        """A perfectly flat frame has no left>right gradients, hashing to 0."""
        frame = make_solid_color_frame(64, 64, (100, 100, 100))
        assert compute_difference_hash_for_frame(frame) == 0

    def test_grayscale_2d_input_is_accepted(self) -> None:
        """A 2-D (already-grayscale) array is accepted without a BGR conversion."""
        grayscale_frame = np.full((32, 32), 128, dtype=np.uint8)
        assert compute_difference_hash_for_frame(grayscale_frame) == 0

    def test_empty_array_raises_value_error(self) -> None:
        """An empty (zero-sized) input array raises ValueError."""
        with pytest.raises(ValueError):
            compute_difference_hash_for_frame(np.zeros((0, 0, 3), dtype=np.uint8))

    def test_similar_frames_have_small_hamming_distance(self) -> None:
        """A frame and a lightly-perturbed copy (small noise) hash to nearby values."""
        base_frame = make_checkerboard_frame(64, 64, 8, seed=11)
        rng = np.random.default_rng(123)
        noise = rng.integers(-5, 6, size=base_frame.shape, dtype=np.int16)
        perturbed_frame = np.clip(
            base_frame.astype(np.int16) + noise, 0, 255
        ).astype(np.uint8)

        hash_a = compute_difference_hash_for_frame(base_frame)
        hash_b = compute_difference_hash_for_frame(perturbed_frame)
        distance = compute_hamming_distance(hash_a, hash_b)
        assert distance <= 8  # small perturbation should not flip many gradient bits

    def test_very_different_frames_have_large_hamming_distance(self) -> None:
        """Two unrelated high-detail patterns hash to a large Hamming distance."""
        frame_a = make_checkerboard_frame(64, 64, 4, seed=1)
        frame_b = make_checkerboard_frame(64, 64, 4, seed=999)
        distance = compute_hamming_distance(
            compute_difference_hash_for_frame(frame_a),
            compute_difference_hash_for_frame(frame_b),
        )
        assert distance > 8


class TestComputeHammingDistance:
    """Tests the Hamming distance helper directly."""

    def test_identical_hashes_have_zero_distance(self) -> None:
        """The same hash value compared to itself has zero distance."""
        assert compute_hamming_distance(0b10110, 0b10110) == 0

    def test_fully_inverted_hashes_have_full_bit_count_distance(self) -> None:
        """Two bitwise-complementary 8-bit hashes differ in all 8 bits."""
        assert compute_hamming_distance(0b0000_0000, 0b1111_1111) == 8

    def test_single_bit_flip_has_distance_one(self) -> None:
        """Flipping exactly one bit produces a Hamming distance of 1."""
        assert compute_hamming_distance(0b1010, 0b1011) == 1


class TestComputeAssetRepresentativePerceptualHash:
    """Tests the multi-frame asset-level hash reduction."""

    def test_single_repeated_frame_matches_single_frame_hash(self) -> None:
        """Averaging N copies of the same frame equals hashing that one frame."""
        frame = make_checkerboard_frame(48, 48, 6, seed=4)
        asset_hash = compute_asset_representative_perceptual_hash(
            [frame.copy() for _ in range(5)]
        )
        assert asset_hash == compute_difference_hash_for_frame(frame)

    def test_empty_frame_list_raises_value_error(self) -> None:
        """Computing an asset hash from zero frames raises ValueError."""
        with pytest.raises(ValueError):
            compute_asset_representative_perceptual_hash([])

    def test_mismatched_frame_shapes_raise_value_error(self) -> None:
        """Frames of differing shape cannot be averaged together."""
        with pytest.raises(ValueError):
            compute_asset_representative_perceptual_hash(
                [
                    make_solid_color_frame(32, 32, (1, 2, 3)),
                    make_solid_color_frame(16, 16, (1, 2, 3)),
                ]
            )


class TestInMemoryPerceptualHashRegistry:
    """Tests the in-memory registry's registration and duplicate search."""

    def test_register_and_lookup_round_trips(self) -> None:
        """A registered asset hash can be looked up by asset_id."""
        registry = InMemoryPerceptualHashRegistry()
        registry.register_asset_hash("asset-a", 0xABCD)
        assert registry.get_asset_hash("asset-a") == 0xABCD

    def test_lookup_of_unregistered_asset_returns_none(self) -> None:
        """Looking up an unknown asset_id returns None rather than raising."""
        registry = InMemoryPerceptualHashRegistry()
        assert registry.get_asset_hash("does-not-exist") is None

    def test_len_reflects_registered_asset_count(self) -> None:
        """len(registry) reports the number of registered assets."""
        registry = InMemoryPerceptualHashRegistry()
        registry.register_asset_hash("a", 1)
        registry.register_asset_hash("b", 2)
        assert len(registry) == 2

    def test_finds_near_duplicate_within_threshold(self) -> None:
        """An asset with a hash close to another registered asset is found."""
        registry = InMemoryPerceptualHashRegistry()
        registry.register_asset_hash("original", 0b0000_0000)
        registry.register_asset_hash("near_duplicate", 0b0000_0011)  # distance 2
        registry.register_asset_hash("unrelated", 0b1111_1111)  # distance 8

        matches = registry.find_near_duplicate_assets(
            "original", hamming_distance_threshold=4
        )

        assert [match.asset_id for match in matches] == ["near_duplicate"]
        assert matches[0].hamming_distance == 2

    def test_matches_sorted_by_ascending_distance(self) -> None:
        """Multiple matches are returned most-similar-first."""
        registry = InMemoryPerceptualHashRegistry()
        registry.register_asset_hash("query", 0b0000_0000)
        registry.register_asset_hash("far", 0b0000_1111)  # distance 4
        registry.register_asset_hash("near", 0b0000_0001)  # distance 1

        matches = registry.find_near_duplicate_assets(
            "query", hamming_distance_threshold=8
        )
        assert [match.asset_id for match in matches] == ["near", "far"]

    def test_query_excludes_itself_from_matches(self) -> None:
        """The query asset never appears in its own match list."""
        registry = InMemoryPerceptualHashRegistry()
        registry.register_asset_hash("solo", 42)
        matches = registry.find_near_duplicate_assets(
            "solo", hamming_distance_threshold=64
        )
        assert matches == []

    def test_unregistered_query_asset_raises_key_error(self) -> None:
        """Querying for near-duplicates of an unregistered asset raises KeyError."""
        registry = InMemoryPerceptualHashRegistry()
        with pytest.raises(KeyError):
            registry.find_near_duplicate_assets("nope")


class TestFindNearDuplicateAssetsFunction:
    """Tests the free-function variant operating over a plain dict registry."""

    def test_finds_matches_over_plain_dict(self) -> None:
        """The functional API behaves the same as the registry method."""
        hash_registry = {"a": 0b0000_0000, "b": 0b0000_0001, "c": 0b1111_0000}
        matches = find_near_duplicate_assets("a", hash_registry, hamming_distance_threshold=2)
        assert [match.asset_id for match in matches] == ["b"]

    def test_unregistered_query_raises_key_error(self) -> None:
        """A query asset_id absent from the mapping raises KeyError."""
        with pytest.raises(KeyError):
            find_near_duplicate_assets("missing", {"a": 1})
