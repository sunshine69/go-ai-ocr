package main

import (
	"bytes"
	"testing"
)

func TestUnwrapMarkdown(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "strips markdown fence",
			in:   "```markdown\n# Hello\n\nSome **body**.\n```\n",
			want: "# Hello\n\nSome **body**.",
		},
		{
			name: "case-insensitive language tag",
			in:   "```MARKDOWN\nraw text\n```\n",
			want: "raw text",
		},
		{
			name: "leading whitespace before fence",
			in:   "   ```markdown\nx\n```\n",
			want: "x",
		},
		{
			name: "no fence passes through untouched",
			in:   "# Hello\n\nSome **body**.\n",
			want: "# Hello\n\nSome **body**.",
		},
		{
			name: "non-markdown fence is not touched",
			in:   "```go\npackage main\n```\n",
			want: "```go\npackage main\n```",
		},
		{
			name: "open fence but no close leaves input",
			in:   "```markdown\nx\n",
			want: "```markdown\nx",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(unwrapMarkdown([]byte(tc.in)))
			if got != tc.want {
				t.Fatalf("unwrapMarkdown(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestUnwrapMarkdownMatches(t *testing.T) {
	// Sanity check that a fence we expect to strip is handled and that
	// bytes.Equal holds for an identical passthrough input.
	in := []byte("```markdown\nkeep\n```\n")
	got := unwrapMarkdown(in)
	if !bytes.Equal(got, []byte("keep")) {
		t.Fatalf("got %q; want %q", got, []byte("keep"))
	}

	plain := []byte("keep this")
	if !bytes.Equal(unwrapMarkdown(plain), plain) {
		t.Fatalf("passthrough changed input")
	}
}
