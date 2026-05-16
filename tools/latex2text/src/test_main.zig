//! Integration tests that exercise the parser against on-disk LaTeX
//! fixtures and assert structural properties of the result.

const std = @import("std");
const parser = @import("parser");
const tar_mod = @import("tar.zig");

fn loadFile(arena: std.mem.Allocator, comptime path: []const u8) ![]const u8 {
    return try loadFilePath(arena, path);
}

fn loadFilePath(arena: std.mem.Allocator, path: []const u8) ![]const u8 {
    // Resolve fixtures relative to the build root via the testing Io.
    const io = std.testing.io;
    const dir = std.Io.Dir.cwd();
    var f = try dir.openFile(io, path, .{});
    defer f.close(io);
    var buf: [64 * 1024]u8 = undefined;
    var r = f.reader(io, &buf);
    var out: std.ArrayList(u8) = .empty;
    var chunk: [64 * 1024]u8 = undefined;
    while (true) {
        const n = try r.interface.readSliceShort(&chunk);
        if (n == 0) break;
        try out.appendSlice(arena, chunk[0..n]);
        if (n < chunk.len) break;
    }
    return try out.toOwnedSlice(arena);
}

test "fixture: simple.tex" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const src = try loadFile(arena.allocator(), "tests/fixtures/simple.tex");
    const doc = try parser.parse(arena.allocator(), src, .{});
    try std.testing.expect(doc.sections.len >= 1);
    try std.testing.expect(std.mem.indexOf(u8, doc.title, "Simple Test") != null);
    try std.testing.expect(std.mem.indexOf(u8, doc.abstract, "abstract") != null);
}

test "fixture: with_math.tex" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const src = try loadFile(arena.allocator(), "tests/fixtures/with_math.tex");
    const doc = try parser.parse(arena.allocator(), src, .{});
    try std.testing.expect(doc.math_placeholders >= 2);
    try std.testing.expect(doc.figures.len >= 1);
    try std.testing.expect(std.mem.indexOf(u8, doc.sections[0].body, "[CITE]") != null);
    // v0.3: figures now carry src_paths for every \includegraphics in the env.
    // with_math.tex has a single figure pointing at "diagram.pdf".
    var found_diagram = false;
    for (doc.figures) |fig| {
        for (fig.src_paths) |p| {
            if (std.mem.eql(u8, p, "diagram.pdf")) found_diagram = true;
        }
    }
    try std.testing.expect(found_diagram);
}

test "figures: subfigure layout yields multiple src_paths" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const src =
        \\\documentclass{article}
        \\\usepackage{graphicx}
        \\\begin{document}
        \\\begin{figure}
        \\  \includegraphics[width=0.4\textwidth]{a.png}
        \\  \includegraphics[width=0.4\textwidth]{sub/b.pdf}
        \\  \caption{Two panels.}
        \\  \label{fig:multi}
        \\\end{figure}
        \\\end{document}
    ;
    const doc = try parser.parse(arena.allocator(), src, .{});
    try std.testing.expect(doc.figures.len >= 1);
    const fig = doc.figures[0];
    try std.testing.expectEqual(@as(usize, 2), fig.src_paths.len);
    try std.testing.expectEqualStrings("a.png", fig.src_paths[0]);
    try std.testing.expectEqualStrings("sub/b.pdf", fig.src_paths[1]);
}

test "fixture: edge_cases.tex" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const src = try loadFile(arena.allocator(), "tests/fixtures/edge_cases.tex");
    const doc = try parser.parse(arena.allocator(), src, .{});
    // unknown command body kept
    var body_join: std.ArrayList(u8) = .empty;
    defer body_join.deinit(arena.allocator());
    for (doc.sections) |sec| {
        try body_join.appendSlice(arena.allocator(), sec.body);
        try body_join.append(arena.allocator(), '\n');
    }
    try std.testing.expect(std.mem.indexOf(u8, body_join.items, "kept text") != null);
    try std.testing.expect(std.mem.indexOf(u8, body_join.items, "50%") != null);
    try std.testing.expect(std.mem.indexOf(u8, body_join.items, "\u{2014}") != null);
    // Issue 4: \textcolor must drop color name, keep body.
    try std.testing.expect(std.mem.indexOf(u8, body_join.items, "hello world") != null);
    try std.testing.expect(std.mem.indexOf(u8, body_join.items, "redhello") == null);
    try std.testing.expect(std.mem.indexOf(u8, body_join.items, "red hello") == null);
    // Issue 2: tabular cells joined by " | ".
    try std.testing.expect(std.mem.indexOf(u8, body_join.items, "Alpha | 10 | 1.0") != null);
    try std.testing.expect(std.mem.indexOf(u8, body_join.items, "Beta | 20 | 2.5") != null);
    // Polish Ł survives.
    try std.testing.expect(std.mem.indexOf(u8, body_join.items, "\u{0141}ukasz") != null);
}

test "fixture: multifile via resolver" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const aalloc = arena.allocator();
    const main_src = try loadFile(aalloc, "tests/fixtures/multifile/main.tex");
    const intro_src = try loadFile(aalloc, "tests/fixtures/multifile/intro.tex");
    const method_src = try loadFile(aalloc, "tests/fixtures/multifile/method.tex");

    var ctx = Resolver{ .arena = aalloc };
    try ctx.add("intro", intro_src);
    try ctx.add("method", method_src);

    const doc = try parser.parse(aalloc, main_src, .{
        .resolve_ctx = @ptrCast(&ctx),
        .resolve_include = Resolver.cb,
    });
    try std.testing.expect(doc.sections.len >= 2);
    var seen_intro = false;
    var seen_method = false;
    for (doc.sections) |s| {
        if (std.mem.indexOf(u8, s.title, "Intro") != null) seen_intro = true;
        if (std.mem.indexOf(u8, s.title, "Method") != null) seen_method = true;
    }
    try std.testing.expect(seen_intro);
    try std.testing.expect(seen_method);
}

// Integration test: parse the real Attention Is All You Need tarball
// (arxiv:1706.03762) end-to-end. We expect a cached copy at
// tests/cache/1706.03762.tar.gz; if it's missing the test is skipped so
// `zig build test` stays offline-clean.
test "attention paper end-to-end" {
    var arena = std.heap.ArenaAllocator.init(std.testing.allocator);
    defer arena.deinit();
    const aalloc = arena.allocator();
    const path = "tests/cache/1706.03762.tar.gz";
    const raw = loadFilePath(aalloc, path) catch |err| switch (err) {
        error.FileNotFound => return error.SkipZigTest,
        else => return err,
    };
    const arch = try tar_mod.loadTarball(aalloc, raw);
    const main_tex = arch.findMainTex() orelse return error.NoMainTex;
    const doc = try parser.parse(aalloc, main_tex.body, .{
        .resolve_ctx = @ptrCast(@constCast(&arch)),
        .resolve_include = resolveInArchive,
    });
    // Spec assertions from the bug report.
    try std.testing.expect(doc.sections.len >= 20);
    try std.testing.expect(doc.figures.len >= 5);
    try std.testing.expectEqual(@as(usize, 8), doc.authors.len);

    // Body should contain literal math source for sqrt{d_k}.
    var body_join: std.ArrayList(u8) = .empty;
    defer body_join.deinit(aalloc);
    for (doc.sections) |sec| {
        try body_join.appendSlice(aalloc, sec.body);
        try body_join.append(aalloc, '\n');
    }
    try std.testing.expect(std.mem.indexOf(u8, body_join.items, "\\sqrt{d_k}") != null);
    // No textcolor / color leakage.
    try std.testing.expect(std.mem.indexOf(u8, body_join.items, "redProvided") == null);
    // Authors don't include affiliations / emails.
    for (doc.authors) |a| {
        try std.testing.expect(std.mem.indexOfScalar(u8, a, '@') == null);
        try std.testing.expect(!std.mem.startsWith(u8, a, "Google"));
        try std.testing.expect(!std.mem.startsWith(u8, a, "University"));
    }
}

const ArchiveResolver = tar_mod.Archive;

fn resolveInArchive(ctx: ?*anyopaque, name: []const u8) ?[]const u8 {
    const arch: *const tar_mod.Archive = @ptrCast(@alignCast(ctx orelse return null));
    if (arch.findByBasename(name)) |e| return e.body;
    const stripped = if (std.mem.endsWith(u8, name, ".tex")) name[0 .. name.len - 4] else name;
    if (arch.findByBasename(stripped)) |e| return e.body;
    return null;
}

const Resolver = struct {
    arena: std.mem.Allocator,
    names: std.ArrayList([]const u8) = .empty,
    bodies: std.ArrayList([]const u8) = .empty,

    fn add(self: *Resolver, name: []const u8, body: []const u8) !void {
        try self.names.append(self.arena, name);
        try self.bodies.append(self.arena, body);
    }

    fn cb(ctx: ?*anyopaque, name: []const u8) ?[]const u8 {
        const self: *Resolver = @ptrCast(@alignCast(ctx orelse return null));
        for (self.names.items, self.bodies.items) |n, b| {
            if (std.mem.eql(u8, n, name)) return b;
            // also try with .tex
            const trimmed = if (std.mem.endsWith(u8, name, ".tex")) name[0 .. name.len - 4] else name;
            if (std.mem.eql(u8, n, trimmed)) return b;
        }
        return null;
    }
};
