//! latex2text — fast arxiv LaTeX → JSON converter.
//!
//! See README.md in this directory for design + usage.

const std = @import("std");
const parser = @import("parser");
const tar_mod = @import("tar.zig");

const Mode = enum { tarball_path, tarball_stdin, tex_path, tex_stdin };
const Out = enum { json, text };

const Args = struct {
    mode: Mode = .tarball_stdin,
    path: ?[]const u8 = null,
    out: Out = .json,
    strict: bool = false,
};

const help_text =
    \\latex2text [--from-tarball PATH | --from-stdin-tarball
    \\            | --from-tex FILE | --from-stdin-tex]
    \\           [--out json|text] [--strict | --lenient]
    \\
    \\Default: --from-stdin-tarball --out json --lenient
    \\Reads from stdin or the given path; writes JSON (or plain text) to stdout.
    \\
;

pub fn main(init: std.process.Init) !void {
    const gpa = init.gpa;
    const args = try parseArgs(init.minimal.args.iterate());
    const t_start = std.Io.Clock.awake.now(init.io);

    var arena = std.heap.ArenaAllocator.init(gpa);
    defer arena.deinit();
    const aalloc = arena.allocator();

    const stdin_file = std.Io.File.stdin();
    var stdout_buf: [16 * 1024]u8 = undefined;
    const stdout_file = std.Io.File.stdout();
    var stdout_writer = stdout_file.writer(init.io, &stdout_buf);
    const stdout = &stdout_writer.interface;

    var doc: parser.Document = undefined;
    var arch_holder: ?tar_mod.Archive = null;
    switch (args.mode) {
        .tex_path => {
            const path = args.path orelse return die(stdout, "missing --from-tex PATH\n");
            const data = try readFileAll(aalloc, init.io, path);
            doc = try parser.parse(aalloc, data, .{ .strict = args.strict });
        },
        .tex_stdin => {
            const data = try readStdinAll(aalloc, init.io, stdin_file);
            doc = try parser.parse(aalloc, data, .{ .strict = args.strict });
        },
        .tarball_path => {
            const path = args.path orelse return die(stdout, "missing --from-tarball PATH\n");
            const raw = try readFileAll(aalloc, init.io, path);
            arch_holder = try tar_mod.loadTarball(aalloc, raw);
            const arch = arch_holder.?;
            const main_tex = arch.findMainTex() orelse return die(stdout, "no main .tex file in tarball\n");
            doc = try parser.parse(aalloc, main_tex.body, .{
                .strict = args.strict,
                .resolve_ctx = @ptrCast(@constCast(&arch)),
                .resolve_include = resolveInclude,
            });
        },
        .tarball_stdin => {
            const raw = try readStdinAll(aalloc, init.io, stdin_file);
            arch_holder = try tar_mod.loadTarball(aalloc, raw);
            const arch = arch_holder.?;
            const main_tex = arch.findMainTex() orelse return die(stdout, "no main .tex file in tarball\n");
            doc = try parser.parse(aalloc, main_tex.body, .{
                .strict = args.strict,
                .resolve_ctx = @ptrCast(@constCast(&arch)),
                .resolve_include = resolveInclude,
            });
        },
    }

    const elapsed = t_start.untilNow(init.io, .awake);
    doc.elapsed_ms = @intCast(@max(0, elapsed.toMilliseconds()));

    switch (args.out) {
        .json => try writeJson(stdout, doc),
        .text => try writeText(stdout, doc),
    }
    try stdout.flush();
}

fn resolveInclude(ctx: ?*anyopaque, name: []const u8) ?[]const u8 {
    const arch: *const tar_mod.Archive = @ptrCast(@alignCast(ctx orelse return null));

    // TeX convention: `\input{X}` first tries `X.tex` if X has no
    // extension. Without this, papers that split content into
    // `Sections/0_abstract.tex` etc. with `\input{Sections/0_abstract}`
    // fail to resolve and the parser emits empty output.
    if (!std.mem.endsWith(u8, name, ".tex")) {
        var buf: [512]u8 = undefined;
        if (name.len + 4 <= buf.len) {
            @memcpy(buf[0..name.len], name);
            @memcpy(buf[name.len .. name.len + 4], ".tex");
            const with_tex = buf[0 .. name.len + 4];
            if (arch.findByPath(with_tex)) |e| return e.body;
        }
    }

    // Then the literal name (covers `\input{file.tex}` with extension
    // included).
    if (arch.findByPath(name)) |e| return e.body;
    if (arch.findByBasename(name)) |e| return e.body;

    // Final fallback: strip .tex extension if present and try as a
    // basename-only match. Preserves the v0.1 behaviour for tarballs
    // whose tex_files are flat (no Sections/ subdir).
    const stripped = if (std.mem.endsWith(u8, name, ".tex")) name[0 .. name.len - 4] else name;
    if (arch.findByBasename(stripped)) |e| return e.body;
    return null;
}

fn parseArgs(it_init: anytype) !Args {
    var args: Args = .{};
    var it = it_init;
    _ = it.skip(); // argv[0]
    while (it.next()) |raw| {
        const a = raw[0..raw.len];
        if (std.mem.eql(u8, a, "--from-tarball")) {
            args.mode = .tarball_path;
            if (it.next()) |p| args.path = p[0..p.len];
        } else if (std.mem.eql(u8, a, "--from-stdin-tarball")) {
            args.mode = .tarball_stdin;
        } else if (std.mem.eql(u8, a, "--from-tex")) {
            args.mode = .tex_path;
            if (it.next()) |p| args.path = p[0..p.len];
        } else if (std.mem.eql(u8, a, "--from-stdin-tex")) {
            args.mode = .tex_stdin;
        } else if (std.mem.eql(u8, a, "--out")) {
            if (it.next()) |v| {
                if (std.mem.eql(u8, v[0..v.len], "json")) args.out = .json;
                if (std.mem.eql(u8, v[0..v.len], "text")) args.out = .text;
            }
        } else if (std.mem.eql(u8, a, "--strict")) {
            args.strict = true;
        } else if (std.mem.eql(u8, a, "--lenient")) {
            args.strict = false;
        } else if (std.mem.eql(u8, a, "-h") or std.mem.eql(u8, a, "--help")) {
            std.debug.print("{s}", .{help_text});
            std.process.exit(0);
        } else {
            std.debug.print("warning: unknown arg {s}\n", .{a});
        }
    }
    return args;
}

fn readFileAll(alloc: std.mem.Allocator, io: std.Io, path: []const u8) ![]u8 {
    const cwd = std.Io.Dir.cwd();
    var f = try cwd.openFile(io, path, .{});
    defer f.close(io);
    var buf: [64 * 1024]u8 = undefined;
    var r = f.reader(io, &buf);
    var out: std.ArrayList(u8) = .empty;
    var chunk: [64 * 1024]u8 = undefined;
    while (true) {
        const n = try r.interface.readSliceShort(&chunk);
        if (n == 0) break;
        try out.appendSlice(alloc, chunk[0..n]);
        if (n < chunk.len) break;
    }
    return try out.toOwnedSlice(alloc);
}

fn readStdinAll(alloc: std.mem.Allocator, io: std.Io, stdin_file: std.Io.File) ![]u8 {
    var buf: [64 * 1024]u8 = undefined;
    var r = stdin_file.readerStreaming(io, &buf);
    var out: std.ArrayList(u8) = .empty;
    var chunk: [64 * 1024]u8 = undefined;
    while (true) {
        const n = try r.interface.readSliceShort(&chunk);
        if (n == 0) break;
        try out.appendSlice(alloc, chunk[0..n]);
        if (n < chunk.len) break;
    }
    return try out.toOwnedSlice(alloc);
}

fn die(w: *std.Io.Writer, msg: []const u8) !void {
    try w.writeAll(msg);
    try w.flush();
    std.process.exit(1);
}

fn writeJson(w: *std.Io.Writer, doc: parser.Document) !void {
    // The Document struct contains pointer-shaped fields that aren't friendly
    // for std.json.Stringify.value's automatic emission, so build the object
    // by hand with the streaming Stringify API.
    var s = std.json.Stringify{ .writer = w, .options = .{} };
    try s.beginObject();
    try s.objectField("title");
    try s.write(doc.title);
    try s.objectField("abstract");
    try s.write(doc.abstract);
    try s.objectField("authors");
    try s.beginArray();
    for (doc.authors) |a| try s.write(a);
    try s.endArray();
    try s.objectField("sections");
    try s.beginArray();
    for (doc.sections) |sec| {
        try s.beginObject();
        try s.objectField("depth");
        try s.write(@as(u32, sec.depth));
        try s.objectField("title");
        try s.write(sec.title);
        try s.objectField("body");
        try s.write(sec.body);
        try s.endObject();
    }
    try s.endArray();
    try s.objectField("figures");
    try s.beginArray();
    for (doc.figures) |fig| {
        try s.beginObject();
        try s.objectField("label");
        try s.write(fig.label);
        try s.objectField("caption");
        try s.write(fig.caption);
        try s.objectField("src_paths");
        try s.beginArray();
        for (fig.src_paths) |p| try s.write(p);
        try s.endArray();
        try s.endObject();
    }
    try s.endArray();
    try s.objectField("math");
    try s.beginArray();
    for (doc.math) |m| {
        try s.beginObject();
        try s.objectField("id");
        try s.write(m.id);
        try s.objectField("env");
        try s.write(m.env);
        try s.objectField("source");
        try s.write(m.source);
        try s.endObject();
    }
    try s.endArray();
    try s.objectField("math_placeholders");
    try s.write(doc.math_placeholders);
    try s.objectField("parser_version");
    try s.write(doc.parser_version);
    try s.objectField("parser_errors");
    try s.beginArray();
    for (doc.parser_errors) |e| try s.write(e);
    try s.endArray();
    try s.objectField("elapsed_ms");
    try s.write(doc.elapsed_ms);
    try s.endObject();
    try w.writeAll("\n");
}

fn writeText(w: *std.Io.Writer, doc: parser.Document) !void {
    if (doc.title.len > 0) try w.print("# {s}\n\n", .{doc.title});
    if (doc.authors.len > 0) {
        try w.writeAll("Authors: ");
        for (doc.authors, 0..) |a, i| {
            if (i > 0) try w.writeAll(", ");
            try w.writeAll(a);
        }
        try w.writeAll("\n\n");
    }
    if (doc.abstract.len > 0) {
        try w.writeAll("Abstract\n\n");
        try w.print("{s}\n\n", .{doc.abstract});
    }
    for (doc.sections) |sec| {
        try w.writeAll("\n");
        var i: u8 = 0;
        while (i < sec.depth) : (i += 1) try w.writeAll("#");
        try w.print(" {s}\n\n{s}\n", .{ sec.title, sec.body });
    }
    if (doc.figures.len > 0) {
        try w.writeAll("\nFigures\n");
        for (doc.figures) |f| try w.print("- [{s}] {s}\n", .{ f.label, f.caption });
    }
}
