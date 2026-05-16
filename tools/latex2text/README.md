# latex2text

A fast, specialized LaTeX → JSON converter for arxiv-shaped papers. Lives
in the `evo` monorepo under `tools/latex2text/` and is built with Zig
0.16.

Replaces pandoc for the **common case** on the deepresearch fast path.
Pandoc stays as the fallback for documents that hit features outside this
parser's scope.

## Why

pandoc takes 5–30s per paper to convert LaTeX → plain text. For
arxiv-shaped papers (predictable preambles, common macro vocabulary), a
specialized streaming parser hits 0.5–2s with no external runtime cost.

Quick local benchmark on `arxiv:1706.03762` (Attention Is All You Need;
~1.1MB compressed tarball, multi-file `\input` includes, dozens of
equations and figures):

| tool        | wall time | output size |
|-------------|-----------|-------------|
| pandoc 3.6  | **241ms** | text        |
| latex2text  | **16ms**  | JSON        |

~15× faster on this paper. Most of pandoc's time is the Haskell startup
plus its full-AST pipeline; latex2text is a single-pass byte walker.
Speedup grows on shorter docs (process-startup-dominated regime).

## Build

```bash
zig build                         # debug build, ./zig-out/bin/latex2text
zig build -Doptimize=ReleaseSafe  # native release
zig build -Dtarget=aarch64-linux-musl -Doptimize=ReleaseSmall  # Pi 5
zig build test                    # unit tests + fixture tests
```

Or via the monorepo Makefile:

```bash
make latex2text       # native, copies into bin/latex2text
make pi-latex2text    # arm64-musl static, lives in tools/latex2text/zig-out/bin
```

## CLI

```
latex2text [--from-tarball PATH | --from-stdin-tarball
            | --from-tex FILE | --from-stdin-tex]
           [--out json|text]
           [--strict | --lenient]
```

Default: `--from-stdin-tarball --out json --lenient`. Reads tarball/tex
from stdin or the given path; writes JSON (or plain text) to stdout.

### JSON shape

```json
{
  "title": "Foo and Bar",
  "abstract": "We show ...",
  "authors": ["A. Author", "B. Author"],
  "sections": [
    {"depth": 1, "title": "Introduction", "body": "... $\\sqrt{d_k}$ ..."},
    {"depth": 1, "title": "Method", "body": "..."},
    {"depth": 2, "title": "Architecture", "body": "Model | BLEU\nA | 1\n"}
  ],
  "figures": [
    {"label": "fig:arch", "caption": "Our architecture diagram."}
  ],
  "math": [
    {"id": "m0", "env": "inline", "source": "\\sqrt{d_k}"},
    {"id": "m1", "env": "equation", "source": "E = mc^2"}
  ],
  "math_placeholders": 47,
  "parser_version": "0.2",
  "parser_errors": [],
  "elapsed_ms": 312
}
```

Section order matches source order. Section bodies are plain text with
paragraph boundaries preserved.

**Math (since v0.2):** the literal LaTeX source for every math expression
is kept inline in the body, wrapped in its original-style delimiters:

- Inline math (`$...$`) is emitted as `$...$` in the body.
- Display math (`\[...\]`, `$$...$$`, `equation`, `align`, `gather`,
  `multline`, `eqnarray`, `flalign`, `displaymath`) is emitted as
  `$$...$$` on its own line in the body.

The full corpus is also collected into a top-level `math` array, with one
`{id, env, source}` entry per occurrence. Downstream chunkers can index
both the inline form (so search for `softmax(QK^T/sqrt(d_k))` hits) and
the array (for equation-level retrieval).

**Tables (since v0.2):** `tabular`-family environments (`tabular`,
`tabularx`, `tabulary`, `longtable`, `array`) render their cells inline
in the body as pipe-joined rows. Each row becomes one line, cells are
joined by ` | ` (Markdown-table-ish, no header bar). The column spec
(`{lccc}`) and pure-rule commands (`\hline`, `\toprule`, `\midrule`,
`\bottomrule`, `\cmidrule`, `\specialrule`, `\rule{}{}`) are stripped.
`\caption` and `\label` of the enclosing `table` env still land in the
`figures` array, keeping the v0.1 contract for that side.

**Math placeholders count** (`math_placeholders`) is the total number of
math expressions encountered (inline + display + every tabular cell that
contains math). It's exposed for compatibility and as a sanity signal.

**Authors (since v0.2):** the `\author{...}` block is split on `\And`,
`\AND`, `\and`, and 3+ consecutive newlines. Each resulting block has
its first non-affiliation/non-email line kept as the canonical name.
Affiliation lines (matching common prefixes like "Google", "University
of", "Department of", URL, etc.) and email lines (containing `@`) are
dropped.

## What it handles

Common arxiv constructs:

- Preamble (`\documentclass`, `\usepackage`, `\newcommand`, etc.) — stripped.
- Comments (`% ...`), with `\%` escape respected.
- `\title{...}`, `\author{...}`, `\begin{abstract}...\end{abstract}`.
- `\section`, `\subsection`, `\subsubsection`, `\paragraph` headers.
- Math: `\begin{equation|align|...}`, `\[...\]`, `$...$`, `$$...$$`
  — kept inline as `$...$` (inline) or `$$...$$` (display) in the body,
  PLUS recorded in a top-level `math` array. Total count exposed as
  `math_placeholders`.
- Lists: `itemize`, `enumerate`, `description` → `\item` → `- ` bullets.
- Figures: extracts `\caption` + `\label`; figure body discarded.
- Tables: extracts caption + label into `figures`; `tabular` body
  rendered as pipe-joined rows inline in the section text.
- `\textcolor`, `\colorbox`, `\fcolorbox`, `\color`, `\definecolor`,
  `\hspace`, `\vspace`, `\setlength`, `\setcounter`, `\fontsize`, etc.
  — first style/metadata arg dropped; remaining body arg (if any) kept.
- `\L`, `\l`, `\O`, `\ss`, `\AE`, `\oe`, `\ldots` etc. → mapped to
  the corresponding Unicode codepoint (so `{\L}ukasz` → `Łukasz`).
- `\newcommand`, `\renewcommand`, `\providecommand` (and `*`-variants):
  whole definition (name + optional params + body) is dropped.
- Text styling: `\textbf`, `\textit`, `\emph`, `\underline`, `\texttt`,
  `\mathbf`, `\boldsymbol`, etc. — body kept, command dropped.
- Refs: `\cite`, `\ref`, `\eqref`, `\autoref`, `\cref` → `[CITE]` /
  `[REF]` / `[EQ]`.
- `\input{filename}` / `\include{filename}` — recursively spliced from
  the tarball.
- Verbatim: `\begin{verbatim}` / `\begin{lstlisting}` body preserved.
- Quirks: `~` → space; `\\` → newline; `---` → em-dash; `--` → en-dash;
  smart quotes normalised to straight.
- Unknown commands: command dropped, brace contents kept inline.
- Unknown environments: `\begin{X}` / `\end{X}` dropped, body kept.

### Tarball handling

arxiv ships papers as gzip tarballs at `https://arxiv.org/e-print/<id>`.
Main-`.tex` detection logic:

1. If only one `.tex` file in the tarball — that's main.
2. Else if `main.tex` exists — main.
3. Else if `paper.tex` exists — main.
4. Else: pick the `.tex` containing `\documentclass`; if multiple, the
   largest wins.

All tarball entries are loaded into memory once (arxiv tarballs are tiny);
`\input{name}` resolves against that map.

## What it does NOT handle (use pandoc fallback)

These are the categories most likely to need pandoc:

- **Bibliographies** — `.bbl` files and `\bibitem` lists are dropped, not
  parsed into a reference list.
- **BibTeX** — `.bib` files are not read or expanded.
- **Math typesetting** — math source is preserved verbatim inline but is
  not rendered to MathML or normalised. Macros referenced inside math
  bodies (e.g. `\dmodel` defined via `\newcommand`) come through as the
  literal control sequence, not the expansion.
- **Custom macros** — `\newcommand{\foo}{...}` is stripped; later uses of
  `\foo` are treated as unknown commands (body kept, name dropped). For
  macro-heavy papers this loses semantic context.
- **TikZ / pgfplots / pstricks** — figure bodies are dropped; only
  `\caption` survives.
- **Specialised classes** — `revtex4-1`, `iopart`, `acmart`, etc. use
  unusual sectioning macros (`\section*`, `\title[...]{...}` with optional
  short titles, abstract-as-command instead of env). Star variants are
  handled; the rest may need pandoc.
- **Conditional input** (`\if...\fi`) — not interpreted; both branches
  effectively pass through.
- **Table layout** — column spec, alignment, and rules are stripped.
  `\multicolumn` / `\multirow` are flattened to their content. Cells
  are joined by ` | ` with no header bar.
- **Non-ASCII source via legacy encodings** — input is assumed UTF-8.

When in doubt, the backend should run latex2text first and **fall back
to pandoc** if `parser_errors` is non-empty, `sections` is empty, or the
output looks suspiciously short for the input size.

## Tests

Fixtures live in `tests/fixtures/`:

- `simple.tex` — minimal one-section paper.
- `with_math.tex` — equations, citations, figures.
- `multifile/{main,intro,method}.tex` — `\input` resolution.
- `edge_cases.tex` — unknown commands, unknown environments, escaped
  percent, em-dashes, smart quotes, paragraph headers, `\textcolor`,
  `\color`, a tabular table, Polish `\L`.

There's also an end-to-end test against arxiv:1706.03762 ("Attention Is
All You Need"). The tarball is expected at
`tests/cache/1706.03762.tar.gz` (gitignored — fetch with
`curl -L -o tests/cache/1706.03762.tar.gz https://arxiv.org/e-print/1706.03762`).
The test is skipped if the cache is missing, so `zig build test` is
network-free.

Run with `zig build test`. Tests assert structural properties (section
count, titles match, math count > 0, figure labels extracted) rather
than verbatim text — the parser is expected to evolve.

## How to wire this in (deepresearch backend)

Out of scope for this PR (different repo at `/home/sirus/deepresearch/`),
but the integration shape:

1. Ship the `aarch64-linux-musl` static binary into the deepresearch
   alpine sidecar image (or invoke it as a process from the existing
   `arxiv-skip` sidecar).
2. Before invoking pandoc, pipe the downloaded `.tar.gz` directly to
   `latex2text --from-stdin-tarball --out json` and parse the JSON.
3. If parsing fails, JSON has non-empty `parser_errors`, or `sections`
   is empty, fall back to pandoc on a temp directory extraction.
4. Expected steady-state: ~95% of papers handled by latex2text in
   well under 50ms; remainder routed to pandoc as before.

The CLI is intentionally stdin-friendly so the backend can keep its
existing tarball-handling without writing to disk.

## Parser internals

The parser is a single file (`src/parser.zig`) operating on an in-memory
`[]const u8`. Output flows into a `Builder` struct that maintains:

- `title`, `abstract`, `authors` accumulators.
- A "current section" being filled (`cur_depth`, `cur_title`, `cur_body`).
- A `preamble` buffer for text before the first `\section`.
- A `target` redirection pointer so nested `renderInto` calls (e.g.,
  rendering an `\textbf{X}` inside a `\caption{}` inside a `\begin{figure}`)
  go where the caller asked rather than into the live section body.

`tar.zig` decompresses gzip via `std.compress.flate` and walks entries
via `std.tar.Iterator`, materialising every file as an in-arena byte
slice. `\input{name}` lookup is a linear scan over the entries — fine
for arxiv tarball sizes.

`main.zig` wires CLI parsing, stdin/file slurping, and JSON streaming via
`std.json.Stringify`. All allocations land in a single
`std.heap.ArenaAllocator` that is freed at exit, so per-paper memory
profile is "load tarball + decompressed bytes + parsed strings + JSON
output buffer" — typically <10MB even for large papers.
