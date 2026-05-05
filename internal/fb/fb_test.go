package fb

import (
	"testing"
)

func TestBufCopy(t *testing.T) {
	data := make([]byte, 640*480*4)
	for i := range data {
		data[i] = byte(i)
	}
	b := &Buf{
		Data:   data,
		Width:  640,
		Height: 480,
		Stride: 640 * 4,
	}
	copy := b.Copy()
	if len(copy) != len(data) {
		t.Fatalf("Copy length %d != original %d", len(copy), len(data))
	}
	for i := range data {
		if copy[i] != data[i] {
			t.Fatalf("Copy mismatch at byte %d: got %d, want %d", i, copy[i], data[i])
		}
	}
	// Verify Copy is independent (mutate original, copy unchanged)
	data[0] = 0xFF
	if copy[0] == 0xFF {
		t.Error("Copy is not independent of original buffer")
	}
}

func TestBufClose(t *testing.T) {
	data := make([]byte, 100)
	b := &Buf{
		Data:   data,
		Width:  10,
		Height: 10,
		Stride: 10,
	}
	// Close should not panic
	b.Close()
	// Double close should not panic
	b.Close()
}

func TestBufZeroSized(t *testing.T) {
	b := &Buf{}
	copy := b.Copy()
	if len(copy) != 0 {
		t.Errorf("Expected empty copy, got %d bytes", len(copy))
	}
	b.Close() // should not panic
}

func TestBufStride(t *testing.T) {
	b := &Buf{
		Data:   make([]byte, 1920*1080*4),
		Width:  1920,
		Height: 1080,
		Stride: 1920 * 4,
	}
	if b.Width*b.Height*b.Stride/b.Width != len(b.Data) {
		t.Error("Stride and dimensions inconsistent with data size")
	}
}
