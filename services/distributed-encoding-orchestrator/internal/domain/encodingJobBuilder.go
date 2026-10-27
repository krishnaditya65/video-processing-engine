// This file implements the Builder design pattern for constructing
// EncodingJob values. Direct struct-literal construction of an
// EncodingJob is legal Go but easy to get wrong (forgetting scene
// boundaries, an empty ladder, a missing job identifier); the builder
// centralizes required-field validation and gives call sites a
// self-documenting fluent construction sequence.
package domain

import (
	"errors"
	"fmt"
	"time"

	"distributed-encoding-orchestrator/internal/ffmpeg"
)

// EncodingJobBuilder incrementally accumulates the fields of an
// EncodingJob and validates them on Build. Each With* method returns the
// same *EncodingJobBuilder to allow fluent chaining:
//
//	job, err := NewEncodingJobBuilder("job-123", "s3://bucket/movie.mov").
//	    WithTargetCodec(ffmpeg.CodecHevc).
//	    WithSceneBoundaries(scenes).
//	    WithBitrateLadderRungs(rungs).
//	    WithMaxChunkDurationSeconds(2.0).
//	    Build()
//
// Concurrency/safety: an EncodingJobBuilder is a plain mutable value
// intended for single-goroutine, single-call-chain use while constructing
// one job; it is NOT safe for concurrent use by multiple goroutines. The
// EncodingJob value produced by Build is an ordinary immutable-by-convention
// data value with no such restriction.
type EncodingJobBuilder struct {
	jobIdentifier           string
	sourceAssetURI          string
	targetCodec             ffmpeg.SupportedVideoCodec
	targetCodecWasSet       bool
	sceneBoundaries         []SceneBoundary
	bitrateLadderRungs      []BitrateLadderRung
	maxChunkDurationSeconds float64
}

// NewEncodingJobBuilder starts constructing an EncodingJob with the two
// fields that have no sensible default: jobIdentifier (must be
// non-empty) and sourceAssetURI (must be non-empty). Both are validated
// at Build time rather than here, so the fluent chain can be assembled
// before any error is surfaced.
func NewEncodingJobBuilder(jobIdentifier string, sourceAssetURI string) *EncodingJobBuilder {
	return &EncodingJobBuilder{
		jobIdentifier:           jobIdentifier,
		sourceAssetURI:          sourceAssetURI,
		maxChunkDurationSeconds: 2.0, // Netflix-style ~2s micro-chunk default.
	}
}

// WithTargetCodec sets which codec every chunk of this job will be
// encoded with. Required — Build fails if never called.
func (builder *EncodingJobBuilder) WithTargetCodec(codec ffmpeg.SupportedVideoCodec) *EncodingJobBuilder {
	builder.targetCodec = codec
	builder.targetCodecWasSet = true
	return builder
}

// WithSceneBoundaries sets the ingestion service's structural map for
// this asset. Required — Build fails if empty.
func (builder *EncodingJobBuilder) WithSceneBoundaries(scenes []SceneBoundary) *EncodingJobBuilder {
	builder.sceneBoundaries = scenes
	return builder
}

// WithBitrateLadderRungs sets the (resolution, bitrate) renditions to
// produce for every chunk. Required — Build fails if empty.
func (builder *EncodingJobBuilder) WithBitrateLadderRungs(rungs []BitrateLadderRung) *EncodingJobBuilder {
	builder.bitrateLadderRungs = rungs
	return builder
}

// WithMaxChunkDurationSeconds overrides the default 2-second maximum
// chunk duration used when splitting long scenes. Pass a value <= 0 to
// disable splitting entirely (one chunk per scene, regardless of length).
func (builder *EncodingJobBuilder) WithMaxChunkDurationSeconds(seconds float64) *EncodingJobBuilder {
	builder.maxChunkDurationSeconds = seconds
	return builder
}

// Build validates the accumulated fields and, if valid, returns the
// finished EncodingJob with Status set to EncodingJobStatusPending and
// SubmittedAt set to the current time. Returns a descriptive error
// (without constructing a job) if jobIdentifier/sourceAssetURI are
// empty, no target codec was set, no scene boundaries were provided, or
// no bitrate ladder rungs were provided.
func (builder *EncodingJobBuilder) Build() (EncodingJob, error) {
	var validationErrors []error

	if builder.jobIdentifier == "" {
		validationErrors = append(validationErrors, errors.New("jobIdentifier must not be empty"))
	}
	if builder.sourceAssetURI == "" {
		validationErrors = append(validationErrors, errors.New("sourceAssetURI must not be empty"))
	}
	if !builder.targetCodecWasSet {
		validationErrors = append(validationErrors, errors.New("WithTargetCodec must be called"))
	}
	if len(builder.sceneBoundaries) == 0 {
		validationErrors = append(validationErrors, errors.New("WithSceneBoundaries must be called with at least one scene"))
	}
	if len(builder.bitrateLadderRungs) == 0 {
		validationErrors = append(validationErrors, errors.New("WithBitrateLadderRungs must be called with at least one rung"))
	}

	if len(validationErrors) > 0 {
		return EncodingJob{}, fmt.Errorf("domain: cannot build EncodingJob: %w", errors.Join(validationErrors...))
	}

	return EncodingJob{
		JobIdentifier:           builder.jobIdentifier,
		SourceAssetURI:          builder.sourceAssetURI,
		TargetCodec:             builder.targetCodec,
		SceneBoundaries:         builder.sceneBoundaries,
		BitrateLadderRungs:      builder.bitrateLadderRungs,
		MaxChunkDurationSeconds: builder.maxChunkDurationSeconds,
		SubmittedAt:             time.Now(),
		Status:                  EncodingJobStatusPending,
	}, nil
}
