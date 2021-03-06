package cbor

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"unicode/utf8"
)

// Encoded returns the array of bytes that prefix a CBOR item of type t with
// either value or length "value", depending on the type.
func Encoded(t Type, value int) []byte {
	var buffer bytes.Buffer
	item := New(&buffer)
	item.encodeInt(t, value)
	item.Finish()
	return buffer.Bytes()
}

// EncodedFixedLen is like Encoded(), but always uses the size-byte encoding of
// value.
func EncodedFixedLen(size int, t Type, value int) []byte {
	var buffer bytes.Buffer
	item := New(&buffer)
	item.encodeSizedInt64(size, t, uint64(value))
	item.Finish()
	return buffer.Bytes()
}

type countingWriter struct {
	w *bufio.Writer
	// bytes counts the total number of bytes written to w.
	bytes uint64
}

func newCountingWriter(to io.Writer) *countingWriter {
	return &countingWriter{w: bufio.NewWriter(to)}
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	nn, err := cw.w.Write(p)
	cw.bytes += uint64(nn)
	return nn, err
}

func (cw *countingWriter) Flush() error {
	return cw.w.Flush()
}

type item struct {
	*countingWriter
	// nil for the root.
	parent *compoundItem
	// nil for the leaf-most active child.
	activeChild *item
	// The byte offset within the buffer at which this item starts.
	startOffset uint64
}
type compoundItem struct {
	item
	// How many elements have been added to this item so far.
	elements uint64
}

type TopLevel struct {
	compoundItem
}

// New returns a new CBOR top-level item for the caller to write into. Call
// .Finish() when serialization is complete.
func New(to io.Writer) *TopLevel {
	result := &TopLevel{}
	result.countingWriter = newCountingWriter(to)
	return result
}

// Finish checks for well-formed-ness and flushes the serialization to the
// Writer passed to New.
func (c *TopLevel) Finish() error {
	if c.activeChild != nil {
		panic(fmt.Sprintf("Must finish child %v before its parent %v.",
			c.activeChild, c))
	}
	err := c.Flush()
	c.countingWriter = nil
	return err
}

func encodedSize(i uint64) int {
	if i < 24 {
		return 0
	}
	if i < (1 << 8) {
		return 1
	}
	if i < (1 << 16) {
		return 2
	}
	if i < (1 << 32) {
		return 4
	}
	return 8
}

func (ci *compoundItem) encodeInt(t Type, i int) {
	ci.encodeInt64(t, uint64(i))
}
func (ci *compoundItem) encodeInt64(t Type, i uint64) {
	ci.encodeSizedInt64(encodedSize(i), t, i)
}
func (ci *compoundItem) encodeSizedInt64(size int, t Type, i uint64) {
	ci.elements++

	switch size {
	case 0:
		ci.Write([]byte{byte(t) | byte(i)})
	case 1:
		ci.Write([]byte{byte(t) | 24, byte(i)})
	case 2:
		ci.Write([]byte{byte(t) | 25, byte(i >> 8), byte(i)})
	case 4:
		ci.Write([]byte{byte(t) | 26,
			byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)})
	case 8:
		ci.Write([]byte{byte(t) | 27,
			byte(i >> 56), byte(i >> 48), byte(i >> 40), byte(i >> 32),
			byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)})
	default:
		panic(fmt.Sprintf("Unexpected CBOR item size: %v", size))
	}
}

func (ci *compoundItem) AppendUint64(i uint64) {
	ci.encodeInt64(TypePosInt, i)
}

// AppendFixedSizeUint64 always uses the 8-byte encoding for this uint64.
func (ci *compoundItem) AppendFixedSizeUint64(i uint64) {
	ci.encodeSizedInt64(8, TypePosInt, i)
}

func (ci *compoundItem) AppendInt64(i int64) {
	if i < 0 {
		ci.encodeInt64(TypeNegInt, uint64(-1-i))
	} else {
		ci.encodeInt64(TypePosInt, uint64(i))
	}
}

func (ci *compoundItem) AppendBytes(bs []byte) {
	ci.encodeInt(TypeBytes, len(bs))
	ci.Write(bs)
}

type BytesWriter struct {
	item
	remainingSize int64
}

func (bw *BytesWriter) Write(p []byte) (int, error) {
	n, err := bw.item.Write(p)
	bw.remainingSize -= int64(n)
	if bw.remainingSize < 0 {
		panic(fmt.Sprintf("Wrote too many bytes to a fixed-size field, by %v.",
			-bw.remainingSize))
	}
	return n, err
}

func (bw *BytesWriter) Finish() {
	if bw.remainingSize != 0 {
		panic(fmt.Sprintf("Wrote too few bytes to a fixed-size field, by %v.",
			bw.remainingSize))
	}
	bw.parent.activeChild = nil
	bw.countingWriter = nil
}

// AppendBytesWriter lets the caller write n bytes into a CBOR bytestring, with
// its size set by n. This doesn't necessarily materialize the whole thing in
// memory at once.
func (ci *compoundItem) AppendBytesWriter(n int64) *BytesWriter {
	ci.encodeInt64(TypeBytes, uint64(n))
	bw := &BytesWriter{
		item: item{
			countingWriter: ci.countingWriter,
			parent:         ci,
			startOffset:    ci.bytes,
		},
		remainingSize: n,
	}
	ci.activeChild = &bw.item
	return bw
}

// AppendUTF8 checks that bs holds valid UTF-8.
func (ci *compoundItem) AppendUTF8(bs []byte) {
	if !utf8.Valid(bs) {
		panic(fmt.Sprintf("Invalid UTF-8 in %q.", bs))
	}
	ci.encodeInt(TypeText, len(bs))
	ci.Write(bs)
}

func (ci *compoundItem) AppendUTF8S(str string) {
	ci.AppendUTF8([]byte(str))
}

// ByteLenSoFar returns the number of bytes from the start of item's encoding.
func (ci *compoundItem) ByteLenSoFar() uint64 {
	return ci.bytes - ci.startOffset
}

func (ci *compoundItem) AppendSerializedItem(r io.Reader) {
	ci.elements++
	io.Copy(ci, r)
}

type Array struct {
	compoundItem
	expectedSize uint64
}

func (ci *compoundItem) AppendArray(expectedSize uint64) *Array {
	startOffset := ci.bytes
	ci.encodeInt64(TypeArray, expectedSize)
	a := &Array{
		compoundItem: compoundItem{
			item: item{
				countingWriter: ci.countingWriter,
				parent:         ci,
				startOffset:    startOffset,
			},
			elements: 0,
		},
		expectedSize: expectedSize,
	}
	ci.activeChild = &a.item
	return a
}

func (a *Array) Finish() {
	if a.activeChild != nil {
		panic(fmt.Sprintf("Must finish child %v before its parent %v.",
			a.activeChild, a))
	}
	if a.elements != a.expectedSize {
		panic(fmt.Sprintf("Array has size %v but was initialized with size %v",
			a.elements, a.expectedSize))
	}
	a.parent.activeChild = nil
	a.countingWriter = nil
}

type Map struct {
	compoundItem
	expectedSize uint64
}

func (ci *compoundItem) AppendMap(expectedSize uint64) *Map {
	startOffset := ci.bytes
	ci.encodeInt64(TypeMap, expectedSize)
	m := &Map{
		compoundItem: compoundItem{
			item: item{
				countingWriter: ci.countingWriter,
				parent:         ci,
				startOffset:    startOffset,
			},
			elements: 0,
		},
		expectedSize: expectedSize,
	}
	ci.activeChild = &m.item
	return m
}

func (m *Map) Finish() {
	if m.activeChild != nil {
		panic(fmt.Sprintf("Must finish child %v before its parent %v.",
			m.activeChild, m))
	}
	if m.elements%2 != 0 {
		panic("Map's last key is missing a value.")
	}
	if m.elements != m.expectedSize*2 {
		panic(fmt.Sprintf("Map has size %v but was initialized with size %v",
			m.elements/2, m.expectedSize))
	}
	m.parent.activeChild = nil
	m.countingWriter = nil
}
