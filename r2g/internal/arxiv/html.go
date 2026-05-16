package arxiv

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/PuerkitoBio/goquery"
)

// ar5iv mounts the rendered paper inside <article class="ltx_document">.
// Anything outside that node is page chrome (navbar, footer) we want to
// drop before conversion.
const ar5ivArticleSelector = "article.ltx_document"

// ar5ivNoise are selectors to strip from the article before conversion.
// Mirrors the prune list in the Python _fetch_ar5iv at lines 261-271.
var ar5ivNoise = []string{
	"script",
	"style",
	".ltx_page_navbar",
	".ltx_page_footer",
	".ar5iv-footer",
	".ltx_pagination",
	".ltx_role_refnum",
}

// multiBlankLine collapses 3+ consecutive newlines down to 2. Matches
// the Python `re.sub(r"\n{3,}", "\n\n", ...)` post-processing.
var multiBlankLine = regexp.MustCompile(`\n{3,}`)

// convertAr5ivHTML lifts the rendered article out of an ar5iv page,
// rewrites MathML nodes to `$...$` / `$$...$$` markers, prunes noise,
// and runs html-to-markdown. Returns (markdown, title, ok, err); ok is
// false when the page doesn't contain a rendered article (treated as a
// cascade miss by the caller).
func convertAr5ivHTML(htmlStr, fallbackTitle string) (string, string, bool, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlStr))
	if err != nil {
		return "", "", false, fmt.Errorf("parse ar5iv html: %w", err)
	}
	replaceMathMLWithLatex(doc)

	article := doc.Find(ar5ivArticleSelector).First()
	if article.Length() == 0 {
		// ar5iv returned a non-rendered fallback page (typically a
		// redirect notice to arxiv.org/abs/<id>). The cascade should
		// fall through to a later tier.
		return "", "", false, nil
	}
	for _, sel := range ar5ivNoise {
		article.Find(sel).Remove()
	}

	title := fallbackTitle
	if t := article.Find(".ltx_title").First(); t.Length() > 0 {
		if s := strings.TrimSpace(t.Text()); s != "" {
			title = s
		}
	}

	htmlBytes, err := goquery.OuterHtml(article)
	if err != nil {
		return "", "", false, fmt.Errorf("serialise ar5iv article: %w", err)
	}
	md, err := htmltomarkdown.ConvertString(htmlBytes)
	if err != nil {
		return "", "", false, fmt.Errorf("html-to-markdown ar5iv: %w", err)
	}
	md = multiBlankLine.ReplaceAllString(md, "\n\n")
	md = strings.TrimSpace(md) + "\n"
	return md, title, true, nil
}

// convertVanityHTML is the arxiv-vanity equivalent. Vanity uses inline
// <span class="math"> elements with raw LaTeX inside (no MathML alttext),
// so we rewrite those directly before conversion.
func convertVanityHTML(htmlStr, fallbackTitle string) (string, string, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlStr))
	if err != nil {
		return "", "", fmt.Errorf("parse vanity html: %w", err)
	}
	rewriteVanityMathSpans(doc)

	root := doc.Find("article").First()
	if root.Length() == 0 {
		root = doc.Find("div.paper").First()
	}
	if root.Length() == 0 {
		root = doc.Find("body").First()
	}
	if root.Length() == 0 {
		root = doc.Selection
	}
	for _, sel := range []string{"script", "style", "nav", "header", "footer"} {
		root.Find(sel).Remove()
	}

	title := fallbackTitle
	if t := doc.Find("h1").First(); t.Length() > 0 {
		if s := strings.TrimSpace(t.Text()); s != "" {
			title = s
		}
	} else if t := doc.Find("title").First(); t.Length() > 0 {
		if s := strings.TrimSpace(t.Text()); s != "" {
			title = s
		}
	}

	htmlBytes, err := goquery.OuterHtml(root)
	if err != nil {
		return "", "", fmt.Errorf("serialise vanity article: %w", err)
	}
	md, err := htmltomarkdown.ConvertString(htmlBytes)
	if err != nil {
		return "", "", fmt.Errorf("html-to-markdown vanity: %w", err)
	}
	md = multiBlankLine.ReplaceAllString(md, "\n\n")
	md = strings.TrimSpace(md) + "\n"
	return md, title, nil
}

// replaceMathMLWithLatex rewrites every <math alttext="..."> element to
// a plain-text "$alttext$" (or "$$alttext$$" for display-style nodes)
// before html-to-markdown runs. Without this, MathML survives as HTML
// soup that markdownify doesn't know how to handle.
//
// Mirrors _replace_mathml_with_latex (Python lines 82-113).
func replaceMathMLWithLatex(doc *goquery.Document) {
	doc.Find("math").Each(func(_ int, s *goquery.Selection) {
		alt, hasAlt := s.Attr("alttext")
		if !hasAlt || alt == "" {
			alt = strings.TrimSpace(s.Text())
		}
		if alt == "" {
			s.Remove()
			return
		}
		classes, _ := s.Attr("class")
		display, _ := s.Attr("display")
		isDisplay := strings.Contains(classes, "displaystyle") || display == "block"
		delim := "$"
		if isDisplay {
			delim = "$$"
		}
		// Replace the node with a text node carrying the inline math.
		s.ReplaceWithHtml(delim + escapeLatexForHTMLText(alt) + delim)
	})
}

// rewriteVanityMathSpans handles arxiv-vanity's <span class="math"> nodes.
// Span contents are raw LaTeX (not MathML), so we just lift the text
// content and wrap with the right delimiters.
func rewriteVanityMathSpans(doc *goquery.Document) {
	doc.Find("span").Each(func(_ int, s *goquery.Selection) {
		classes, _ := s.Attr("class")
		if classes == "" || !strings.Contains(strings.ToLower(classes), "math") {
			return
		}
		text := strings.TrimSpace(s.Text())
		if text == "" {
			s.Remove()
			return
		}
		isDisplay := strings.Contains(strings.ToLower(classes), "display")
		delim := "$"
		if isDisplay {
			delim = "$$"
		}
		s.ReplaceWithHtml(delim + escapeLatexForHTMLText(text) + delim)
	})
}

// escapeLatexForHTMLText keeps the LaTeX intact when it gets re-injected
// into the HTML DOM as a text node. We only escape the four HTML special
// chars the parser would otherwise interpret (< > &) -- LaTeX backslashes,
// braces, and dollar signs are safe in HTML text. The downstream
// html-to-markdown then carries this text through unchanged.
func escapeLatexForHTMLText(s string) string {
	var b bytes.Buffer
	for _, r := range s {
		switch r {
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '&':
			b.WriteString("&amp;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
