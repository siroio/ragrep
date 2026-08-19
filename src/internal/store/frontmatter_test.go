package store

import (
	"reflect"
	"testing"
)

func TestParseTags(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{"inline", "---\ntags: [Design, API]\n---\nbody", []string{"design", "api"}},
		{"inline no brackets", "---\ntags: design, api\n---\nbody", []string{"design", "api"}},
		{"block list", "---\ntags:\n  - a\n  - B\n---\nbody", []string{"a", "b"}},
		{"quotes stripped", "---\ntags: [\"a\", 'b']\n---\n", []string{"a", "b"}},
		{"dedup case-insensitive", "---\ntags: [a, A, a]\n---\n", []string{"a"}},
		{"crlf", "---\r\ntags: [a]\r\n---\r\nbody", []string{"a"}},
		{"no frontmatter", "body text", nil},
		{"unclosed frontmatter", "---\ntags: [a]\nbody", nil},
		{"no tags key", "---\ntitle: x\n---\nbody", nil},
		{"empty inline", "---\ntags: []\n---\n", nil},
		{"tags key then non-list line", "---\ntags:\ntitle: x\n---\n", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseTags(c.content)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("ParseTags(%q) = %v, want %v", c.content, got, c.want)
			}
		})
	}
}
