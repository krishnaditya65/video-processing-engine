package fmp4

import (
	"encoding/binary"
	"testing"
)

// TestBuildFileTypeBoxHasCorrectSizeAndType verifies the ftyp box's size
// field equals its actual byte length and that its type code is correct.
func TestBuildFileTypeBoxHasCorrectSizeAndType(t *testing.T) {
	ftyp := BuildFileTypeBox()
	headers, err := WalkTopLevelBoxes(ftyp)
	if err != nil {
		t.Fatalf("WalkTopLevelBoxes returned error: %v", err)
	}
	if len(headers) != 1 {
		t.Fatalf("expected exactly 1 top-level box, got %d", len(headers))
	}
	if headers[0].Type != "ftyp" {
		t.Errorf("expected box type %q, got %q", "ftyp", headers[0].Type)
	}
	if int(headers[0].TotalSize) != len(ftyp) {
		t.Errorf("ftyp size field = %d, actual byte length = %d", headers[0].TotalSize, len(ftyp))
	}
}

// TestBuildMovieBoxNestingIsByteExact verifies moov's declared size field
// matches its true length, and that its child trak/mvex boxes' sizes sum
// (plus mvhd) exactly to moov's payload length, proving the box tree is
// nested with byte-exact size fields rather than merely superficially
// box-shaped.
func TestBuildMovieBoxNestingIsByteExact(t *testing.T) {
	tracks := []TrackDescriptor{
		{TrackID: 1, TimescaleUnitsPerSecond: 90000, SamplePayload: []byte("video-bytes")},
		{TrackID: 2, TimescaleUnitsPerSecond: 90000, SamplePayload: []byte("audio-bytes")},
	}
	moov := BuildMovieBox(tracks)

	topLevel, err := WalkTopLevelBoxes(moov)
	if err != nil {
		t.Fatalf("WalkTopLevelBoxes(moov) error: %v", err)
	}
	if len(topLevel) != 1 || topLevel[0].Type != "moov" {
		t.Fatalf("expected a single top-level moov box, got %+v", topLevel)
	}
	if int(topLevel[0].TotalSize) != len(moov) {
		t.Errorf("moov size field = %d, actual byte length = %d", topLevel[0].TotalSize, len(moov))
	}

	// Walk moov's own payload as a sequence of children: mvhd, trak, trak, mvex.
	children, err := WalkTopLevelBoxes(moov[8:])
	if err != nil {
		t.Fatalf("WalkTopLevelBoxes(moov payload) error: %v", err)
	}
	wantTypes := []string{"mvhd", "trak", "trak", "mvex"}
	if len(children) != len(wantTypes) {
		t.Fatalf("expected %d children in moov, got %d (%+v)", len(wantTypes), len(children), children)
	}
	for i, want := range wantTypes {
		if children[i].Type != want {
			t.Errorf("child %d: expected type %q, got %q", i, want, children[i].Type)
		}
	}

	// Sum of child sizes + 8-byte moov header must equal moov's own declared size.
	sum := 8
	for _, c := range children {
		sum += int(c.TotalSize)
	}
	if sum != int(topLevel[0].TotalSize) {
		t.Errorf("sum of moov's header+children sizes = %d, but moov declares size %d", sum, topLevel[0].TotalSize)
	}

	// Each trak must itself nest tkhd + mdia with byte-exact sizes. The
	// first trak child immediately follows mvhd within moov's payload.
	firstTrakStart := 8 + int(children[0].TotalSize)
	trakBytes := moov[firstTrakStart:]
	trakHeaders, err := WalkTopLevelBoxes(trakBytes[:children[1].TotalSize])
	if err != nil {
		t.Fatalf("WalkTopLevelBoxes(trak) error: %v", err)
	}
	if len(trakHeaders) != 1 || trakHeaders[0].Type != "trak" {
		t.Fatalf("expected single trak box, got %+v", trakHeaders)
	}
	trakChildren, err := WalkTopLevelBoxes(trakBytes[8:trakHeaders[0].TotalSize])
	if err != nil {
		t.Fatalf("WalkTopLevelBoxes(trak payload) error: %v", err)
	}
	wantTrakChildren := []string{"tkhd", "mdia"}
	if len(trakChildren) != len(wantTrakChildren) {
		t.Fatalf("expected %d trak children, got %d (%+v)", len(wantTrakChildren), len(trakChildren), trakChildren)
	}
	for i, want := range wantTrakChildren {
		if trakChildren[i].Type != want {
			t.Errorf("trak child %d: expected type %q, got %q", i, want, trakChildren[i].Type)
		}
	}
}

// TestBuildMovieFragmentAndMediaDataBoxesTrunDataOffsetIsCorrect verifies
// that the moof/mdat pair produced for a chunk carries trun data_offset
// values that correctly point at each track's sample bytes inside the
// following mdat, per the default-base-is-moof convention: this is the
// core correctness property a real player relies on to locate sample
// bytes.
func TestBuildMovieFragmentAndMediaDataBoxesTrunDataOffsetIsCorrect(t *testing.T) {
	tracks := []TrackDescriptor{
		{TrackID: 1, TimescaleUnitsPerSecond: 90000, SamplePayload: []byte("VIDEOPAYLOADBYTES"), BaseMediaDecodeTimeInTimescaleUnits: 180000},
		{TrackID: 2, TimescaleUnitsPerSecond: 90000, SamplePayload: []byte("AUDIOPAYLOAD"), BaseMediaDecodeTimeInTimescaleUnits: 180000},
	}

	moof, mdat, err := BuildMovieFragmentAndMediaDataBoxes(7, tracks)
	if err != nil {
		t.Fatalf("BuildMovieFragmentAndMediaDataBoxes error: %v", err)
	}

	moofHeaders, err := WalkTopLevelBoxes(moof)
	if err != nil {
		t.Fatalf("WalkTopLevelBoxes(moof) error: %v", err)
	}
	if len(moofHeaders) != 1 || moofHeaders[0].Type != "moof" {
		t.Fatalf("expected single moof box, got %+v", moofHeaders)
	}
	if int(moofHeaders[0].TotalSize) != len(moof) {
		t.Errorf("moof size field = %d, actual byte length = %d", moofHeaders[0].TotalSize, len(moof))
	}

	mdatHeaders, err := WalkTopLevelBoxes(mdat)
	if err != nil {
		t.Fatalf("WalkTopLevelBoxes(mdat) error: %v", err)
	}
	if len(mdatHeaders) != 1 || mdatHeaders[0].Type != "mdat" {
		t.Fatalf("expected single mdat box, got %+v", mdatHeaders)
	}
	wantMdatSize := 8 + len(tracks[0].SamplePayload) + len(tracks[1].SamplePayload)
	if int(mdatHeaders[0].TotalSize) != wantMdatSize {
		t.Errorf("mdat size = %d, want %d", mdatHeaders[0].TotalSize, wantMdatSize)
	}

	// moof's children: mfhd, traf, traf.
	moofChildren, err := WalkTopLevelBoxes(moof[8:])
	if err != nil {
		t.Fatalf("WalkTopLevelBoxes(moof payload) error: %v", err)
	}
	if len(moofChildren) != 3 {
		t.Fatalf("expected 3 moof children (mfhd + 2 traf), got %d", len(moofChildren))
	}

	// Concatenated moof+mdat forms the full response payload; data_offset
	// in each trun is relative to the start of moof, so
	// fullPayload[dataOffset:] must begin with that track's sample bytes.
	fullPayload := append(append([]byte{}, moof...), mdat...)

	trafOffsetsWithinMoof := []int{}
	cursor := 8 + int(moofChildren[0].TotalSize) // skip moof header + mfhd
	for _, c := range moofChildren[1:] {         // remaining children are traf boxes
		trafOffsetsWithinMoof = append(trafOffsetsWithinMoof, cursor)
		cursor += int(c.TotalSize)
	}

	for i, track := range tracks {
		trafStart := trafOffsetsWithinMoof[i]
		trafBuf := moof[trafStart:]
		trafHeaders, err := WalkTopLevelBoxes(trafBuf[:moofChildren[i+1].TotalSize])
		if err != nil {
			t.Fatalf("WalkTopLevelBoxes(traf) error: %v", err)
		}
		if len(trafHeaders) != 1 || trafHeaders[0].Type != "traf" {
			t.Fatalf("expected single traf box, got %+v", trafHeaders)
		}
		trafChildren, err := WalkTopLevelBoxes(trafBuf[8:trafHeaders[0].TotalSize])
		if err != nil {
			t.Fatalf("WalkTopLevelBoxes(traf payload) error: %v", err)
		}
		var trunPayload []byte
		for _, tc := range trafChildren {
			if tc.Type == "trun" {
				// tc's offsets are relative to trafBuf[8:] (the slice
				// passed into WalkTopLevelBoxes above), so add back the
				// 8-byte traf header to get an offset within trafBuf.
				trunPayloadStart := 8 + tc.PayloadOffset
				trunPayload = trafBuf[trunPayloadStart : trunPayloadStart+int(tc.TotalSize)-8]
			}
		}
		if trunPayload == nil {
			t.Fatalf("track %d: trun box not found inside traf", i)
		}
		dataOffset := int32(binary.BigEndian.Uint32(trunPayload[8:12]))

		if int(dataOffset) < 0 || int(dataOffset) > len(fullPayload) {
			t.Fatalf("track %d: data_offset %d out of range of full payload (len %d)", i, dataOffset, len(fullPayload))
		}
		gotSampleBytes := fullPayload[int(dataOffset) : int(dataOffset)+len(track.SamplePayload)]
		if string(gotSampleBytes) != string(track.SamplePayload) {
			t.Errorf("track %d: data_offset %d does not point at expected sample bytes: got %q, want %q",
				i, dataOffset, gotSampleBytes, track.SamplePayload)
		}
	}
}

// TestBuildMovieFragmentAndMediaDataBoxesRejectsZeroTracks verifies the
// muxer refuses to build a fragment with no tracks rather than silently
// producing a malformed moof/mdat pair.
func TestBuildMovieFragmentAndMediaDataBoxesRejectsZeroTracks(t *testing.T) {
	_, _, err := BuildMovieFragmentAndMediaDataBoxes(1, nil)
	if err == nil {
		t.Fatal("expected an error for zero tracks, got nil")
	}
}

// TestPackageElementaryStreamChunkAsFragmentedMp4ProducesValidBoxTree
// exercises the full JIT packaging entry point end-to-end and verifies
// the resulting byte stream begins with ftyp, then moov, then moof, then
// mdat, each with a byte-correct size field.
func TestPackageElementaryStreamChunkAsFragmentedMp4ProducesValidBoxTree(t *testing.T) {
	chunk, err := PackageElementaryStreamChunkAsFragmentedMp4(3, 90000, 270000, []byte("video-chunk-3"), []byte("audio-chunk-3"))
	if err != nil {
		t.Fatalf("PackageElementaryStreamChunkAsFragmentedMp4 error: %v", err)
	}

	full := chunk.FullPayload()
	headers, err := WalkTopLevelBoxes(full)
	if err != nil {
		t.Fatalf("WalkTopLevelBoxes(full payload) error: %v", err)
	}
	wantOrder := []string{"ftyp", "moov", "moof", "mdat"}
	if len(headers) != len(wantOrder) {
		t.Fatalf("expected %d top-level boxes, got %d: %+v", len(wantOrder), len(headers), headers)
	}
	for i, want := range wantOrder {
		if headers[i].Type != want {
			t.Errorf("box %d: expected type %q, got %q", i, want, headers[i].Type)
		}
	}

	runningOffset := 0
	for _, h := range headers {
		if h.PayloadOffset-8 != runningOffset {
			t.Errorf("box %q expected to start at offset %d, computed start %d", h.Type, runningOffset, h.PayloadOffset-8)
		}
		runningOffset += int(h.TotalSize)
	}
	if runningOffset != len(full) {
		t.Errorf("sum of top-level box sizes = %d, but full payload length = %d", runningOffset, len(full))
	}
}

// TestWalkTopLevelBoxesRejectsTruncatedBuffer verifies the parser used by
// the test suite itself fails closed on malformed/truncated input rather
// than reading out of bounds.
func TestWalkTopLevelBoxesRejectsTruncatedBuffer(t *testing.T) {
	_, err := WalkTopLevelBoxes([]byte{0, 0})
	if err == nil {
		t.Fatal("expected an error for a truncated box header, got nil")
	}
}
