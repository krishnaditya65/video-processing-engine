// Package fmp4 implements a simplified-but-structurally-correct ISO-BMFF
// (fragmented MP4) box writer good enough to demonstrate real JIT
// (just-in-time) packaging: given raw video/audio elementary-stream chunk
// bytes fetched from object storage, it produces a byte-correct
// ftyp/moov/moof/mdat box tree with proper 32-bit size prefixes and proper
// parent/child nesting, matching what a real fMP4 muxer (e.g. Shaka
// Packager, bento4) emits at the box-structure level.
//
// # What is implemented vs. simplified/omitted
//
// Implemented, byte-correct, at a real ISO-BMFF box level:
//   - ftyp (File Type box): major_brand/minor_version/compatible_brands.
//   - moov (Movie box) containing mvhd (Movie Header) and one trak (Track
//     box) per elementary stream, each trak containing tkhd (Track Header)
//     and a minimal mdia > mdhd (Media Header) skeleton, plus an mvex
//     (Movie Extends) with trex (Track Extends) to mark the file as
//     fragmented, per ISO/IEC 14496-12.
//   - moof (Movie Fragment box) containing mfhd (Movie Fragment Header)
//     and one traf (Track Fragment box) per stream, each traf containing
//     tfhd (Track Fragment Header) and trun (Track Fragment Run) with a
//     correct sample count and a correct data_offset back-reference into
//     the following mdat.
//   - mdat (Media Data box) containing the concatenated raw chunk payload
//     bytes, length-correct against its own size prefix.
//   - Every box's 32-bit big-endian size field is verified by the test
//     suite to equal 8 + len(payload), and to equal the sum of its
//     children's sizes plus its own header/fixed fields, i.e. the tree is
//     byte-exact, not just superficially box-shaped.
//
// Simplified or omitted (documented explicitly so this is not mistaken for
// a spec-complete muxer):
//   - No stbl (Sample Table) with real stsd sample-description entries
//     (avcC/hvcC/esds codec-init data); a real player needs stsd/avcC to
//     configure its decoder, which this simulation stubs as an empty
//     placeholder box instead of encoding actual SPS/PPS/codec-private
//     data.
//   - trun does not carry per-sample size/duration/flags entries (no
//     sample_size / sample_duration / sample_flags arrays); it declares a
//     single logical sample spanning the whole chunk. Real content has a
//     size entry per encoded frame.
//   - No sidx (Segment Index box), no styp, no CENC 'senc'/'saiz'/'saio'
//     encryption auxiliary-information boxes even when DRM is active
//     (encryption signaling here lives purely in the manifest layer — see
//     package drm — not in the packaged box stream).
//   - 64-bit ("largesize") box headers are not implemented; every box in
//     this package is assumed to fit in a 32-bit size field, true for the
//     ~2-second chunks this service targets.
//   - No real video/audio bitstream parsing: the "video track" and "audio
//     track" bytes are treated as opaque payload and copied verbatim into
//     mdat.
package fmp4

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// isoBmffBoxType is a 4-character ISO-BMFF box type code (e.g. "ftyp",
// "moov"). It is kept as a distinct string type rather than a bare string
// to make box-construction call sites self-documenting.
type isoBmffBoxType string

// writeFullyFormedIsoBmffBox writes one ISO-BMFF box: a 4-byte big-endian
// size field covering the whole box (header + payload), the 4-byte
// ASCII box type, and the payload bytes verbatim. This is the single
// primitive every higher-level box builder in this file goes through, so
// every box in the tree is guaranteed to carry a byte-correct size field.
func writeFullyFormedIsoBmffBox(boxType isoBmffBoxType, payload []byte) []byte {
	totalSize := uint32(8 + len(payload))
	buf := make([]byte, 0, totalSize)
	sizeField := make([]byte, 4)
	binary.BigEndian.PutUint32(sizeField, totalSize)
	buf = append(buf, sizeField...)
	buf = append(buf, []byte(boxType)...)
	buf = append(buf, payload...)
	return buf
}

// concatenateChildBoxes flattens an ordered list of already-serialized
// child box byte slices into one contiguous payload, for use as a parent
// box's payload (e.g. moov's payload is mvhd's bytes followed by each
// trak's bytes).
func concatenateChildBoxes(children ...[]byte) []byte {
	var buf bytes.Buffer
	for _, child := range children {
		buf.Write(child)
	}
	return buf.Bytes()
}

// BuildFileTypeBox constructs the "ftyp" box that must be the first box in
// any ISO-BMFF file, declaring the major brand, minor version, and
// compatible brand list. JIT-packaged fMP4 responses in this service use
// major brand "isom" with "iso6"/"mp42" listed as compatible, matching
// common CMAF-style fragmented output.
func BuildFileTypeBox() []byte {
	payload := make([]byte, 0, 16)
	payload = append(payload, []byte("isom")...)      // major_brand
	payload = append(payload, 0x00, 0x00, 0x02, 0x00) // minor_version = 512
	payload = append(payload, []byte("iso6")...)      // compatible_brands[0]
	payload = append(payload, []byte("mp42")...)      // compatible_brands[1]
	return writeFullyFormedIsoBmffBox("ftyp", payload)
}

// TrackDescriptor describes one elementary-stream track (video or audio)
// for the purposes of building this chunk's moov/moof skeleton: enough
// metadata to write correctly-sized tkhd/mdhd/tfhd/trun boxes, without
// attempting real codec-specific sample-description encoding (see the
// package doc's "simplified/omitted" list).
type TrackDescriptor struct {
	// TrackID is the ISO-BMFF track_ID, unique within the file (this
	// service uses 1 for video, 2 for audio by convention).
	TrackID uint32
	// TimescaleUnitsPerSecond is this track's media timescale.
	TimescaleUnitsPerSecond uint32
	// SamplePayload is the raw elementary-stream bytes for this track's
	// single logical sample in this chunk (see package doc: real
	// per-frame sample tables are not implemented).
	SamplePayload []byte
	// BaseMediaDecodeTimeInTimescaleUnits is this track's starting
	// decode timestamp for this fragment, in TimescaleUnitsPerSecond
	// units.
	BaseMediaDecodeTimeInTimescaleUnits uint64
}

// buildMovieHeaderBox constructs a minimal "mvhd" (Movie Header) box
// (version 0) with a fixed 1000 Hz movie timescale and duration left as 0
// (undefined for a fragmented/live-style file, per common CMAF practice).
func buildMovieHeaderBox() []byte {
	payload := make([]byte, 100)
	// version(1) + flags(3) already zero.
	binary.BigEndian.PutUint32(payload[4:8], 0)            // creation_time
	binary.BigEndian.PutUint32(payload[8:12], 0)           // modification_time
	binary.BigEndian.PutUint32(payload[12:16], 1000)       // timescale
	binary.BigEndian.PutUint32(payload[16:20], 0)          // duration (unknown/fragmented)
	binary.BigEndian.PutUint32(payload[20:24], 0x00010000) // rate = 1.0
	binary.BigEndian.PutUint16(payload[24:26], 0x0100)     // volume = 1.0
	// reserved(2)+reserved(2x4)+matrix(9x4)+pre_defined(6x4) left zero.
	binary.BigEndian.PutUint32(payload[96:100], 0xFFFFFFFF) // next_track_ID = unknown
	return writeFullyFormedIsoBmffBox("mvhd", payload)
}

// buildTrackHeaderBox constructs a minimal "tkhd" (Track Header) box
// (version 0) for the given track.
func buildTrackHeaderBox(track TrackDescriptor) []byte {
	payload := make([]byte, 84)
	payload[3] = 0x07 // flags = track_enabled | track_in_movie | track_in_preview
	binary.BigEndian.PutUint32(payload[12:16], track.TrackID)
	// reserved(4) left zero, duration(4) at [20:24] left zero (fragmented).
	binary.BigEndian.PutUint32(payload[76:80], 0x00010000) // width placeholder (16.16 fixed)
	binary.BigEndian.PutUint32(payload[80:84], 0x00010000) // height placeholder (16.16 fixed)
	return writeFullyFormedIsoBmffBox("tkhd", payload)
}

// buildMediaHeaderBox constructs a minimal "mdhd" (Media Header) box
// (version 0) carrying the track's timescale.
func buildMediaHeaderBox(track TrackDescriptor) []byte {
	payload := make([]byte, 24)
	binary.BigEndian.PutUint32(payload[12:16], track.TimescaleUnitsPerSecond) // timescale
	binary.BigEndian.PutUint16(payload[20:22], 0x55C4)                        // language = "und"
	return writeFullyFormedIsoBmffBox("mdhd", payload)
}

// buildMediaInformationSkeletonBox constructs a placeholder "minf" box
// containing only a stub "stbl" (Sample Table) box. A real muxer would
// populate stbl with stsd/stts/stsc/stsz/stco entries describing the
// track's codec configuration and sample layout; this simulation stops at
// a structurally-valid, empty stub (see package doc).
func buildMediaInformationSkeletonBox() []byte {
	stub := writeFullyFormedIsoBmffBox("stbl", nil)
	return writeFullyFormedIsoBmffBox("minf", stub)
}

// buildMediaBox constructs the "mdia" box wrapping mdhd + a minf skeleton
// for one track.
func buildMediaBox(track TrackDescriptor) []byte {
	mdhd := buildMediaHeaderBox(track)
	minf := buildMediaInformationSkeletonBox()
	return writeFullyFormedIsoBmffBox("mdia", concatenateChildBoxes(mdhd, minf))
}

// buildTrackBox constructs one "trak" box (tkhd + mdia) for the given
// track descriptor.
func buildTrackBox(track TrackDescriptor) []byte {
	tkhd := buildTrackHeaderBox(track)
	mdia := buildMediaBox(track)
	return writeFullyFormedIsoBmffBox("trak", concatenateChildBoxes(tkhd, mdia))
}

// buildTrackExtendsBox constructs one "trex" (Track Extends) box, required
// inside "mvex" to mark a track as fragmented per ISO/IEC 14496-12 §8.8.3.
func buildTrackExtendsBox(track TrackDescriptor) []byte {
	payload := make([]byte, 20)
	binary.BigEndian.PutUint32(payload[4:8], track.TrackID)
	binary.BigEndian.PutUint32(payload[8:12], 1) // default_sample_description_index
	return writeFullyFormedIsoBmffBox("trex", payload)
}

// buildMovieExtendsBox constructs the "mvex" box, one "trex" child per
// track, marking this file as fragmented.
func buildMovieExtendsBox(tracks []TrackDescriptor) []byte {
	var children [][]byte
	for _, track := range tracks {
		children = append(children, buildTrackExtendsBox(track))
	}
	return writeFullyFormedIsoBmffBox("mvex", concatenateChildBoxes(children...))
}

// BuildMovieBox constructs the top-level "moov" (Movie) box for the given
// tracks: mvhd, one trak per track, and an mvex marking the file
// fragmented. This is emitted once at the start of a JIT-packaged
// response's init segment.
func BuildMovieBox(tracks []TrackDescriptor) []byte {
	children := [][]byte{buildMovieHeaderBox()}
	for _, track := range tracks {
		children = append(children, buildTrackBox(track))
	}
	children = append(children, buildMovieExtendsBox(tracks))
	return writeFullyFormedIsoBmffBox("moov", concatenateChildBoxes(children...))
}

// buildMovieFragmentHeaderBox constructs the "mfhd" (Movie Fragment
// Header) box carrying this fragment's sequence number.
func buildMovieFragmentHeaderBox(fragmentSequenceNumber uint32) []byte {
	payload := make([]byte, 8)
	binary.BigEndian.PutUint32(payload[4:8], fragmentSequenceNumber)
	return writeFullyFormedIsoBmffBox("mfhd", payload)
}

// trackFragmentHeaderFlagDefaultBaseIsMoof marks a tfhd as using the moof
// (rather than the preceding mdat) as its implicit base for data_offset
// computation - standard practice for CMAF-style fragments.
const trackFragmentHeaderFlagDefaultBaseIsMoof = 0x020000

// buildTrackFragmentHeaderBox constructs one "tfhd" (Track Fragment
// Header) box for the given track.
func buildTrackFragmentHeaderBox(track TrackDescriptor) []byte {
	payload := make([]byte, 8)
	payload[1] = byte((trackFragmentHeaderFlagDefaultBaseIsMoof >> 16) & 0xFF)
	payload[2] = byte((trackFragmentHeaderFlagDefaultBaseIsMoof >> 8) & 0xFF)
	payload[3] = byte(trackFragmentHeaderFlagDefaultBaseIsMoof & 0xFF)
	binary.BigEndian.PutUint32(payload[4:8], track.TrackID)
	return writeFullyFormedIsoBmffBox("tfhd", payload)
}

// buildTrackFragmentBaseMediaDecodeTimeBox constructs the "tfdt" (Track
// Fragment Base Media Decode Time) box (version 1, 64-bit) recording where
// on the media timeline this fragment's samples begin.
func buildTrackFragmentBaseMediaDecodeTimeBox(track TrackDescriptor) []byte {
	payload := make([]byte, 12)
	payload[0] = 1 // version 1 => 64-bit baseMediaDecodeTime
	binary.BigEndian.PutUint64(payload[4:12], track.BaseMediaDecodeTimeInTimescaleUnits)
	return writeFullyFormedIsoBmffBox("tfdt", payload)
}

// trackFragmentRunFlagDataOffsetPresent marks a trun as carrying an
// explicit data_offset field, which this muxer always sets so the sample
// bytes' position inside the following mdat is unambiguous.
const trackFragmentRunFlagDataOffsetPresent = 0x000001

// buildTrackFragmentRunBox constructs one "trun" (Track Fragment Run) box
// declaring a single logical sample (see package doc: no real per-sample
// tables) of length len(track.SamplePayload), with dataOffsetFromMoofStart
// pointing at that sample's first byte relative to the start of the
// enclosing moof box, per the default-base-is-moof convention declared in
// tfhd.
func buildTrackFragmentRunBox(track TrackDescriptor, dataOffsetFromMoofStart int32) []byte {
	payload := make([]byte, 12)
	payload[1] = byte(trackFragmentRunFlagDataOffsetPresent >> 16)
	payload[2] = byte(trackFragmentRunFlagDataOffsetPresent >> 8)
	payload[3] = byte(trackFragmentRunFlagDataOffsetPresent)
	binary.BigEndian.PutUint32(payload[4:8], 1) // sample_count = 1 (single logical sample)
	binary.BigEndian.PutUint32(payload[8:12], uint32(int32(dataOffsetFromMoofStart)))
	return writeFullyFormedIsoBmffBox("trun", payload)
}

// buildTrackFragmentBox constructs one "traf" box (tfhd + tfdt + trun) for
// the given track, given that track's sample data offset relative to the
// start of the moof box it will live in.
func buildTrackFragmentBox(track TrackDescriptor, dataOffsetFromMoofStart int32) []byte {
	tfhd := buildTrackFragmentHeaderBox(track)
	tfdt := buildTrackFragmentBaseMediaDecodeTimeBox(track)
	trun := buildTrackFragmentRunBox(track, dataOffsetFromMoofStart)
	return writeFullyFormedIsoBmffBox("traf", concatenateChildBoxes(tfhd, tfdt, trun))
}

// isoBmffBoxHeaderSizeInBytes is the fixed 8-byte size+type header every
// box in this package uses (32-bit size field, no largesize/extended
// type).
const isoBmffBoxHeaderSizeInBytes = 8

// BuildMovieFragmentAndMediaDataBoxes constructs the "moof" + "mdat" box
// pair for one packaged chunk covering the given tracks, computing each
// track's trun data_offset so it correctly points at that track's sample
// bytes within the mdat that immediately follows the moof, per the
// default-base-is-moof convention. Returns the two serialized boxes
// separately (callers concatenate moof then mdat into the response body)
// so tests can independently verify each box's structure.
func BuildMovieFragmentAndMediaDataBoxes(fragmentSequenceNumber uint32, tracks []TrackDescriptor) (moofBox []byte, mdatBox []byte, err error) {
	if len(tracks) == 0 {
		return nil, nil, fmt.Errorf("fmp4: cannot build a movie fragment with zero tracks")
	}

	mfhd := buildMovieFragmentHeaderBox(fragmentSequenceNumber)

	// First pass: compute the eventual moof size so trun data_offsets
	// (which are relative to the start of moof) can be resolved before
	// the moof box is actually serialized. This mirrors how a real
	// muxer must size traf boxes before it can fill in data_offset.
	trafSizes := make([]int, len(tracks))
	runningMoofPayloadSize := len(mfhd)
	for i, track := range tracks {
		// Placeholder offset (0) only affects trun's own fixed-size
		// payload, not its length, so it is safe to size traf boxes
		// before final offsets are known.
		traf := buildTrackFragmentBox(track, 0)
		trafSizes[i] = len(traf)
		runningMoofPayloadSize += len(traf)
	}
	moofTotalSize := isoBmffBoxHeaderSizeInBytes + runningMoofPayloadSize

	// Second pass: each track's sample bytes are laid out consecutively
	// in mdat in track order, immediately after mdat's own 8-byte
	// header; data_offset is relative to the start of moof, so it is
	// moofTotalSize (skip the whole moof) + mdat's 8-byte header + the
	// running sum of any earlier tracks' sample bytes in this mdat.
	trafs := make([][]byte, len(tracks))
	runningSampleByteOffset := 0
	for i, track := range tracks {
		dataOffset := int32(moofTotalSize + isoBmffBoxHeaderSizeInBytes + runningSampleByteOffset)
		trafs[i] = buildTrackFragmentBox(track, dataOffset)
		runningSampleByteOffset += len(track.SamplePayload)
	}

	moofBox = writeFullyFormedIsoBmffBox("moof", concatenateChildBoxes(append([][]byte{mfhd}, trafs...)...))

	var mdatPayload bytes.Buffer
	for _, track := range tracks {
		mdatPayload.Write(track.SamplePayload)
	}
	mdatBox = writeFullyFormedIsoBmffBox("mdat", mdatPayload.Bytes())

	return moofBox, mdatBox, nil
}

// PackagedFmp4Chunk is the fully-assembled response body for one
// JIT-packaged fMP4 chunk request: an initialization segment (ftyp+moov,
// only needed once per rendition switch by a real player, but included
// with every chunk here for simplicity) followed by the fragment
// (moof+mdat) carrying this specific chunk's media bytes.
type PackagedFmp4Chunk struct {
	// InitializationSegment holds the concatenated ftyp+moov bytes.
	InitializationSegment []byte
	// MovieFragment holds the concatenated moof+mdat bytes for this
	// chunk.
	MovieFragment []byte
}

// FullPayload concatenates the initialization segment and movie fragment
// into the single byte stream an HTTP response body would carry.
func (chunk PackagedFmp4Chunk) FullPayload() []byte {
	return concatenateChildBoxes(chunk.InitializationSegment, chunk.MovieFragment)
}

// PackageElementaryStreamChunkAsFragmentedMp4 is the JIT packaging entry
// point: given the raw video and audio elementary-stream bytes for one
// chunk (as would be fetched from separate object-storage keys per this
// service's responsibility statement), it builds a complete, structurally
// valid fragmented MP4 byte stream wrapping them, using track ID 1 for
// video and track ID 2 for audio.
func PackageElementaryStreamChunkAsFragmentedMp4(
	fragmentSequenceNumber uint32,
	timescaleUnitsPerSecond uint32,
	baseMediaDecodeTimeInTimescaleUnits uint64,
	videoTrackBytes []byte,
	audioTrackBytes []byte,
) (PackagedFmp4Chunk, error) {
	tracks := []TrackDescriptor{
		{
			TrackID:                             1,
			TimescaleUnitsPerSecond:             timescaleUnitsPerSecond,
			SamplePayload:                       videoTrackBytes,
			BaseMediaDecodeTimeInTimescaleUnits: baseMediaDecodeTimeInTimescaleUnits,
		},
		{
			TrackID:                             2,
			TimescaleUnitsPerSecond:             timescaleUnitsPerSecond,
			SamplePayload:                       audioTrackBytes,
			BaseMediaDecodeTimeInTimescaleUnits: baseMediaDecodeTimeInTimescaleUnits,
		},
	}

	ftyp := BuildFileTypeBox()
	moov := BuildMovieBox(tracks)
	moof, mdat, err := BuildMovieFragmentAndMediaDataBoxes(fragmentSequenceNumber, tracks)
	if err != nil {
		return PackagedFmp4Chunk{}, err
	}

	return PackagedFmp4Chunk{
		InitializationSegment: concatenateChildBoxes(ftyp, moov),
		MovieFragment:         concatenateChildBoxes(moof, mdat),
	}, nil
}

// ParsedBoxHeader is the decoded size+type header of one top-level box, as
// returned by WalkTopLevelBoxes for structural verification in tests.
type ParsedBoxHeader struct {
	// Type is the 4-character box type code.
	Type string
	// TotalSize is this box's full size (header + payload) in bytes, as
	// read from its own size field.
	TotalSize uint32
	// PayloadOffset is the byte offset, within the buffer passed to
	// WalkTopLevelBoxes, at which this box's payload (post-header)
	// begins.
	PayloadOffset int
}

// WalkTopLevelBoxes parses a byte buffer as a sequence of sibling
// ISO-BMFF boxes and returns each one's decoded header, without
// recursing into children. It is used by the test suite to assert on
// size-field correctness and box ordering; a production tool would use a
// real ISO-BMFF parsing library, but this hand-rolled walker is sufficient
// to validate the byte layout this package itself produces.
func WalkTopLevelBoxes(buf []byte) ([]ParsedBoxHeader, error) {
	var headers []ParsedBoxHeader
	offset := 0
	for offset < len(buf) {
		if len(buf)-offset < isoBmffBoxHeaderSizeInBytes {
			return nil, fmt.Errorf("fmp4: truncated box header at offset %d", offset)
		}
		size := binary.BigEndian.Uint32(buf[offset : offset+4])
		boxType := string(buf[offset+4 : offset+8])
		if size < isoBmffBoxHeaderSizeInBytes {
			return nil, fmt.Errorf("fmp4: box %q at offset %d has implausible size %d", boxType, offset, size)
		}
		if offset+int(size) > len(buf) {
			return nil, fmt.Errorf("fmp4: box %q at offset %d claims size %d beyond buffer end", boxType, offset, size)
		}
		headers = append(headers, ParsedBoxHeader{
			Type:          boxType,
			TotalSize:     size,
			PayloadOffset: offset + isoBmffBoxHeaderSizeInBytes,
		})
		offset += int(size)
	}
	return headers, nil
}
