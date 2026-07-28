package main

import "testing"

// -h/--help must print usage and exit 0, not read as a generic parse error
// (exit 1). Doesn't need the model: parsing fails before the embedder or DB
// are ever touched.
func TestHelpExitsZero(t *testing.T) {
	if code := run([]string{"search", "-h"}); code != 0 {
		t.Fatalf("search -h exit=%d, want 0", code)
	}
}

// getContent's --lines branch: happy path, invalid ranges, out-of-range
// start, clamping past EOF, and CRLF normalization. No model needed.
func TestGetContentLines(t *testing.T) {
	s := newTestStore(t)
	content := "l1\r\nl2\r\nl3\r\nl4\r\nl5"
	if _, err := s.UpsertDoc("a.txt", content, 1, fakeEmbed); err != nil {
		t.Fatal(err)
	}

	got, err := getContent(s, "a.txt", "2-3", -1, 0)
	if err != nil || got != "l2\nl3" {
		t.Fatalf("lines 2-3: got %q err=%v", got, err)
	}

	if _, err := getContent(s, "a.txt", "0-3", -1, 0); err == nil {
		t.Fatal("want error for a<1")
	}
	if _, err := getContent(s, "a.txt", "3-2", -1, 0); err == nil {
		t.Fatal("want error for b<a")
	}
	if _, err := getContent(s, "a.txt", "100-200", -1, 0); err != errNotFound {
		t.Fatalf("want errNotFound for a>len(lines), got %v", err)
	}

	got, err = getContent(s, "a.txt", "4-100", -1, 0)
	if err != nil || got != "l4\nl5" {
		t.Fatalf("clamp to EOF: got %q err=%v", got, err)
	}
}

// A panic escaping run() must surface as exit 1 (this CLI's generic error
// code), not Go's own panic exit code 2 -- which would collide with the
// "no hits / not found" contract.
func TestProtect(t *testing.T) {
	if code := protect(func() int { panic("boom") }); code != 1 {
		t.Fatalf("panic: got %d, want 1", code)
	}
	if code := protect(func() int { return 2 }); code != 2 {
		t.Fatalf("no panic: got %d, want 2", code)
	}
}
