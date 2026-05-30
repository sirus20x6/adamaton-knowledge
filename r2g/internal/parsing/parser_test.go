package parsing

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNormalizeCT(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "application/octet-stream"},
		{"text/markdown", "text/markdown"},
		{"TEXT/Markdown", "text/markdown"},
		{"text/plain; charset=utf-8", "text/plain"},
		{"  application/pdf  ", "application/pdf"},
		{"multipart/form-data; boundary=xyz", "multipart/form-data"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := normalizeCT(tc.in); got != tc.want {
				t.Errorf("normalizeCT(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSniff(t *testing.T) {
	cases := []struct {
		name     string
		body     []byte
		filename string
		want     string
	}{
		{"pdf magic", []byte("%PDF-1.7\nrest"), "x.bin", "application/pdf"},
		{"gzip magic", []byte{0x1f, 0x8b, 0x08, 0x00}, "x.bin", "application/gzip"},
		{"md by ext", []byte("hello"), "notes.MD", "text/markdown"},
		{"txt by ext", []byte("hello"), "notes.txt", "text/plain"},
		{"pdf by ext", []byte("hello"), "paper.pdf", "application/pdf"},
		{"tar by ext", []byte("hello"), "src.tar", "application/gzip"},
		{"tgz by ext", []byte("hello"), "src.tgz", "application/gzip"},
		{"gz by ext", []byte("hello"), "src.gz", "application/gzip"},
		{"bz2 by ext", []byte("hello"), "src.bz2", "application/gzip"},
		{"default markdown", []byte("hello"), "noext", "text/markdown"},
		{"empty body unknown ext", []byte{}, "", "text/markdown"},
		{"magic wins over ext", []byte("%PDF-x"), "thing.md", "application/pdf"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sniff(tc.body, tc.filename); got != tc.want {
				t.Errorf("sniff(%q, %q) = %q, want %q", tc.body, tc.filename, got, tc.want)
			}
		})
	}
}

// TestParse_TextPassthrough covers the in-process routes that need no
// subprocess.
func TestParse_TextPassthrough(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		filename    string
		body        string
		wantDocType string
	}{
		{"markdown", "text/markdown", "", "# Title", "md"},
		{"x-markdown", "text/x-markdown", "", "# Title", "md"},
		{"text/md", "text/md", "", "# Title", "md"},
		{"plain", "text/plain", "", "hello", "txt"},
		{"markdown with charset", "text/markdown; charset=utf-8", "", "# T", "md"},
		{"octet-stream sniffed to md by ext", "application/octet-stream", "n.md", "# T", "md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Parse(context.Background(), tc.contentType, tc.filename, []byte(tc.body))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if res.DocType != tc.wantDocType {
				t.Errorf("DocType = %q, want %q", res.DocType, tc.wantDocType)
			}
			if res.Text != tc.body {
				t.Errorf("Text = %q, want %q", res.Text, tc.body)
			}
			if res.Figures != nil {
				t.Errorf("Figures = %v, want nil for text route", res.Figures)
			}
		})
	}
}

// TestParse_UnsupportedType returns ErrUnsupportedType for a type that maps
// to no parser.
func TestParse_UnsupportedType(t *testing.T) {
	_, err := Parse(context.Background(), "application/zip", "x.zip", []byte("PK\x03\x04"))
	if !errors.Is(err, ErrUnsupportedType) {
		t.Errorf("err = %v, want ErrUnsupportedType", err)
	}
}

// TestParsePDF_EmptyBody errors before shelling out.
func TestParsePDF_EmptyBody(t *testing.T) {
	_, err := ParsePDF(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "empty body") {
		t.Errorf("err = %v, want empty body error", err)
	}
}

// TestParseLatexArchive_MissingBinary returns a descriptive error when the
// latex2text binary isn't present at LATEX2TEXT_BIN.
func TestParseLatexArchive_MissingBinary(t *testing.T) {
	t.Setenv("LATEX2TEXT_BIN", "/nonexistent/path/latex2text-xyz")
	_, _, err := ParseLatexArchive(context.Background(), []byte{0x1f, 0x8b})
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
	if !strings.Contains(err.Error(), "not available") {
		t.Errorf("err = %v, want it to mention the binary is not available", err)
	}
}

func TestSynthesizeMarkdown(t *testing.T) {
	t.Run("full document", func(t *testing.T) {
		d := latex2textDoc{
			Title:    "My Paper",
			Abstract: "We did things.",
			Authors:  []string{"Alice", "Bob"},
			Sections: []struct {
				Title string `json:"title"`
				Body  string `json:"body"`
				Level int    `json:"level"`
			}{
				{Title: "Intro", Body: "Background.", Level: 1},
				{Title: "Method", Body: "Details.", Level: 2},
			},
			Figures: []struct {
				Caption  string   `json:"caption"`
				Label    string   `json:"label"`
				SrcPaths []string `json:"src_paths"`
			}{
				{Caption: "A plot", Label: "fig1", SrcPaths: []string{"f1.png"}},
			},
		}
		md := synthesizeMarkdown(d)
		for _, want := range []string{
			"# My Paper",
			"*Authors:* Alice, Bob",
			"## Abstract",
			"We did things.",
			"# Intro",
			"## Method",
			"*Figure fig1: A plot*",
		} {
			if !strings.Contains(md, want) {
				t.Errorf("markdown missing %q\n---\n%s", want, md)
			}
		}
	})

	t.Run("empty doc yields empty string", func(t *testing.T) {
		if md := synthesizeMarkdown(latex2textDoc{}); md != "" {
			t.Errorf("empty doc produced %q, want empty", md)
		}
	})

	t.Run("section level clamping", func(t *testing.T) {
		d := latex2textDoc{
			Sections: []struct {
				Title string `json:"title"`
				Body  string `json:"body"`
				Level int    `json:"level"`
			}{
				{Title: "Zero", Body: "b", Level: 0},  // -> 2
				{Title: "Huge", Body: "b", Level: 99}, // -> 6
			},
		}
		md := synthesizeMarkdown(d)
		if !strings.Contains(md, "## Zero") {
			t.Errorf("level<=0 should clamp to ##; got:\n%s", md)
		}
		if !strings.Contains(md, "###### Huge") {
			t.Errorf("level>6 should clamp to ######; got:\n%s", md)
		}
	})

	t.Run("figure with neither caption nor label is skipped", func(t *testing.T) {
		d := latex2textDoc{
			Figures: []struct {
				Caption  string   `json:"caption"`
				Label    string   `json:"label"`
				SrcPaths []string `json:"src_paths"`
			}{
				{Caption: "", Label: "", SrcPaths: []string{"x.png"}},
			},
		}
		if md := synthesizeMarkdown(d); strings.Contains(md, "*Figure") {
			t.Errorf("empty figure should be skipped; got:\n%s", md)
		}
	})

	t.Run("figure label only", func(t *testing.T) {
		d := latex2textDoc{
			Figures: []struct {
				Caption  string   `json:"caption"`
				Label    string   `json:"label"`
				SrcPaths []string `json:"src_paths"`
			}{
				{Label: "fig2"},
			},
		}
		md := synthesizeMarkdown(d)
		if !strings.Contains(md, "*Figure fig2*") {
			t.Errorf("want '*Figure fig2*'; got:\n%s", md)
		}
	})
}

func TestTail(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 10, "abc"},
		{"abcde", 5, "abcde"},
		{"abcdefgh", 3, "...fgh"},
		{"", 5, ""},
	}
	for _, tc := range cases {
		if got := tail(tc.in, tc.n); got != tc.want {
			t.Errorf("tail(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}
