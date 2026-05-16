//! arxiv-flavor LaTeX → structured text parser.
//!
//! Operates on an in-memory byte slice. The caller is responsible for
//! resolving `\input{}` includes against a tarball (or filesystem); this
//! parser exposes a `resolveInclude` callback hook for that.
//!
//! Design: single-pass tokeniser that walks the source character by
//! character. We keep a small state machine for verbatim / math /
//! comment handling. Output is collected into a `Document` struct that
//! the caller can serialise to JSON.

const std = @import("std");
const mem = std.mem;
const Allocator = std.mem.Allocator;
const ArrayList = std.ArrayList;

pub const parser_version = "0.3";

pub const Figure = struct {
    label: []const u8 = "",
    caption: []const u8 = "",
    src_paths: [][]const u8 = &.{},
};

pub const MathBlock = struct {
    id: []const u8 = "",
    env: []const u8 = "",
    source: []const u8 = "",
};

pub const Section = struct {
    depth: u8,
    title: []const u8,
    body: []const u8,
};

pub const Document = struct {
    title: []const u8 = "",
    abstract: []const u8 = "",
    authors: [][]const u8 = &.{},
    sections: []Section = &.{},
    figures: []Figure = &.{},
    math: []MathBlock = &.{},
    math_placeholders: usize = 0,
    parser_version: []const u8 = parser_version,
    parser_errors: [][]const u8 = &.{},
    elapsed_ms: u64 = 0,
};

/// Callback signature: given an `\input{name}` argument, return either the
/// referenced LaTeX source (caller-owned slice tied to the parser arena) or
/// `null` if the file cannot be resolved.
pub const ResolveFn = *const fn (ctx: ?*anyopaque, name: []const u8) ?[]const u8;

pub const Options = struct {
    strict: bool = false,
    resolve_ctx: ?*anyopaque = null,
    resolve_include: ?ResolveFn = null,
};

/// Builder-style intermediate state. Lives for the duration of a parse.
///
/// Output is directed via an explicit `target` pointer. Section body and
/// preamble buffers are reached via that pointer rather than by swapping
/// field contents, which makes nested renderInto calls safe.
const Builder = struct {
    arena: Allocator,
    opts: Options,
    title: ArrayList(u8) = .empty,
    abstract: ArrayList(u8) = .empty,
    authors: ArrayList([]const u8) = .empty,
    sections: ArrayList(Section) = .empty,
    figures: ArrayList(Figure) = .empty,
    math: ArrayList(MathBlock) = .empty,
    errors: ArrayList([]const u8) = .empty,
    math_count: usize = 0,

    // Current open section being filled.
    cur_depth: u8 = 1,
    cur_title: ArrayList(u8) = .empty,
    cur_body: ArrayList(u8) = .empty,
    have_section: bool = false,
    preamble: ArrayList(u8) = .empty,

    // Where walk()/handleCommand output goes. When null, the default
    // section/preamble heuristic applies (`defaultBuf`).
    target: ?*ArrayList(u8) = null,

    fn note(self: *Builder, comptime fmt: []const u8, args: anytype) !void {
        const msg = try std.fmt.allocPrint(self.arena, fmt, args);
        try self.errors.append(self.arena, msg);
    }

    fn flushSection(self: *Builder) !void {
        if (!self.have_section and self.preamble.items.len == 0) return;
        const title = if (self.have_section)
            try self.arena.dupe(u8, trim(self.cur_title.items))
        else
            "";
        var body_buf: ArrayList(u8) = .empty;
        if (self.preamble.items.len > 0) {
            try body_buf.appendSlice(self.arena, self.preamble.items);
            self.preamble.clearRetainingCapacity();
        }
        try body_buf.appendSlice(self.arena, self.cur_body.items);
        const body_trim = trim(body_buf.items);
        // Skip emitting an empty placeholder section.
        if (title.len == 0 and body_trim.len == 0) {
            self.cur_title.clearRetainingCapacity();
            self.cur_body.clearRetainingCapacity();
            return;
        }
        const body = try self.arena.dupe(u8, body_trim);
        try self.sections.append(self.arena, .{
            .depth = self.cur_depth,
            .title = title,
            .body = body,
        });
        self.cur_title.clearRetainingCapacity();
        self.cur_body.clearRetainingCapacity();
    }

    fn outputBuf(self: *Builder) *ArrayList(u8) {
        if (self.target) |t| return t;
        return if (self.have_section) &self.cur_body else &self.preamble;
    }

    fn nextMathId(self: *Builder) ![]const u8 {
        return try std.fmt.allocPrint(self.arena, "m{d}", .{self.math_count});
    }

    fn recordMath(self: *Builder, env: []const u8, source: []const u8) !void {
        const id = try self.nextMathId();
        try self.math.append(self.arena, .{
            .id = id,
            .env = try self.arena.dupe(u8, env),
            .source = try self.arena.dupe(u8, source),
        });
        self.math_count += 1;
    }
};

// ---------- Public entry point ------------------------------------------

pub fn parse(arena: Allocator, source: []const u8, opts: Options) !Document {
    var b = Builder{ .arena = arena, .opts = opts };
    try walk(&b, source);
    try b.flushSection();

    return Document{
        .title = try arena.dupe(u8, trim(b.title.items)),
        .abstract = try arena.dupe(u8, trim(b.abstract.items)),
        .authors = try b.authors.toOwnedSlice(arena),
        .sections = try b.sections.toOwnedSlice(arena),
        .figures = try b.figures.toOwnedSlice(arena),
        .math = try b.math.toOwnedSlice(arena),
        .math_placeholders = b.math_count,
        .parser_errors = try b.errors.toOwnedSlice(arena),
    };
}

// ---------- Tokeniser / walker ------------------------------------------

const max_input_depth = 8;
var input_depth: usize = 0; // recursion guard for \input

fn walk(b: *Builder, source: []const u8) (Allocator.Error)!void {
    var i: usize = 0;
    while (i < source.len) {
        const c = source[i];
        switch (c) {
            '%' => {
                // Comment to EOL. (We already handled \% in literal()).
                while (i < source.len and source[i] != '\n') : (i += 1) {}
            },
            '\\' => {
                i += 1;
                if (i >= source.len) break;
                // Handle \\, \%, \&, \$, \_, \{, \}, \#, \~
                const next = source[i];
                if (next == '\\') {
                    try b.outputBuf().append(b.arena, '\n');
                    i += 1;
                    // optional [..] spacing arg after \\
                    if (i < source.len and source[i] == '[') {
                        i = skipBracket(source, i);
                    }
                    continue;
                }
                if (isEscapedLiteral(next)) {
                    try b.outputBuf().append(b.arena, next);
                    i += 1;
                    continue;
                }
                if (next == '[') {
                    // \[ ... \] display math
                    const inner_start = i + 1;
                    const end = findClosingDisplayMath(source, inner_start);
                    // end points just past the closing \] — inner body is source[inner_start..end-2]
                    const body_end = if (end >= 2 and end <= source.len) end - 2 else end;
                    const inner = source[inner_start..body_end];
                    try emitDisplayMath(b, inner, "displaymath");
                    i = end;
                    continue;
                }
                // Otherwise it's a command name. Read it.
                const cmd_start = i;
                while (i < source.len and (std.ascii.isAlphabetic(source[i]) or source[i] == '@')) : (i += 1) {}
                if (i < source.len and source[i] == '*') i += 1; // star variants
                const cmd = source[cmd_start..i];
                if (cmd.len == 0) {
                    // unknown single-char escape; just skip
                    if (i < source.len) i += 1;
                    continue;
                }
                i = try handleCommand(b, cmd, source, i);
            },
            '$' => {
                if (i + 1 < source.len and source[i + 1] == '$') {
                    // display math $$...$$
                    const inner_start = i + 2;
                    const end = findDollarDisplay(source, inner_start);
                    const body_end = if (end >= 2 and end <= source.len) end - 2 else end;
                    const inner = source[inner_start..body_end];
                    try emitDisplayMath(b, inner, "displaymath");
                    i = end;
                } else {
                    const inner_start = i + 1;
                    const end = findDollar(source, inner_start);
                    const body_end = if (end >= 1 and end <= source.len) end - 1 else end;
                    const inner = source[inner_start..body_end];
                    try emitInlineMath(b, inner);
                    i = end;
                }
            },
            '~' => {
                try b.outputBuf().append(b.arena, ' ');
                i += 1;
            },
            '-' => {
                if (i + 2 < source.len and source[i + 1] == '-' and source[i + 2] == '-') {
                    try b.outputBuf().appendSlice(b.arena, "\u{2014}"); // em-dash
                    i += 3;
                } else if (i + 1 < source.len and source[i + 1] == '-') {
                    try b.outputBuf().appendSlice(b.arena, "\u{2013}"); // en-dash
                    i += 2;
                } else {
                    try b.outputBuf().append(b.arena, '-');
                    i += 1;
                }
            },
            '`' => {
                if (i + 1 < source.len and source[i + 1] == '`') {
                    try b.outputBuf().append(b.arena, '"');
                    i += 2;
                } else {
                    try b.outputBuf().append(b.arena, '\'');
                    i += 1;
                }
            },
            '\'' => {
                if (i + 1 < source.len and source[i + 1] == '\'') {
                    try b.outputBuf().append(b.arena, '"');
                    i += 2;
                } else {
                    try b.outputBuf().append(b.arena, '\'');
                    i += 1;
                }
            },
            '{', '}' => {
                // unmatched braces at top level — just drop them.
                i += 1;
            },
            else => {
                try b.outputBuf().append(b.arena, c);
                i += 1;
            },
        }
    }
}

fn handleCommand(b: *Builder, cmd: []const u8, source: []const u8, idx: usize) !usize {
    // strip optional [..] before {..}
    var i = skipOptionalArgs(source, idx);
    const base = stripStar(cmd);

    if (mem.eql(u8, base, "documentclass") or mem.eql(u8, base, "usepackage") or
        mem.eql(u8, base, "bibliographystyle") or mem.eql(u8, base, "bibliography") or
        mem.eql(u8, base, "pagestyle") or mem.eql(u8, base, "thispagestyle") or
        mem.eql(u8, base, "newtheorem") or mem.eql(u8, base, "DeclareMathOperator") or
        mem.eql(u8, base, "noindent") or mem.eql(u8, base, "newline") or
        mem.eql(u8, base, "vfill") or mem.eql(u8, base, "hfill") or
        mem.eql(u8, base, "centering") or mem.eql(u8, base, "maketitle") or
        mem.eql(u8, base, "tableofcontents") or mem.eql(u8, base, "addcontentsline") or
        mem.eql(u8, base, "PassOptionsToPackage") or mem.eql(u8, base, "RequirePackage") or
        mem.eql(u8, base, "LoadClass") or mem.eql(u8, base, "ProvidesPackage") or
        mem.eql(u8, base, "ProvidesClass") or mem.eql(u8, base, "NeedsTeXFormat") or
        mem.eql(u8, base, "DeclareOption") or mem.eql(u8, base, "ProcessOptions") or
        mem.eql(u8, base, "ExecuteOptions") or mem.eql(u8, base, "renewenvironment") or
        mem.eql(u8, base, "newenvironment") or
        mem.eql(u8, base, "DeclareRobustCommand") or mem.eql(u8, base, "AtBeginDocument") or
        mem.eql(u8, base, "AtEndDocument") or mem.eql(u8, base, "geometry") or
        mem.eql(u8, base, "captionsetup") or mem.eql(u8, base, "graphicspath") or
        mem.eql(u8, base, "includegraphics") or mem.eql(u8, base, "input@path"))
    {
        // Eat any following brace groups, then return.
        return eatTrailingGroups(source, i);
    }

    // \newcommand-family takes an optional `\name` _before_ its first
    // brace arg, so the normal "eat trailing [..] and {..}" doesn't catch
    // the target command name. Swallow that target explicitly so the
    // command body never gets re-walked as if it were live LaTeX.
    if (mem.eql(u8, base, "newcommand") or mem.eql(u8, base, "renewcommand") or
        mem.eql(u8, base, "providecommand") or mem.eql(u8, base, "newrobustcmd") or
        mem.eql(u8, base, "renewrobustcmd"))
    {
        return skipMacroDefinition(source, i);
    }

    // Commands whose first brace arg is style/metadata and should be dropped,
    // with subsequent arg(s) kept and rendered inline. Common offenders:
    //   \textcolor{red}{body}         → body
    //   \colorbox{yellow}{body}       → body
    //   \fcolorbox{red}{yellow}{body} → body
    //   \color{red}                   → (nothing — eats one arg, no body)
    //   \fontsize{12pt}{14pt}         → (nothing — eats two metadata args)
    //   \hspace{2em} / \vspace{1em}   → (nothing)
    //   \setcounter{x}{1}             → (nothing)
    //   \setlength{\foo}{1pt}         → (nothing)
    //   \definecolor{name}{rgb}{...}  → (nothing)
    // We model this with a small table.
    if (firstArgDropSpec(base)) |spec| {
        return try handleFirstArgDrop(b, source, i, spec);
    }

    if (mem.eql(u8, base, "begin")) return try handleEnv(b, source, i, true);
    if (mem.eql(u8, base, "end")) return try handleEnv(b, source, i, false);

    if (mem.eql(u8, base, "title")) {
        const g = readGroup(source, i);
        try renderInto(b, &b.title, g.body);
        return g.end;
    }
    if (mem.eql(u8, base, "author")) {
        const g = readGroup(source, i);
        try parseAuthors(b, g.body);
        return g.end;
    }
    if (mem.eql(u8, base, "section")) return try openSection(b, source, i, 1);
    if (mem.eql(u8, base, "subsection")) return try openSection(b, source, i, 2);
    if (mem.eql(u8, base, "subsubsection")) return try openSection(b, source, i, 3);
    if (mem.eql(u8, base, "paragraph") or mem.eql(u8, base, "subparagraph")) {
        const g = readGroup(source, i);
        var hdr: ArrayList(u8) = .empty;
        defer hdr.deinit(b.arena);
        try renderInto(b, &hdr, g.body);
        try b.outputBuf().appendSlice(b.arena, "\n\n");
        try b.outputBuf().appendSlice(b.arena, trim(hdr.items));
        try b.outputBuf().append(b.arena, '.');
        try b.outputBuf().append(b.arena, ' ');
        return g.end;
    }
    if (mem.eql(u8, base, "label")) {
        // Drop labels in body; figure/equation labels are captured by env
        // handlers via findCommandArg, so swallowing here is safe.
        const g = readGroup(source, i);
        return g.end;
    }
    if (mem.eql(u8, base, "cite") or mem.eql(u8, base, "citep") or
        mem.eql(u8, base, "citet") or mem.eql(u8, base, "citealp") or
        mem.eql(u8, base, "citeauthor") or mem.eql(u8, base, "citeyear") or
        mem.eql(u8, base, "nocite"))
    {
        try b.outputBuf().appendSlice(b.arena, "[CITE]");
        // Optionally [..]{..}
        return eatTrailingGroups(source, i);
    }
    if (mem.eql(u8, base, "ref") or mem.eql(u8, base, "pageref") or
        mem.eql(u8, base, "autoref") or mem.eql(u8, base, "Cref") or
        mem.eql(u8, base, "cref"))
    {
        try b.outputBuf().appendSlice(b.arena, "[REF]");
        return eatTrailingGroups(source, i);
    }
    if (mem.eql(u8, base, "eqref")) {
        try b.outputBuf().appendSlice(b.arena, "[EQ]");
        return eatTrailingGroups(source, i);
    }
    if (mem.eql(u8, base, "footnote") or mem.eql(u8, base, "footnotetext") or
        mem.eql(u8, base, "thanks") or mem.eql(u8, base, "affil") or
        mem.eql(u8, base, "affiliation") or mem.eql(u8, base, "email") or
        mem.eql(u8, base, "address") or mem.eql(u8, base, "institute") or
        mem.eql(u8, base, "altaffiliation"))
    {
        // Drop body entirely — pollutes title/abstract/author extraction.
        return eatTrailingGroups(source, i);
    }
    if (mem.eql(u8, base, "url") or mem.eql(u8, base, "href")) {
        // \url{x} → x ; \href{x}{y} → y
        if (mem.eql(u8, base, "href")) {
            _ = readGroup(source, i); // drop URL
            i = readGroup(source, i).end;
            const g2 = readGroup(source, i);
            try renderInto(b, b.outputBuf(), g2.body);
            return g2.end;
        }
        const g = readGroup(source, i);
        try renderInto(b, b.outputBuf(), g.body);
        return g.end;
    }
    if (mem.eql(u8, base, "input") or mem.eql(u8, base, "include")) {
        const g = readGroup(source, i);
        const name = trim(g.body);
        if (b.opts.resolve_include) |resolve| {
            if (input_depth < max_input_depth) {
                if (resolve(b.opts.resolve_ctx, name)) |included| {
                    input_depth += 1;
                    defer input_depth -= 1;
                    try walk(b, included);
                } else {
                    try b.note("could not resolve \\input{{{s}}}", .{name});
                }
            } else {
                try b.note("\\input depth limit exceeded at {s}", .{name});
            }
        }
        return g.end;
    }
    if (mem.eql(u8, base, "item")) {
        // bullets / enumerated items emitted as plain "- " — list_kind is
        // tracked via the environment stack but the simple representation
        // is good enough for plain-text downstream consumers.
        try b.outputBuf().appendSlice(b.arena, "\n- ");
        return i;
    }
    if (mem.eql(u8, base, "caption")) {
        // captions appearing outside a figure env: just emit body inline.
        const g = readGroup(source, i);
        try renderInto(b, b.outputBuf(), g.body);
        return g.end;
    }

    // Text-style commands: keep body, drop wrapper.
    if (isPassthroughCommand(base)) {
        const g = readGroup(source, i);
        try renderInto(b, b.outputBuf(), g.body);
        return g.end;
    }

    // Single-character symbol commands: \L → Ł, \ss → ß, etc. These take no
    // brace arg of their own; the surrounding `{...}` in `{\L}ukasz` is a
    // grouping construct that ends at the `}`, NOT an argument.
    if (symbolReplacement(base)) |sym| {
        try b.outputBuf().appendSlice(b.arena, sym);
        return i;
    }

    // Unknown command: keep all brace-arg contents joined by spaces, drop
    // the command itself.
    return try eatAndRenderTrailingGroups(b, source, i);
}

fn symbolReplacement(name: []const u8) ?[]const u8 {
    // Commonly used \X commands that map to a single Unicode codepoint.
    if (mem.eql(u8, name, "L")) return "\u{0141}"; // Ł
    if (mem.eql(u8, name, "l")) return "\u{0142}"; // ł
    if (mem.eql(u8, name, "O")) return "\u{00D8}"; // Ø
    if (mem.eql(u8, name, "o")) return "\u{00F8}"; // ø
    if (mem.eql(u8, name, "AE")) return "\u{00C6}"; // Æ
    if (mem.eql(u8, name, "ae")) return "\u{00E6}"; // æ
    if (mem.eql(u8, name, "OE")) return "\u{0152}"; // Œ
    if (mem.eql(u8, name, "oe")) return "\u{0153}"; // œ
    if (mem.eql(u8, name, "ss")) return "\u{00DF}"; // ß
    if (mem.eql(u8, name, "AA")) return "\u{00C5}"; // Å
    if (mem.eql(u8, name, "aa")) return "\u{00E5}"; // å
    if (mem.eql(u8, name, "P")) return "\u{00B6}"; // ¶
    if (mem.eql(u8, name, "S")) return "\u{00A7}"; // §
    if (mem.eql(u8, name, "dag")) return "\u{2020}"; // †
    if (mem.eql(u8, name, "ddag")) return "\u{2021}"; // ‡
    if (mem.eql(u8, name, "ldots")) return "\u{2026}"; // …
    if (mem.eql(u8, name, "dots")) return "\u{2026}";
    if (mem.eql(u8, name, "copyright")) return "\u{00A9}"; // ©
    if (mem.eql(u8, name, "pounds")) return "\u{00A3}"; // £
    return null;
}

const FirstArgDropSpec = struct {
    /// number of leading brace groups whose content is metadata and should be
    /// dropped (after any [..] optionals).
    drop_count: u8,
    /// when true, any further brace groups beyond `drop_count` have their
    /// content rendered into the output. When false, all following brace
    /// groups are eaten silently (no output).
    keep_rest: bool,
};

fn firstArgDropSpec(name: []const u8) ?FirstArgDropSpec {
    // textcolor{color}{body}     — drop 1, keep rest
    // colorbox{color}{body}      — drop 1, keep rest
    // fcolorbox{frame}{bg}{body} — drop 2, keep rest
    // color{name}                — drop 1, no body
    // definecolor{n}{m}{spec}    — eat 3, no body
    // fontsize{pt}{ht}           — eat 2, no body
    // selectfont                 — no args
    // setcounter{c}{n}           — eat 2, no body
    // setlength{l}{v}            — eat 2, no body
    // addtolength{l}{v}          — eat 2, no body
    // hspace{x} / vspace{x}      — eat 1, no body
    // hphantom{x} / vphantom{x}  — eat 1, no body
    // phantom{x}                 — eat 1, no body
    // rule{w}{h}                 — eat 2, no body
    // scalebox{factor}{body}     — drop 1, keep rest
    // raisebox{lift}{body}       — drop 1, keep rest
    if (mem.eql(u8, name, "textcolor")) return .{ .drop_count = 1, .keep_rest = true };
    if (mem.eql(u8, name, "colorbox")) return .{ .drop_count = 1, .keep_rest = true };
    if (mem.eql(u8, name, "fcolorbox")) return .{ .drop_count = 2, .keep_rest = true };
    if (mem.eql(u8, name, "scalebox")) return .{ .drop_count = 1, .keep_rest = true };
    if (mem.eql(u8, name, "raisebox")) return .{ .drop_count = 1, .keep_rest = true };
    // \multicolumn{nspan}{align}{body} → body
    if (mem.eql(u8, name, "multicolumn")) return .{ .drop_count = 2, .keep_rest = true };
    // \multirow{nspan}{width}{body} → body (and *-variant)
    if (mem.eql(u8, name, "multirow")) return .{ .drop_count = 2, .keep_rest = true };
    if (mem.eql(u8, name, "multirowcell")) return .{ .drop_count = 2, .keep_rest = true };
    // \cmidrule{a-b} → nothing
    if (mem.eql(u8, name, "cmidrule")) return .{ .drop_count = 1, .keep_rest = false };
    if (mem.eql(u8, name, "cline")) return .{ .drop_count = 1, .keep_rest = false };
    if (mem.eql(u8, name, "specialrule")) return .{ .drop_count = 3, .keep_rest = false };
    if (mem.eql(u8, name, "color")) return .{ .drop_count = 1, .keep_rest = false };
    if (mem.eql(u8, name, "definecolor")) return .{ .drop_count = 3, .keep_rest = false };
    if (mem.eql(u8, name, "pagecolor")) return .{ .drop_count = 1, .keep_rest = false };
    if (mem.eql(u8, name, "fontsize")) return .{ .drop_count = 2, .keep_rest = false };
    if (mem.eql(u8, name, "selectfont")) return .{ .drop_count = 0, .keep_rest = false };
    if (mem.eql(u8, name, "setcounter")) return .{ .drop_count = 2, .keep_rest = false };
    if (mem.eql(u8, name, "setlength")) return .{ .drop_count = 2, .keep_rest = false };
    if (mem.eql(u8, name, "addtolength")) return .{ .drop_count = 2, .keep_rest = false };
    if (mem.eql(u8, name, "hspace")) return .{ .drop_count = 1, .keep_rest = false };
    if (mem.eql(u8, name, "vspace")) return .{ .drop_count = 1, .keep_rest = false };
    if (mem.eql(u8, name, "hphantom")) return .{ .drop_count = 1, .keep_rest = false };
    if (mem.eql(u8, name, "vphantom")) return .{ .drop_count = 1, .keep_rest = false };
    if (mem.eql(u8, name, "phantom")) return .{ .drop_count = 1, .keep_rest = false };
    if (mem.eql(u8, name, "rule")) return .{ .drop_count = 2, .keep_rest = false };
    if (mem.eql(u8, name, "vrule")) return .{ .drop_count = 0, .keep_rest = false };
    if (mem.eql(u8, name, "hrule")) return .{ .drop_count = 0, .keep_rest = false };
    return null;
}

fn handleFirstArgDrop(b: *Builder, source: []const u8, idx: usize, spec: FirstArgDropSpec) !usize {
    var i = skipOptionalArgs(source, idx);
    var dropped: u8 = 0;
    while (dropped < spec.drop_count) : (dropped += 1) {
        i = skipOptionalArgs(source, i);
        if (i >= source.len or source[i] != '{') break;
        const g = readGroup(source, i);
        i = g.end;
    }
    // Now: either render subsequent groups or eat them.
    if (spec.keep_rest) {
        return try eatAndRenderTrailingGroups(b, source, i);
    }
    return eatTrailingGroups(source, i);
}

fn openSection(b: *Builder, source: []const u8, idx: usize, depth: u8) !usize {
    const g = readGroup(source, idx);
    // Snapshot previous section.
    try b.flushSection();
    b.cur_depth = depth;
    var title_buf: ArrayList(u8) = .empty;
    defer title_buf.deinit(b.arena);
    try renderInto(b, &title_buf, g.body);
    try b.cur_title.appendSlice(b.arena, title_buf.items);
    b.have_section = true;
    return g.end;
}

// ---------- Math emission ------------------------------------------------

fn emitInlineMath(b: *Builder, inner: []const u8) !void {
    try b.recordMath("inline", inner);
    try b.outputBuf().append(b.arena, '$');
    try b.outputBuf().appendSlice(b.arena, inner);
    try b.outputBuf().append(b.arena, '$');
}

fn emitDisplayMath(b: *Builder, inner: []const u8, env: []const u8) !void {
    try b.recordMath(env, inner);
    try b.outputBuf().appendSlice(b.arena, "\n$$");
    try b.outputBuf().appendSlice(b.arena, inner);
    try b.outputBuf().appendSlice(b.arena, "$$\n");
}

// ---------- Environment handling ----------------------------------------

fn handleEnv(b: *Builder, source: []const u8, idx: usize, opening: bool) !usize {
    const g = readGroup(source, idx);
    const name_full = trim(g.body);
    const name = stripStar(name_full);
    if (!opening) {
        // \end{X} on its own — usually we already consumed the body. But
        // if encountered (e.g. mismatched), just skip.
        return g.end;
    }

    if (mem.eql(u8, name, "abstract")) {
        const inner = findEnvBody(source, g.end, name_full);
        var buf: ArrayList(u8) = .empty;
        defer buf.deinit(b.arena);
        // (No prior output redirect needed; renderEnvBody handles it.)
        // Render into a fresh buffer by temporarily redirecting through a sub-builder is overkill.
        // Easier: walk into `buf` directly via a transient builder.
        try renderEnvBody(b, &buf, inner.body);
        try b.abstract.appendSlice(b.arena, buf.items);
        return inner.end;
    }
    if (mem.eql(u8, name, "document")) {
        // Just keep walking inside. No-op marker.
        return g.end;
    }
    if (mem.eql(u8, name, "equation") or mem.eql(u8, name, "align") or
        mem.eql(u8, name, "eqnarray") or mem.eql(u8, name, "gather") or
        mem.eql(u8, name, "multline") or mem.eql(u8, name, "displaymath") or
        mem.eql(u8, name, "math") or mem.eql(u8, name, "flalign"))
    {
        const inner = findEnvBody(source, g.end, name_full);
        try emitDisplayMath(b, inner.body, name);
        return inner.end;
    }
    if (mem.eql(u8, name, "verbatim") or mem.eql(u8, name, "lstlisting") or
        mem.eql(u8, name, "minted"))
    {
        const inner = findEnvBody(source, g.end, name_full);
        try b.outputBuf().append(b.arena, '\n');
        try b.outputBuf().appendSlice(b.arena, inner.body);
        try b.outputBuf().append(b.arena, '\n');
        return inner.end;
    }
    if (mem.eql(u8, name, "figure") or mem.eql(u8, name, "figure*") or
        mem.eql(u8, name, "subfigure") or mem.eql(u8, name, "wrapfigure"))
    {
        const inner = findEnvBody(source, g.end, name_full);
        try extractFigure(b, inner.body);
        return inner.end;
    }
    if (mem.eql(u8, name, "table") or mem.eql(u8, name, "table*") or
        mem.eql(u8, name, "longtable"))
    {
        const inner = findEnvBody(source, g.end, name_full);
        // Extract caption/label → figures array, AND render any tabular
        // body inline so the cell text lands in the surrounding section.
        try extractTableCaption(b, inner.body);
        try renderTableBody(b, inner.body);
        return inner.end;
    }
    if (mem.eql(u8, name, "tabular") or mem.eql(u8, name, "tabularx") or
        mem.eql(u8, name, "tabulary") or mem.eql(u8, name, "array"))
    {
        // Bare tabular with no surrounding table env — strip column spec
        // (first brace arg) and render rows inline.
        var after_spec = g.end;
        after_spec = skipOptionalArgs(source, after_spec);
        // tabularx takes a width arg first ({width}{colspec}).
        const takes_extra = mem.eql(u8, name, "tabularx") or mem.eql(u8, name, "tabulary");
        if (takes_extra) {
            after_spec = skipOptionalArgs(source, after_spec);
            if (after_spec < source.len and source[after_spec] == '{') {
                const ext = readGroup(source, after_spec);
                after_spec = ext.end;
            }
        }
        after_spec = skipOptionalArgs(source, after_spec);
        if (after_spec < source.len and source[after_spec] == '{') {
            const spec = readGroup(source, after_spec);
            after_spec = spec.end;
        }
        const inner = findEnvBody(source, after_spec, name_full);
        try renderTabularRows(b, inner.body);
        return inner.end;
    }
    if (mem.eql(u8, name, "itemize") or mem.eql(u8, name, "enumerate") or
        mem.eql(u8, name, "description")) {
        // Render body inline; \item handler will emit bullets.
        try b.outputBuf().append(b.arena, '\n');
        return g.end;
    }
    if (mem.eql(u8, name, "thebibliography")) {
        const inner = findEnvBody(source, g.end, name_full);
        // Strip — bib goes through a separate pipeline.
        return inner.end;
    }
    if (mem.eql(u8, name, "comment") or mem.eql(u8, name, "ignore")) {
        const inner = findEnvBody(source, g.end, name_full);
        return inner.end;
    }
    // Generic environment: drop \begin/\end but keep body.
    return g.end;
}

fn extractFigure(b: *Builder, body: []const u8) !void {
    var fig: Figure = .{};
    // Find \caption{...} and \label{...}.
    if (findCommandArg(body, "caption")) |cap| {
        var buf: ArrayList(u8) = .empty;
        defer buf.deinit(b.arena);
        try renderEnvBody(b, &buf, cap);
        fig.caption = try b.arena.dupe(u8, trim(buf.items));
    }
    if (findCommandArg(body, "label")) |lbl| {
        fig.label = try b.arena.dupe(u8, trim(lbl));
    }
    // Collect every \includegraphics{src} inside the env. Sub-figure layouts
    // (\begin{subfigure}{...} ... \includegraphics{...}) and figures with
    // multiple includegraphics commands both produce >1 src — downstream
    // (r2g/internal/figures) is responsible for resolving each one against
    // the tarball and creating one document_figures row per path.
    fig.src_paths = try collectIncludegraphics(b.arena, body);
    try b.figures.append(b.arena, fig);
}

// Iterate every `\includegraphics[...]{path}` in `body` and return the
// path arguments in source order. Skips prefix-match false positives like
// `\includegraphicsx` via the same alpha-after-cmd check that findCommandArg
// uses.
fn collectIncludegraphics(arena: Allocator, body: []const u8) ![][]const u8 {
    const cmd = "includegraphics";
    var out: ArrayList([]const u8) = .empty;
    var i: usize = 0;
    while (i < body.len) : (i += 1) {
        if (body[i] != '\\') continue;
        if (i + 1 + cmd.len > body.len) break;
        if (!mem.eql(u8, body[i + 1 .. i + 1 + cmd.len], cmd)) continue;
        const after_cmd = i + 1 + cmd.len;
        if (after_cmd < body.len) {
            const nx = body[after_cmd];
            if (std.ascii.isAlphabetic(nx)) continue;
        }
        const start = skipOptionalArgs(body, after_cmd);
        if (start >= body.len or body[start] != '{') {
            i = after_cmd - 1;
            continue;
        }
        const g = readGroup(body, start);
        const src = trim(g.body);
        if (src.len > 0) {
            try out.append(arena, try arena.dupe(u8, src));
        }
        i = g.end - 1;
    }
    return out.toOwnedSlice(arena);
}

fn extractTableCaption(b: *Builder, body: []const u8) !void {
    if (findCommandArg(body, "caption")) |cap| {
        var buf: ArrayList(u8) = .empty;
        defer buf.deinit(b.arena);
        try renderEnvBody(b, &buf, cap);
        // Tables: surface as a figure-like caption so downstream sees it.
        try b.figures.append(b.arena, .{
            .label = if (findCommandArg(body, "label")) |l| try b.arena.dupe(u8, trim(l)) else "",
            .caption = try b.arena.dupe(u8, trim(buf.items)),
        });
    }
}

// Walk through `body`, find every `tabular`-family `\begin{...}` and render
// each one's rows into the active output. Other content (captions, labels,
// `\vspace`, etc.) is ignored — captions are already extracted upstream.
fn renderTableBody(b: *Builder, body: []const u8) !void {
    var i: usize = 0;
    while (i < body.len) : (i += 1) {
        if (body[i] != '\\') continue;
        if (!mem.startsWith(u8, body[i..], "\\begin")) continue;
        const after = i + "\\begin".len;
        const g = readGroup(body, after);
        const ename = stripStar(trim(g.body));
        const is_tabular = mem.eql(u8, ename, "tabular") or
            mem.eql(u8, ename, "tabularx") or
            mem.eql(u8, ename, "tabulary") or
            mem.eql(u8, ename, "array");
        if (!is_tabular) {
            i = g.end - 1;
            continue;
        }
        var after_spec = g.end;
        after_spec = skipOptionalArgs(body, after_spec);
        const takes_extra = mem.eql(u8, ename, "tabularx") or mem.eql(u8, ename, "tabulary");
        if (takes_extra) {
            after_spec = skipOptionalArgs(body, after_spec);
            if (after_spec < body.len and body[after_spec] == '{') {
                const ext = readGroup(body, after_spec);
                after_spec = ext.end;
            }
        }
        after_spec = skipOptionalArgs(body, after_spec);
        if (after_spec < body.len and body[after_spec] == '{') {
            const spec = readGroup(body, after_spec);
            after_spec = spec.end;
        }
        const inner = findEnvBody(body, after_spec, g.body);
        try b.outputBuf().append(b.arena, '\n');
        try renderTabularRows(b, inner.body);
        i = inner.end - 1;
    }
}

// Render a tabular environment body: cells joined by " | ", rows on their
// own lines. Cells are rendered through the normal walker so embedded math,
// styling, citations etc. survive.
fn renderTabularRows(b: *Builder, body: []const u8) !void {
    // Split by row terminator (\\) at top level (depth 0 of brace nesting).
    // Within each row, split cells by `&` at top level. Use temporary buffers
    // to render each cell through the parser.
    var row_buf: ArrayList(u8) = .empty;
    defer row_buf.deinit(b.arena);
    var cell_buf: ArrayList(u8) = .empty;
    defer cell_buf.deinit(b.arena);

    var emitted_any_row = false;
    var any_cell_in_row = false;
    var row_start: usize = 0;
    var i: usize = 0;
    while (i < body.len) {
        // Track brace depth so '&' / '\\' inside groups (e.g. multicolumn)
        // are not row/cell delimiters.
        if (body[i] == '%') {
            // skip comment to EOL
            while (i < body.len and body[i] != '\n') : (i += 1) {}
            continue;
        }
        if (body[i] == '\\') {
            // escape or command — check for row terminator \\
            if (i + 1 < body.len and body[i + 1] == '\\') {
                // emit pending cell, then row, then reset.
                try emitTabularCell(b, &row_buf, &cell_buf, body[row_start..i], &any_cell_in_row);
                try flushTabularRow(b, &row_buf, &emitted_any_row, &any_cell_in_row);
                i += 2;
                // skip optional [..] after \\
                if (i < body.len and body[i] == '[') i = skipBracket(body, i);
                row_start = i;
                continue;
            }
            // Skip any other command up to the end of its name to avoid
            // mis-parsing things like `\&` (which we DO want as a literal
            // ampersand in a cell — but the walker handles that during
            // cell rendering). We just advance one to skip the escape.
            i += 2;
            continue;
        }
        if (body[i] == '{') {
            // skip a balanced group
            const g = readGroup(body, i);
            i = g.end;
            continue;
        }
        if (body[i] == '&') {
            // cell separator at depth 0
            try emitTabularCell(b, &row_buf, &cell_buf, body[row_start..i], &any_cell_in_row);
            i += 1;
            row_start = i;
            continue;
        }
        i += 1;
    }
    // Trailing cell / row.
    if (row_start < body.len) {
        try emitTabularCell(b, &row_buf, &cell_buf, body[row_start..body.len], &any_cell_in_row);
    }
    if (any_cell_in_row) {
        try flushTabularRow(b, &row_buf, &emitted_any_row, &any_cell_in_row);
    }
}

fn emitTabularCell(
    b: *Builder,
    row_buf: *ArrayList(u8),
    cell_buf: *ArrayList(u8),
    raw_cell: []const u8,
    any_cell_in_row: *bool,
) !void {
    cell_buf.clearRetainingCapacity();
    // Skip cells that are pure horizontal-rule / formatting noise.
    if (isTabularNoise(raw_cell)) {
        // Still count the cell for "& produces a cell boundary" semantics —
        // emit empty cell so column structure is preserved when prefixed.
        if (any_cell_in_row.*) {
            try row_buf.appendSlice(b.arena, " | ");
        }
        any_cell_in_row.* = true;
        return;
    }
    try renderInto(b, cell_buf, raw_cell);
    // Compress whitespace within a cell to single spaces; trim ends.
    const compact = try compactWhitespace(b.arena, cell_buf.items);
    if (any_cell_in_row.*) {
        try row_buf.appendSlice(b.arena, " | ");
    }
    try row_buf.appendSlice(b.arena, compact);
    any_cell_in_row.* = true;
}

fn flushTabularRow(
    b: *Builder,
    row_buf: *ArrayList(u8),
    emitted_any_row: *bool,
    any_cell_in_row: *bool,
) !void {
    const trimmed = trim(row_buf.items);
    if (trimmed.len > 0) {
        if (!emitted_any_row.*) {
            try b.outputBuf().append(b.arena, '\n');
            emitted_any_row.* = true;
        }
        try b.outputBuf().appendSlice(b.arena, trimmed);
        try b.outputBuf().append(b.arena, '\n');
    }
    row_buf.clearRetainingCapacity();
    any_cell_in_row.* = false;
}

fn isTabularNoise(raw: []const u8) bool {
    const t = trim(raw);
    if (t.len == 0) return true;
    // Common pure-rule commands that contribute no content.
    const noise = [_][]const u8{
        "\\hline",
        "\\toprule",
        "\\midrule",
        "\\bottomrule",
        "\\cline",
        "\\cmidrule",
        "\\specialrule",
        "\\rule",
    };
    for (noise) |n| {
        if (mem.startsWith(u8, t, n)) {
            // accept only if the rest is whitespace / brackets / braces /
            // numbers / dashes (i.e. the line is _only_ this rule command).
            const rest = t[n.len..];
            if (isRuleTail(rest)) return true;
        }
    }
    return false;
}

fn isRuleTail(s: []const u8) bool {
    for (s) |c| {
        switch (c) {
            ' ', '\t', '\n', '\r', '[', ']', '{', '}', '-', '0'...'9', '.', 'p', 't', 'e', 'm', 'x' => continue,
            else => return false,
        }
    }
    return true;
}

fn compactWhitespace(arena: Allocator, s: []const u8) ![]const u8 {
    var out: ArrayList(u8) = .empty;
    var prev_space = true;
    for (s) |c| {
        if (c == ' ' or c == '\t' or c == '\n' or c == '\r') {
            if (!prev_space) {
                try out.append(arena, ' ');
                prev_space = true;
            }
        } else {
            try out.append(arena, c);
            prev_space = false;
        }
    }
    // trim trailing space
    var n = out.items.len;
    while (n > 0 and out.items[n - 1] == ' ') : (n -= 1) {}
    return out.items[0..n];
}

// renderInto parses `inner` and appends the result into `out`. We redirect
// the builder's `target` so all nested walks land in `out`. Math counts
// and figures still roll up to the outer builder.
fn renderInto(b: *Builder, out: *ArrayList(u8), inner: []const u8) !void {
    const prev_target = b.target;
    defer b.target = prev_target;
    b.target = out;
    try walk(b, inner);
}

// Alias retained for readability at call sites that "render an env body".
const renderEnvBody = renderInto;

// ---------- Authors / parsing helpers -----------------------------------

fn parseAuthors(b: *Builder, raw: []const u8) !void {
    // Author splitting: arxiv/NIPS/ACL-style \author{...} blocks use
    // \And or \AND between authors, with each author block holding name +
    // affiliation + email on separate lines. We split on those markers
    // (and on 3+ consecutive newlines as a fallback), then keep the first
    // non-affiliation/non-email line of each block as the canonical name.
    //
    // We substitute the markers with an ASCII unit separator BEFORE the
    // generic render pass so they survive command-eating.
    var pre: ArrayList(u8) = .empty;
    defer pre.deinit(b.arena);
    var j: usize = 0;
    while (j < raw.len) {
        // Detect "\And", "\AND", "\and" as block separators when the
        // following byte is non-alphabetic.
        if (raw[j] == '\\' and j + 3 < raw.len) {
            const tail = raw[j + 1 ..];
            const is_and = mem.startsWith(u8, tail, "and") or
                mem.startsWith(u8, tail, "And") or
                mem.startsWith(u8, tail, "AND");
            if (is_and) {
                const stop = j + 1 + 3;
                if (stop == raw.len or !std.ascii.isAlphabetic(raw[stop])) {
                    try pre.append(b.arena, '\x1f');
                    j = stop;
                    continue;
                }
            }
        }
        // Detect 3+ consecutive newlines.
        if (raw[j] == '\n') {
            var n: usize = 0;
            var k = j;
            while (k < raw.len and (raw[k] == '\n' or raw[k] == '\r' or raw[k] == ' ' or raw[k] == '\t')) : (k += 1) {
                if (raw[k] == '\n') n += 1;
            }
            if (n >= 3) {
                try pre.append(b.arena, '\x1f');
                j = k;
                continue;
            }
        }
        try pre.append(b.arena, raw[j]);
        j += 1;
    }
    var buf: ArrayList(u8) = .empty;
    defer buf.deinit(b.arena);
    try renderInto(b, &buf, pre.items);

    var it = std.mem.splitScalar(u8, buf.items, '\x1f');
    while (it.next()) |block| {
        const name = pickAuthorName(block) orelse continue;
        const owned = try b.arena.dupe(u8, name);
        try b.authors.append(b.arena, owned);
    }
}

// Given a multi-line author block, return the first line that looks like
// a person's name (not an email, affiliation, or blank).
fn pickAuthorName(block: []const u8) ?[]const u8 {
    var line_it = std.mem.splitScalar(u8, block, '\n');
    while (line_it.next()) |raw_line| {
        const line = trim(raw_line);
        if (line.len == 0) continue;
        if (looksLikeAffiliationOrEmail(line)) continue;
        // Trim a trailing comma if any (e.g. "Alice Example,").
        var end = line.len;
        while (end > 0 and (line[end - 1] == ',' or line[end - 1] == ';' or line[end - 1] == '.')) : (end -= 1) {}
        if (end == 0) continue;
        return line[0..end];
    }
    return null;
}

fn looksLikeAffiliationOrEmail(line: []const u8) bool {
    // Email-ish: contains @ with a dot afterwards.
    if (mem.indexOfScalar(u8, line, '@')) |at| {
        if (mem.indexOfScalar(u8, line[at..], '.') != null) return true;
    }
    // Common affiliation prefixes (case-insensitive at the start).
    const prefixes = [_][]const u8{
        "google",
        "microsoft",
        "facebook",
        "meta",
        "amazon",
        "apple",
        "ibm",
        "openai",
        "deepmind",
        "anthropic",
        "nvidia",
        "university of",
        "university,",
        "department of",
        "department,",
        "school of",
        "institute of",
        "college of",
        "laboratory",
        "labs",
        "research lab",
        "research,",
        "research center",
        "research institute",
        "centre for",
        "center for",
        "stanford",
        "berkeley",
        "carnegie mellon",
        "mit ",
        "mit,",
        "eth ",
        "epfl",
        "inria",
        "max planck",
    };
    for (prefixes) |p| {
        if (asciiStartsWithIgnoreCase(line, p)) return true;
    }
    // Lines wholly inside \texttt{...} are usually emails/handles — but our
    // walker already strips the wrapper, so we check for the typical shape
    // of one bare token containing '@' (handled above) or a URL.
    if (mem.startsWith(u8, line, "http://") or mem.startsWith(u8, line, "https://")) return true;
    return false;
}

fn asciiStartsWithIgnoreCase(haystack: []const u8, needle: []const u8) bool {
    if (haystack.len < needle.len) return false;
    var i: usize = 0;
    while (i < needle.len) : (i += 1) {
        const a = std.ascii.toLower(haystack[i]);
        const b = std.ascii.toLower(needle[i]);
        if (a != b) return false;
    }
    return true;
}

// ---------- Low-level scanners ------------------------------------------

const Group = struct { body: []const u8, end: usize };

fn readGroup(source: []const u8, idx: usize) Group {
    // Skip whitespace.
    var i = idx;
    while (i < source.len and isSpace(source[i])) : (i += 1) {}
    if (i >= source.len or source[i] != '{') return .{ .body = "", .end = i };
    var depth: usize = 1;
    const start = i + 1;
    i = start;
    while (i < source.len and depth > 0) : (i += 1) {
        const c = source[i];
        if (c == '\\') {
            // Skip the next char (escape).
            i += 1;
            continue;
        }
        if (c == '{') depth += 1 else if (c == '}') {
            depth -= 1;
            if (depth == 0) return .{ .body = source[start..i], .end = i + 1 };
        }
    }
    return .{ .body = source[start..source.len], .end = source.len };
}

fn skipOptionalArgs(source: []const u8, idx: usize) usize {
    var i = idx;
    while (i < source.len) {
        // skip whitespace
        while (i < source.len and isSpace(source[i])) : (i += 1) {}
        if (i >= source.len or source[i] != '[') break;
        i = skipBracket(source, i);
    }
    return i;
}

fn skipBracket(source: []const u8, idx: usize) usize {
    var i = idx;
    if (i >= source.len or source[i] != '[') return i;
    i += 1;
    var depth: usize = 1;
    while (i < source.len and depth > 0) : (i += 1) {
        if (source[i] == '\\') {
            i += 1;
            continue;
        }
        if (source[i] == '[') depth += 1 else if (source[i] == ']') depth -= 1;
    }
    return i;
}

// \newcommand and friends look like:
//   \newcommand{\foo}{body}
//   \newcommand\foo[1]{body}
//   \newcommand*\foo[1][default]{body}
// The "name" arg can be either a brace group OR a bare \cmd. We have to
// recognise both, then swallow the optional [n][default] params and the
// body brace group.
fn skipMacroDefinition(source: []const u8, idx: usize) usize {
    var i = idx;
    while (i < source.len and isSpace(source[i])) : (i += 1) {}
    // Name: either {\foo} or \foo.
    if (i < source.len and source[i] == '{') {
        const g = readGroup(source, i);
        i = g.end;
    } else if (i < source.len and source[i] == '\\') {
        i += 1;
        while (i < source.len and (std.ascii.isAlphabetic(source[i]) or source[i] == '@')) : (i += 1) {}
        if (i < source.len and source[i] == '*') i += 1;
    }
    // optional param spec and default(s)
    i = skipOptionalArgs(source, i);
    // body brace group
    if (i < source.len) {
        while (i < source.len and isSpace(source[i])) : (i += 1) {}
        if (i < source.len and source[i] == '{') {
            const g = readGroup(source, i);
            i = g.end;
        }
    }
    return i;
}

fn eatTrailingGroups(source: []const u8, idx: usize) usize {
    var i = skipOptionalArgs(source, idx);
    while (i < source.len) {
        i = skipOptionalArgs(source, i);
        if (i >= source.len or source[i] != '{') break;
        const g = readGroup(source, i);
        i = g.end;
    }
    return i;
}

fn eatAndRenderTrailingGroups(b: *Builder, source: []const u8, idx: usize) !usize {
    var i = skipOptionalArgs(source, idx);
    var any = false;
    while (i < source.len) {
        i = skipOptionalArgs(source, i);
        if (i >= source.len or source[i] != '{') break;
        const g = readGroup(source, i);
        if (any) try b.outputBuf().append(b.arena, ' ');
        try renderInto(b, b.outputBuf(), g.body);
        i = g.end;
        any = true;
    }
    return i;
}

const EnvBody = struct { body: []const u8, end: usize };

fn findEnvBody(source: []const u8, start: usize, env_name: []const u8) EnvBody {
    // Look for matching \end{env_name}, respecting nesting.
    var depth: usize = 1;
    var i = start;
    const begin_marker = "\\begin";
    const end_marker = "\\end";
    while (i < source.len) {
        if (source[i] == '\\') {
            if (mem.startsWith(u8, source[i..], begin_marker)) {
                const after = i + begin_marker.len;
                const g = readGroup(source, after);
                if (mem.eql(u8, stripStar(trim(g.body)), stripStar(env_name))) depth += 1;
                i = g.end;
                continue;
            }
            if (mem.startsWith(u8, source[i..], end_marker)) {
                const after = i + end_marker.len;
                const g = readGroup(source, after);
                if (mem.eql(u8, stripStar(trim(g.body)), stripStar(env_name))) {
                    depth -= 1;
                    if (depth == 0) return .{ .body = source[start..i], .end = g.end };
                }
                i = g.end;
                continue;
            }
        }
        i += 1;
    }
    return .{ .body = source[start..source.len], .end = source.len };
}

fn findCommandArg(body: []const u8, cmd: []const u8) ?[]const u8 {
    var i: usize = 0;
    while (i < body.len) : (i += 1) {
        if (body[i] != '\\') continue;
        if (i + 1 + cmd.len > body.len) break;
        if (!mem.eql(u8, body[i + 1 .. i + 1 + cmd.len], cmd)) continue;
        // Make sure it's not a prefix match (e.g. \captionof when we want \caption).
        const after_cmd = i + 1 + cmd.len;
        if (after_cmd < body.len) {
            const nx = body[after_cmd];
            if (std.ascii.isAlphabetic(nx)) continue;
        }
        const start = skipOptionalArgs(body, after_cmd);
        const g = readGroup(body, start);
        return g.body;
    }
    return null;
}

fn findClosingDisplayMath(source: []const u8, start: usize) usize {
    var i = start;
    while (i < source.len) : (i += 1) {
        if (source[i] == '\\' and i + 1 < source.len and source[i + 1] == ']') return i + 2;
    }
    return source.len;
}

fn findDollarDisplay(source: []const u8, start: usize) usize {
    var i = start;
    while (i + 1 < source.len) : (i += 1) {
        if (source[i] == '\\') {
            i += 1;
            continue;
        }
        if (source[i] == '$' and source[i + 1] == '$') return i + 2;
    }
    return source.len;
}

fn findDollar(source: []const u8, start: usize) usize {
    var i = start;
    while (i < source.len) : (i += 1) {
        if (source[i] == '\\') {
            i += 1;
            continue;
        }
        if (source[i] == '$') return i + 1;
    }
    return source.len;
}

// ---------- Tiny utils --------------------------------------------------

fn isSpace(c: u8) bool {
    return c == ' ' or c == '\t' or c == '\n' or c == '\r';
}

fn isEscapedLiteral(c: u8) bool {
    return switch (c) {
        '%', '&', '$', '_', '#', '{', '}', '~', '^' => true,
        else => false,
    };
}

fn stripStar(cmd: []const u8) []const u8 {
    if (cmd.len > 0 and cmd[cmd.len - 1] == '*') return cmd[0 .. cmd.len - 1];
    return cmd;
}

fn trim(s: []const u8) []const u8 {
    return std.mem.trim(u8, s, " \t\n\r");
}

fn isPassthroughCommand(name: []const u8) bool {
    const list = [_][]const u8{
        "textbf",       "textit",     "emph",         "underline",
        "textsf",       "texttt",     "textrm",       "textsc",
        "textsuperscript", "textsubscript",
        "uppercase",    "lowercase",  "mbox",         "fbox",
        "textnormal",   "textmd",     "textup",       "textsl",
        "text",         "mathrm",     "mathbf",       "mathit",
        "mathsf",       "mathcal",    "mathbb",       "mathfrak",
        "boldsymbol",   "operatorname",
    };
    inline for (list) |c| {
        if (mem.eql(u8, name, c)) return true;
    }
    return false;
}

// ---------- Inline tests ------------------------------------------------

test "headline & abstract extraction" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const src =
        \\\documentclass{article}
        \\\title{Hello \textbf{World}}
        \\\author{A. Author \and B. Author}
        \\\begin{document}
        \\\maketitle
        \\\begin{abstract}
        \\This is the abstract. It has \cite{Foo} a citation.
        \\\end{abstract}
        \\\section{Intro}
        \\Body of intro with math $x^2$ and \ref{eq:1}.
        \\\section{Method}
        \\Another body.
        \\\end{document}
    ;
    const doc = try parse(arena.allocator(), src, .{});
    try std.testing.expect(std.mem.indexOf(u8, doc.title, "Hello") != null);
    try std.testing.expect(std.mem.indexOf(u8, doc.title, "World") != null);
    try std.testing.expect(std.mem.indexOf(u8, doc.abstract, "abstract") != null);
    try std.testing.expect(std.mem.indexOf(u8, doc.abstract, "[CITE]") != null);
    try std.testing.expect(doc.sections.len == 2);
    try std.testing.expectEqualStrings("Intro", doc.sections[0].title);
    try std.testing.expectEqualStrings("Method", doc.sections[1].title);
    try std.testing.expect(doc.math_placeholders >= 1);
    // Math source is now preserved inline.
    try std.testing.expect(std.mem.indexOf(u8, doc.sections[0].body, "$x^2$") != null);
}

test "figure capture" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const src =
        \\\section{X}
        \\\begin{figure}
        \\\includegraphics{foo.pdf}
        \\\caption{An arch diagram.}
        \\\label{fig:arch}
        \\\end{figure}
    ;
    const doc = try parse(arena.allocator(), src, .{});
    try std.testing.expect(doc.figures.len == 1);
    try std.testing.expectEqualStrings("fig:arch", doc.figures[0].label);
    try std.testing.expect(std.mem.indexOf(u8, doc.figures[0].caption, "arch diagram") != null);
}

test "unknown command keeps body" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const src = "\\section{X}\n A \\unknowncmd{kept text} B.";
    const doc = try parse(arena.allocator(), src, .{});
    try std.testing.expect(std.mem.indexOf(u8, doc.sections[0].body, "kept text") != null);
}

test "verbatim preserved" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const src = "\\section{X}\n\\begin{verbatim}\nlet x = 1;\n\\end{verbatim}";
    const doc = try parse(arena.allocator(), src, .{});
    try std.testing.expect(std.mem.indexOf(u8, doc.sections[0].body, "let x = 1;") != null);
}

test "em / en dash" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const src = "\\section{X}\na---b and c--d.";
    const doc = try parse(arena.allocator(), src, .{});
    try std.testing.expect(std.mem.indexOf(u8, doc.sections[0].body, "\u{2014}") != null);
    try std.testing.expect(std.mem.indexOf(u8, doc.sections[0].body, "\u{2013}") != null);
}

test "escaped percent literal" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const src = "\\section{X}\nWe used 50\\% of the budget. % real comment\nMore text.";
    const doc = try parse(arena.allocator(), src, .{});
    try std.testing.expect(std.mem.indexOf(u8, doc.sections[0].body, "50%") != null);
    try std.testing.expect(std.mem.indexOf(u8, doc.sections[0].body, "real comment") == null);
}

test "inline math source preserved" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const src = "\\section{X}\nWe scale by $\\sqrt{d_k}$ here.";
    const doc = try parse(arena.allocator(), src, .{});
    try std.testing.expect(std.mem.indexOf(u8, doc.sections[0].body, "$\\sqrt{d_k}$") != null);
    try std.testing.expect(doc.math_placeholders == 1);
    try std.testing.expect(doc.math.len == 1);
    try std.testing.expectEqualStrings("inline", doc.math[0].env);
}

test "display math source preserved with $$" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const src =
        \\\section{X}
        \\Then:
        \\\begin{equation}
        \\E = mc^2
        \\\end{equation}
        \\Done.
    ;
    const doc = try parse(arena.allocator(), src, .{});
    try std.testing.expect(std.mem.indexOf(u8, doc.sections[0].body, "$$") != null);
    try std.testing.expect(std.mem.indexOf(u8, doc.sections[0].body, "E = mc^2") != null);
    try std.testing.expectEqualStrings("equation", doc.math[0].env);
}

test "display math via \\[ ... \\] preserved" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const src = "\\section{X}\nLook: \\[ a + b = c \\] done.";
    const doc = try parse(arena.allocator(), src, .{});
    try std.testing.expect(std.mem.indexOf(u8, doc.sections[0].body, "$$") != null);
    try std.testing.expect(std.mem.indexOf(u8, doc.sections[0].body, "a + b = c") != null);
}

test "textcolor drops color name, keeps body" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const src = "\\section{X}\nA \\textcolor{red}{hello world} B.";
    const doc = try parse(arena.allocator(), src, .{});
    try std.testing.expect(std.mem.indexOf(u8, doc.sections[0].body, "hello world") != null);
    try std.testing.expect(std.mem.indexOf(u8, doc.sections[0].body, "redhello") == null);
    try std.testing.expect(std.mem.indexOf(u8, doc.sections[0].body, "red hello") == null);
}

test "color drops the whole declaration" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const src = "\\section{X}\nNothing \\color{red} here.";
    const doc = try parse(arena.allocator(), src, .{});
    try std.testing.expect(std.mem.indexOf(u8, doc.sections[0].body, "red") == null);
    try std.testing.expect(std.mem.indexOf(u8, doc.sections[0].body, "Nothing") != null);
    try std.testing.expect(std.mem.indexOf(u8, doc.sections[0].body, "here.") != null);
}

test "authors split on And and drop affiliations" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const src =
        \\\author{
        \\Alice Example\\
        \\Google Brain\\
        \\\texttt{alice@google.com}\\
        \\\And
        \\Bob Sample\\
        \\University of Foo\\
        \\\texttt{bob@foo.edu}\\
        \\}
        \\\begin{document}
        \\\section{Intro}
        \\Body.
        \\\end{document}
    ;
    const doc = try parse(arena.allocator(), src, .{});
    try std.testing.expectEqual(@as(usize, 2), doc.authors.len);
    try std.testing.expectEqualStrings("Alice Example", doc.authors[0]);
    try std.testing.expectEqualStrings("Bob Sample", doc.authors[1]);
}

test "tabular rows rendered as pipe-joined lines" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const src =
        \\\section{X}
        \\\begin{tabular}{lcc}
        \\Model & BLEU & FLOPs \\
        \\GNMT & 24.6 & 1.4e20 \\
        \\Transformer & 28.4 & 3.3e18 \\
        \\\end{tabular}
    ;
    const doc = try parse(arena.allocator(), src, .{});
    try std.testing.expect(std.mem.indexOf(u8, doc.sections[0].body, "Model | BLEU | FLOPs") != null);
    try std.testing.expect(std.mem.indexOf(u8, doc.sections[0].body, "Transformer | 28.4 | 3.3e18") != null);
}

test "table env renders both caption and inner tabular" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const src =
        \\\section{X}
        \\\begin{table}
        \\\caption{Results}
        \\\label{tab:r}
        \\\begin{tabular}{lc}
        \\A & 1 \\
        \\B & 2 \\
        \\\end{tabular}
        \\\end{table}
    ;
    const doc = try parse(arena.allocator(), src, .{});
    try std.testing.expect(doc.figures.len == 1);
    try std.testing.expectEqualStrings("tab:r", doc.figures[0].label);
    try std.testing.expect(std.mem.indexOf(u8, doc.sections[0].body, "A | 1") != null);
    try std.testing.expect(std.mem.indexOf(u8, doc.sections[0].body, "B | 2") != null);
}
