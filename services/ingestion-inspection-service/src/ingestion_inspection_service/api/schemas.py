"""Pydantic request/response schemas for the FastAPI HTTP service.

These schemas define the wire contract with the Go orchestrator. Field
names use snake_case per Python/PEP 8 convention (see the package-level
docstring and README for the naming-convention rationale); FastAPI/
Pydantic will serialize them as-is, so the Go client should expect
snake_case JSON keys unless an explicit alias is configured at the Go
client layer.
"""

from __future__ import annotations

import typing

import pydantic


class InspectAssetRequestBody(pydantic.BaseModel):
    """Request body for ``POST /assets/{assetId}/inspect``.

    Attributes:
        source_file_path: Filesystem path (accessible to this service,
            e.g. a shared mezzanine storage mount) of the video file to
            inspect.
        scene_detection_strategy: Which pluggable scene-cut Strategy to
            use: ``"histogram_difference"`` or
            ``"structural_similarity"``. Defaults to
            ``"structural_similarity"``.
        analysis_frames_per_second: Optional override for the analysis
            frame-sampling rate.
        skip_audio_loudness_analysis: When true, skips ffmpeg audio
            extraction and the loudness analysis stage.
    """

    source_file_path: str = pydantic.Field(
        ..., description="Filesystem path of the mezzanine video file to inspect."
    )
    scene_detection_strategy: typing.Literal[
        "histogram_difference", "structural_similarity"
    ] = pydantic.Field(
        default="structural_similarity",
        description="Which pluggable scene-cut detection Strategy to use.",
    )
    analysis_frames_per_second: typing.Optional[float] = pydantic.Field(
        default=None,
        description="Override for the analysis frame-sampling rate (fps).",
        gt=0,
    )
    skip_audio_loudness_analysis: bool = pydantic.Field(
        default=False,
        description="When true, skips the ffmpeg-based loudness analysis stage.",
    )


class InspectAssetResponseBody(pydantic.BaseModel):
    """Response body for ``POST /assets/{assetId}/inspect``.

    This intentionally mirrors
    :class:`ingestion_inspection_service.domain.models.StructuralMap`'s
    JSON-serializable shape (via
    ``model_config = {"extra": "allow"}`` and direct construction from
    the structural map's dict form) rather than re-declaring every
    nested field as a separate Pydantic model, since the domain
    dataclasses are already the canonical schema and the pipeline is
    the single producer of this payload.

    Attributes:
        structural_map: The full structural map artifact, as a plain
            JSON-serializable dict.
    """

    structural_map: typing.Dict[str, typing.Any] = pydantic.Field(
        ..., description="The full structural map artifact for this asset."
    )


class InspectionErrorResponseBody(pydantic.BaseModel):
    """Error response body for failed inspection requests.

    Attributes:
        asset_id: The asset ID the failed request was for.
        error_message: Human-readable description of the failure.
    """

    asset_id: str
    error_message: str


class DuplicateDetectionRequestBody(pydantic.BaseModel):
    """Request body for ``POST /assets/{assetId}/duplicates``.

    Attributes:
        source_file_path: Filesystem path of the video file to hash and
            register/check for near-duplicates.
        near_duplicate_hamming_distance_threshold: Optional override for
            the Hamming-distance match threshold; defaults to the
            configured platform default when omitted.
        representative_frame_sample_count: How many evenly-spaced frames
            to sample from the asset to build its representative
            perceptual hash. Small by design (this is a cheap
            triage/duplicate check, not a full inspection pass).
    """

    source_file_path: str = pydantic.Field(
        ..., description="Filesystem path of the video file to hash/check."
    )
    near_duplicate_hamming_distance_threshold: typing.Optional[int] = pydantic.Field(
        default=None,
        description="Override for the near-duplicate Hamming distance threshold.",
        ge=0,
    )
    representative_frame_sample_count: int = pydantic.Field(
        default=5,
        description="Number of evenly-spaced frames sampled to build the asset hash.",
        gt=0,
    )


class NearDuplicateMatchBody(pydantic.BaseModel):
    """One near-duplicate match, mirroring ``hashing.perceptual_hash.NearDuplicateAssetMatch``.

    Attributes:
        asset_id: The matching candidate asset's identifier.
        hamming_distance: Hamming distance between the query and this
            candidate's registered hash.
    """

    asset_id: str
    hamming_distance: int


class DuplicateDetectionResponseBody(pydantic.BaseModel):
    """Response body for ``POST /assets/{assetId}/duplicates``.

    Attributes:
        asset_id: The asset whose hash was computed/registered/checked.
        perceptual_hash_hex: The computed dHash, as a hex string, for
            auditability/debugging.
        near_duplicate_matches: Other registered assets found within the
            Hamming-distance threshold, most similar first.
    """

    asset_id: str
    perceptual_hash_hex: str
    near_duplicate_matches: typing.List[NearDuplicateMatchBody]
