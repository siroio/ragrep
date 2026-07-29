package store

import "testing"

func TestSplitParas(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []Para
	}{
		{"basic", "a\nb\n\nc\n", []Para{
			{Seq: 0, StartLine: 1, EndLine: 2, Text: "a\nb"},
			{Seq: 1, StartLine: 4, EndLine: 4, Text: "c"},
		}},
		{"consecutive blanks", "a\n\n\n\nb", []Para{
			{Seq: 0, StartLine: 1, EndLine: 1, Text: "a"},
			{Seq: 1, StartLine: 5, EndLine: 5, Text: "b"},
		}},
		{"no trailing newline", "a", []Para{
			{Seq: 0, StartLine: 1, EndLine: 1, Text: "a"},
		}},
		{"crlf", "a\r\nb\r\n\r\nc", []Para{
			{Seq: 0, StartLine: 1, EndLine: 2, Text: "a\nb"},
			{Seq: 1, StartLine: 4, EndLine: 4, Text: "c"},
		}},
		{"whitespace-only line is blank", "a\n \t\nb", []Para{
			{Seq: 0, StartLine: 1, EndLine: 1, Text: "a"},
			{Seq: 1, StartLine: 3, EndLine: 3, Text: "b"},
		}},
		{"empty input", "", nil},
		{"headings", "# Auth\n\nintro text\n\n## Errors\n\nhandling text", []Para{
			{Seq: 0, StartLine: 1, EndLine: 1, Text: "# Auth", Heading: "Auth"},
			{Seq: 1, StartLine: 3, EndLine: 3, Text: "intro text", Heading: "Auth"},
			{Seq: 2, StartLine: 5, EndLine: 5, Text: "## Errors", Heading: "Auth > Errors"},
			{Seq: 3, StartLine: 7, EndLine: 7, Text: "handling text", Heading: "Auth > Errors"},
		}},
		{"no headings", "a\n\nb", []Para{
			{Seq: 0, StartLine: 1, EndLine: 1, Text: "a"},
			{Seq: 1, StartLine: 3, EndLine: 3, Text: "b"},
		}},
		{"heading level jump", "# A\n\n### B", []Para{
			{Seq: 0, StartLine: 1, EndLine: 1, Text: "# A", Heading: "A"},
			{Seq: 1, StartLine: 3, EndLine: 3, Text: "### B", Heading: "A > B"},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := splitParas(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("got %d paras, want %d: %+v", len(got), len(c.want), got)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("para %d: got %+v, want %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}
