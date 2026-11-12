package exif

import (
	"bytes"
	"testing"
)

func TestOrientation(t *testing.T) {
	be := []byte("MM\x00*\x00\x00\x00\x08\x00\x02\x01\x0f\x00\x02\x00\x00\x00\x04ABC\x00\x01\x12\x00\x03\x00\x00\x00\x01\x00\x06\x00\x00\x00\x00\x00\x00")
	le := []byte("II*\x00\x08\x00\x00\x00\x01\x00\x12\x01\x03\x00\x01\x00\x00\x00\x08\x00\x00\x00\x00\x00\x00\x00")
	if o := Orientation(be); o != 6 {
		t.Errorf("big endian: %d", o)
	}
	if o := Orientation(le); o != 8 {
		t.Errorf("little endian: %d", o)
	}
	r := ResetOrientation(be)
	if Orientation(r) != 1 || Orientation(be) != 6 || len(r) != len(be) {
		t.Error("reset changed the input or failed")
	}
	if !bytes.Equal(ResetOrientation(r), r) {
		t.Error("reset of oriented data changed it")
	}
	for _, bad := range [][]byte{nil, []byte("MM\x00*"), []byte("MM\x00*\xff\xff\xff\xff"), be[:20]} {
		if Orientation(bad) != 0 {
			t.Errorf("%q: orientation found", bad)
		}
		ResetOrientation(bad)
	}
}

func FuzzOrientation(f *testing.F) {
	f.Add([]byte("MM\x00*\x00\x00\x00\x08\x00\x02\x01\x0f\x00\x02\x00\x00\x00\x04ABC\x00\x01\x12\x00\x03\x00\x00\x00\x01\x00\x06\x00\x00\x00\x00\x00\x00"))
	f.Add([]byte("II*\x00\x08\x00\x00\x00\x01\x00\x12\x01\x03\x00\x01\x00\x00\x00\x08\x00\x00\x00\x00\x00\x00\x00"))
	f.Fuzz(func(t *testing.T, data []byte) {
		o := Orientation(data)
		r := ResetOrientation(data)
		if o != 0 && Orientation(r) != 1 {
			t.Fatalf("reset failed: %d", Orientation(r))
		}
	})
}
