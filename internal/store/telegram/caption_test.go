package telegram

import "testing"

func TestEncodeDecodeCaption(t *testing.T) {
	cap := EncodeCaption("soul.md", "soul", map[string]string{"name": "soul"})
	rec, ok := DecodeCaption(cap)
	if !ok {
		t.Fatalf("decode failed for: %q", cap)
	}
	if rec.Kind != "soul" {
		t.Fatalf("kind=%q", rec.Kind)
	}
	if rec.Fields["name"] != "soul" {
		t.Fatalf("fields=%v", rec.Fields)
	}
}

func TestEscapeRoundTrip(t *testing.T) {
	raw := "Q3; 100% #final"
	cap := EncodeCaption("file", "memory", map[string]string{"name": raw})
	rec, ok := DecodeCaption(cap)
	if !ok {
		t.Fatal("decode failed")
	}
	if rec.Fields["name"] != raw {
		t.Fatalf("got %q want %q", rec.Fields["name"], raw)
	}
}
