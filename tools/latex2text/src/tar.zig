//! Minimal arxiv-tarball loader.
//!
//! Takes a gzipped tar byte slice, decompresses it, extracts every entry
//! into memory, and returns a map of relative path -> file bytes.
//!
//! We do not stream — arxiv tarballs are tiny (a few MB max) and keeping
//! them in memory makes the \input resolver trivial.

const std = @import("std");
const tar = std.tar;
const flate = std.compress.flate;
const Io = std.Io;
const Allocator = std.mem.Allocator;

pub const Entry = struct {
    name: []const u8,
    body: []const u8,
};

pub const Archive = struct {
    entries: []Entry,

    pub fn findByBasename(self: Archive, basename_no_ext: []const u8) ?Entry {
        // Try exact match with .tex, then bare match, then partial.
        for (self.entries) |e| {
            const base = stem(e.name);
            if (std.mem.eql(u8, base, basename_no_ext)) return e;
        }
        for (self.entries) |e| {
            const base = stem(e.name);
            if (std.mem.endsWith(u8, e.name, basename_no_ext)) return e;
            if (std.mem.endsWith(u8, base, basename_no_ext)) return e;
        }
        return null;
    }

    /// Find by full path within the archive. Used when the caller
    /// already has a relative path with directory components (e.g.
    /// `Sections/0_abstract.tex` from a `\input{Sections/0_abstract}`
    /// after the resolver appends `.tex`). Falls back to a
    /// trailing-suffix match so `./Sections/0_abstract.tex` and bare
    /// `Sections/0_abstract.tex` both find a stored
    /// `./Sections/0_abstract.tex` entry.
    pub fn findByPath(self: Archive, path: []const u8) ?Entry {
        for (self.entries) |e| {
            if (std.mem.eql(u8, e.name, path)) return e;
        }
        for (self.entries) |e| {
            if (std.mem.endsWith(u8, e.name, path)) return e;
        }
        return null;
    }

    pub fn findMainTex(self: Archive) ?Entry {
        // Logic from spec:
        // 1. only one .tex → that's main
        // 2. main.tex exists → main
        // 3. paper.tex exists → main
        // 4. otherwise: file containing \documentclass; if multiple, largest
        var tex_count: usize = 0;
        var only: ?Entry = null;
        for (self.entries) |e| {
            if (std.mem.endsWith(u8, e.name, ".tex")) {
                tex_count += 1;
                only = e;
            }
        }
        if (tex_count == 0) return null;
        if (tex_count == 1) return only;
        for (self.entries) |e| {
            if (basename(e.name).len > 0 and std.mem.eql(u8, basename(e.name), "main.tex")) return e;
        }
        for (self.entries) |e| {
            if (basename(e.name).len > 0 and std.mem.eql(u8, basename(e.name), "paper.tex")) return e;
        }
        var best: ?Entry = null;
        for (self.entries) |e| {
            if (!std.mem.endsWith(u8, e.name, ".tex")) continue;
            if (std.mem.indexOf(u8, e.body, "\\documentclass") == null) continue;
            if (best == null or e.body.len > best.?.body.len) best = e;
        }
        return best;
    }
};

fn basename(p: []const u8) []const u8 {
    if (std.mem.lastIndexOfScalar(u8, p, '/')) |i| return p[i + 1 ..];
    return p;
}

fn stem(p: []const u8) []const u8 {
    const b = basename(p);
    if (std.mem.lastIndexOfScalar(u8, b, '.')) |i| return b[0..i];
    return b;
}

/// Detect gzip magic bytes 1f 8b.
pub fn looksLikeGzip(data: []const u8) bool {
    return data.len >= 2 and data[0] == 0x1f and data[1] == 0x8b;
}

/// Detect bare tar (ustar magic at byte 257).
pub fn looksLikeTar(data: []const u8) bool {
    return data.len >= 265 and std.mem.startsWith(u8, data[257..], "ustar");
}

/// Decompress (if needed) and parse a tarball, returning all file entries.
/// All slices in the result are owned by `arena`.
pub fn loadTarball(arena: Allocator, raw: []const u8) !Archive {
    var decompressed_buf: std.ArrayList(u8) = .empty;
    const input_bytes: []const u8 = if (looksLikeGzip(raw)) blk: {
        var fixed: Io.Reader = Io.Reader.fixed(raw);
        const win_buf = try arena.alloc(u8, flate.max_window_len);
        var dec: flate.Decompress = .init(&fixed, .gzip, win_buf);
        // Stream into an ArrayList via a writer-shaped helper.
        try drainReader(&dec.reader, arena, &decompressed_buf);
        break :blk decompressed_buf.items;
    } else raw;

    var entries: std.ArrayList(Entry) = .empty;

    var tar_reader: Io.Reader = Io.Reader.fixed(input_bytes);
    const name_buf = try arena.alloc(u8, std.fs.max_path_bytes);
    const link_buf = try arena.alloc(u8, std.fs.max_path_bytes);
    var it = tar.Iterator.init(&tar_reader, .{
        .file_name_buffer = name_buf,
        .link_name_buffer = link_buf,
    });

    while (try it.next()) |entry| {
        switch (entry.kind) {
            .file => {
                if (entry.size == 0) continue;
                const body = try arena.alloc(u8, @intCast(entry.size));
                try tar_reader.readSliceAll(body);
                it.unread_file_bytes = 0;
                const name_copy = try arena.dupe(u8, entry.name);
                try entries.append(arena, .{ .name = name_copy, .body = body });
            },
            else => {},
        }
    }

    return .{ .entries = try entries.toOwnedSlice(arena) };
}

fn drainReader(r: *Io.Reader, arena: Allocator, out: *std.ArrayList(u8)) !void {
    var tmp: [16 * 1024]u8 = undefined;
    while (true) {
        const n = try r.readSliceShort(&tmp);
        if (n == 0) break;
        try out.appendSlice(arena, tmp[0..n]);
        if (n < tmp.len) break;
    }
}
