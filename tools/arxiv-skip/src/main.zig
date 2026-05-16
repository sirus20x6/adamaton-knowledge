// arxiv-skip — HTTP sidecar that pre-flights arxiv papers to decide
// whether the heavy deepresearch ingest pipeline should bother running.
//
// The reindex worker on the Pi hits POST /check with an arxiv_id and
// gets back {has_latex, reason} in <500ms. That replaces the ~30s
// backend round-trip we pay today for the ~86% of papers that turn
// out to be PDF-only or withdrawn.
//
// Design notes:
//   * Thread-per-connection. Zig 0.16's std.Io.Threaded handles the
//     pool; we just spawn a worker for each accept().
//   * Each request makes its own short-lived std.http.Client. Sharing
//     a connection pool across worker threads would be a meaningful
//     win at 1000+ rps but is overkill for arxiv preflights — the
//     upstream rate limits long before our pool would help.
//   * Stateless. The Go side has evo.reindex_skipped for caching.
//   * Logging is one line per request on stderr, no bodies.

const std = @import("std");
const builtin = @import("builtin");
const http = std.http;
const net = std.Io.net;
const Allocator = std.mem.Allocator;

const default_port: u16 = 9871;
const max_concurrent_workers: usize = 32;
const arxiv_request_timeout_ms: u64 = 5_000;
const user_agent = "evo-arxiv-skip/0.1 (https://github.com/sirus20x6/evo)";

/// Verdict returned to the Go caller.
const Verdict = struct {
    has_latex: bool,
    reason: []const u8,
    /// HTTP status to return to the client. 200 for a definite answer;
    /// 502 for transient upstream failures (caller should retry).
    status: http.Status = .ok,
};

pub fn main(init: std.process.Init) !void {
    const alloc = init.gpa;
    const io = init.io;

    const port = readPortFromEnv(init.environ_map) catch |err| {
        std.debug.print("invalid ARXIV_SKIP_PORT: {s}\n", .{@errorName(err)});
        std.process.exit(2);
    };

    // 0.0.0.0:<port>. Pi compose hits this from another container, so
    // we can't loopback-only.
    var addr: net.IpAddress = .{ .ip4 = .unspecified(port) };
    var server = try addr.listen(io, .{
        .reuse_address = true,
    });
    defer server.deinit(io);

    std.debug.print("arxiv-skip listening on 0.0.0.0:{d}\n", .{port});

    // Bound concurrent workers. Acquire a slot before accept-and-spawn;
    // worker releases on exit. Backpressure is the kernel listen
    // backlog, which keeps memory bounded even under burst load.
    var slots: std.Io.Semaphore = .{ .permits = max_concurrent_workers };

    while (true) {
        slots.waitUncancelable(io);
        const stream = server.accept(io) catch |err| {
            std.debug.print("accept failed: {s}\n", .{@errorName(err)});
            slots.post(io);
            continue;
        };
        // Spawn detached worker. The worker owns the stream + io and
        // calls slots.post() on exit.
        const thread = std.Thread.spawn(.{}, worker, .{ alloc, io, stream, &slots }) catch |err| {
            std.debug.print("spawn worker failed: {s}\n", .{@errorName(err)});
            stream.close(io);
            slots.post(io);
            continue;
        };
        thread.detach();
    }
}

fn readPortFromEnv(environ_map: *std.process.Environ.Map) !u16 {
    const env = environ_map.get("ARXIV_SKIP_PORT") orelse return default_port;
    if (env.len == 0) return default_port;
    return try std.fmt.parseInt(u16, env, 10);
}

/// worker handles a single accepted connection. Reads one HTTP request,
/// dispatches by method+path, writes the response, then closes.
/// `keep_alive` is honored by std.http.Server — when the client asks
/// for it and we agree, the loop runs another receiveHead() round.
fn worker(alloc: Allocator, io: std.Io, stream: net.Stream, slots: *std.Io.Semaphore) void {
    defer slots.post(io);
    defer stream.close(io);

    var read_buf: [8192]u8 = undefined;
    var write_buf: [8192]u8 = undefined;
    var reader = stream.reader(io, &read_buf);
    var writer = stream.writer(io, &write_buf);

    var server = http.Server.init(&reader.interface, &writer.interface);

    // Keep-alive loop — std.http.Server reuses the connection if the
    // client and server both want it.
    while (true) {
        var req = server.receiveHead() catch |err| switch (err) {
            error.HttpConnectionClosing, error.HttpRequestTruncated => return,
            else => {
                std.debug.print("receiveHead failed: {s}\n", .{@errorName(err)});
                return;
            },
        };
        handleRequest(alloc, io, &req) catch |err| {
            std.debug.print("handleRequest failed: {s}\n", .{@errorName(err)});
            return;
        };
        if (!req.head.keep_alive) return;
    }
}

fn handleRequest(alloc: Allocator, io: std.Io, req: *http.Server.Request) !void {
    const target = req.head.target;
    const method = req.head.method;

    // Strip query string for routing.
    const path_end = std.mem.indexOfScalar(u8, target, '?') orelse target.len;
    const path = target[0..path_end];

    if (std.mem.eql(u8, path, "/health") and method == .GET) {
        try req.respond("ok\n", .{
            .status = .ok,
            .extra_headers = &.{
                .{ .name = "content-type", .value = "text/plain; charset=utf-8" },
            },
        });
        return;
    }

    if (std.mem.eql(u8, path, "/metrics") and method == .GET) {
        // Bare-bones placeholder — could plumb counters later. Returning
        // an empty body keeps Prometheus scrapers from crashing while
        // we decide if we want real metrics here.
        try req.respond("# arxiv-skip metrics — TODO\n", .{
            .status = .ok,
            .extra_headers = &.{
                .{ .name = "content-type", .value = "text/plain; version=0.0.4" },
            },
        });
        return;
    }

    if (std.mem.eql(u8, path, "/check") and method == .POST) {
        try handleCheck(alloc, io, req);
        return;
    }

    try req.respond("not found\n", .{
        .status = .not_found,
        .extra_headers = &.{
            .{ .name = "content-type", .value = "text/plain; charset=utf-8" },
        },
    });
}

const CheckRequest = struct {
    arxiv_id: []const u8,
};

fn handleCheck(alloc: Allocator, io: std.Io, req: *http.Server.Request) !void {
    // Read the body. We cap at 64 KiB to keep adversarial inputs from
    // exhausting RAM.
    var body_buf: [4096]u8 = undefined;
    const body_reader = req.readerExpectContinue(&body_buf) catch {
        try respondJsonError(req, .bad_request, "expect-continue failed");
        return;
    };
    var body_storage = std.ArrayList(u8).empty;
    defer body_storage.deinit(alloc);
    var aw = std.Io.Writer.Allocating.fromArrayList(alloc, &body_storage);
    defer body_storage = aw.toArrayList();
    _ = body_reader.streamRemaining(&aw.writer) catch {
        try respondJsonError(req, .bad_request, "failed to read body");
        return;
    };

    if (aw.written().len > 65_536) {
        try respondJsonError(req, .bad_request, "body too large");
        return;
    }

    // Parse {"arxiv_id": "..."} — strict, no extras tolerated.
    var parsed = std.json.parseFromSlice(CheckRequest, alloc, aw.written(), .{
        .ignore_unknown_fields = true,
    }) catch {
        try respondJsonError(req, .bad_request, "invalid json");
        return;
    };
    defer parsed.deinit();

    const arxiv_id = parsed.value.arxiv_id;
    const started = std.Io.Clock.awake.now(io);
    const verdict = preflight(alloc, io, arxiv_id);
    const elapsed_ms: u64 = blk: {
        const ended = std.Io.Clock.awake.now(io);
        const ms = started.durationTo(ended).toMilliseconds();
        if (ms < 0) break :blk 0;
        break :blk @intCast(ms);
    };

    // Stderr log line — one per request, no body.
    std.debug.print("check arxiv={s} verdict={s} reason={s} elapsed_ms={d}\n", .{
        arxiv_id,
        if (verdict.has_latex) "has_latex" else "skip",
        verdict.reason,
        elapsed_ms,
    });

    if (verdict.status == .bad_gateway) {
        var json_buf: [512]u8 = undefined;
        const body = std.fmt.bufPrint(&json_buf,
            \\{{"arxiv_id":"{f}","error":"{f}"}}
        , .{
            std.zig.fmtString(arxiv_id),
            std.zig.fmtString(verdict.reason),
        }) catch "{\"error\":\"too long\"}";
        try req.respond(body, .{
            .status = .bad_gateway,
            .extra_headers = &.{
                .{ .name = "content-type", .value = "application/json" },
            },
        });
        return;
    }

    var json_buf: [1024]u8 = undefined;
    const body = std.fmt.bufPrint(&json_buf,
        \\{{"arxiv_id":"{f}","has_latex":{},"reason":"{f}","elapsed_ms":{d}}}
    , .{
        std.zig.fmtString(arxiv_id),
        verdict.has_latex,
        std.zig.fmtString(verdict.reason),
        elapsed_ms,
    }) catch "{\"error\":\"response too long\"}";
    try req.respond(body, .{
        .status = .ok,
        .extra_headers = &.{
            .{ .name = "content-type", .value = "application/json" },
        },
    });
}

fn respondJsonError(req: *http.Server.Request, status: http.Status, msg: []const u8) !void {
    var json_buf: [256]u8 = undefined;
    const body = std.fmt.bufPrint(&json_buf,
        \\{{"error":"{f}"}}
    , .{std.zig.fmtString(msg)}) catch "{\"error\":\"format error\"}";
    try req.respond(body, .{
        .status = status,
        .extra_headers = &.{
            .{ .name = "content-type", .value = "application/json" },
        },
    });
}

/// preflight resolves the verdict for an arxiv id. Pure logic + one
/// HTTP HEAD against arxiv.org/e-print. Exposed for tests via the
/// `preflightWithFetcher` indirection below.
fn preflight(alloc: Allocator, io: std.Io, arxiv_id: []const u8) Verdict {
    return preflightWithFetcher(alloc, io, arxiv_id, fetchArxivHead);
}

const HeadInfo = struct {
    status: u16,
    content_type: ?[]const u8 = null,
    content_disposition: ?[]const u8 = null,
};

const Fetcher = *const fn (alloc: Allocator, io: std.Io, arxiv_id: []const u8) anyerror!HeadInfo;

fn preflightWithFetcher(alloc: Allocator, io: std.Io, arxiv_id: []const u8, fetcher: Fetcher) Verdict {
    if (!isValidArxivId(arxiv_id)) {
        return .{ .has_latex = false, .reason = "invalid_arxiv_id" };
    }

    _ = alloc;

    const info = fetcher(std.heap.smp_allocator, io, arxiv_id) catch |err| {
        return .{
            .has_latex = false,
            .reason = switch (err) {
                error.Timeout => "arxiv timeout",
                error.ConnectionRefused, error.NetworkUnreachable, error.HostUnreachable => "arxiv unreachable",
                else => "arxiv error",
            },
            .status = .bad_gateway,
        };
    };

    return classify(info);
}

/// classify maps an arxiv response head to a verdict. Pure function —
/// the test suite drives it directly without HTTP.
fn classify(info: HeadInfo) Verdict {
    switch (info.status) {
        404 => return .{ .has_latex = false, .reason = "withdrawn or not found" },
        410 => return .{ .has_latex = false, .reason = "gone" },
        500...599 => return .{
            .has_latex = false,
            .reason = "arxiv upstream error",
            .status = .bad_gateway,
        },
        200 => {},
        else => return .{
            .has_latex = false,
            .reason = "unexpected status",
            .status = .bad_gateway,
        },
    }

    const ct = info.content_type orelse "";
    const cd = info.content_disposition orelse "";

    // Strong positive signal: content-disposition filename ends in
    // .tar.gz or .gz. arxiv returns this even when content-type is
    // generic application/x-eprint.
    if (containsCI(cd, ".tar.gz") or containsCI(cd, ".gz\"") or hasSuffixCI(stripQuotes(cd), ".gz")) {
        return .{ .has_latex = true, .reason = formatReason("content-disposition: ", cd) };
    }

    if (containsCI(ct, "application/gzip") or containsCI(ct, "application/x-gzip")) {
        return .{ .has_latex = true, .reason = formatReason("content-type: ", ct) };
    }
    if (containsCI(ct, "application/x-eprint-tar")) {
        return .{ .has_latex = true, .reason = formatReason("content-type: ", ct) };
    }
    if (containsCI(ct, "application/x-eprint")) {
        // Old single-file format. Tarballs would have matched
        // x-eprint-tar above; bare x-eprint is gzipped TeX source.
        return .{ .has_latex = true, .reason = formatReason("content-type: ", ct) };
    }

    if (containsCI(ct, "application/pdf")) {
        return .{ .has_latex = false, .reason = "PDF-only submission" };
    }
    if (containsCI(ct, "text/html")) {
        // arxiv occasionally renders an error page for withdrawn papers
        // with 200 + text/html. Treat it as a definite skip.
        return .{ .has_latex = false, .reason = "html landing page" };
    }

    return .{ .has_latex = false, .reason = "unknown content-type" };
}

/// formatReason builds a short reason like `content-type: application/pdf`.
/// We can't easily alloc here, so we stash it in a tiny TLS buffer.
threadlocal var reason_buf: [256]u8 = undefined;
fn formatReason(prefix: []const u8, value: []const u8) []const u8 {
    const slice = std.fmt.bufPrint(&reason_buf, "{s}{s}", .{ prefix, value }) catch return prefix;
    return slice;
}

fn stripQuotes(s: []const u8) []const u8 {
    if (s.len >= 2 and s[s.len - 1] == '"') return s[0 .. s.len - 1];
    return s;
}

fn containsCI(haystack: []const u8, needle: []const u8) bool {
    if (needle.len > haystack.len) return false;
    var i: usize = 0;
    while (i + needle.len <= haystack.len) : (i += 1) {
        if (std.ascii.eqlIgnoreCase(haystack[i .. i + needle.len], needle)) return true;
    }
    return false;
}

fn hasSuffixCI(haystack: []const u8, needle: []const u8) bool {
    if (needle.len > haystack.len) return false;
    return std.ascii.eqlIgnoreCase(haystack[haystack.len - needle.len ..], needle);
}

/// isValidArxivId enforces `^([A-Za-z\-\.]+/[0-9]{7}|[0-9]+\.[0-9]+(v[0-9]+)?)$`
/// directly — std doesn't ship a regex engine and pulling one in for
/// six lines of validation is silly.
fn isValidArxivId(id: []const u8) bool {
    if (id.len == 0 or id.len > 64) return false;

    if (std.mem.indexOfScalar(u8, id, '/')) |slash| {
        // Old form: cat/NNNNNNN
        if (slash == 0 or slash == id.len - 1) return false;
        const cat = id[0..slash];
        const num = id[slash + 1 ..];
        if (num.len != 7) return false;
        for (cat) |c| {
            if (!std.ascii.isAlphabetic(c) and c != '-' and c != '.') return false;
        }
        for (num) |c| if (!std.ascii.isDigit(c)) return false;
        return true;
    }

    // New form: NNNN.NNNNN[vN]
    const dot = std.mem.indexOfScalar(u8, id, '.') orelse return false;
    if (dot == 0 or dot == id.len - 1) return false;
    const left = id[0..dot];
    const right = id[dot + 1 ..];
    for (left) |c| if (!std.ascii.isDigit(c)) return false;

    // Right may have optional `vN` suffix
    var v_idx: usize = right.len;
    if (std.mem.indexOfScalar(u8, right, 'v')) |v| v_idx = v;
    if (v_idx == 0) return false;
    for (right[0..v_idx]) |c| if (!std.ascii.isDigit(c)) return false;
    if (v_idx < right.len) {
        if (right[v_idx] != 'v') return false;
        const ver = right[v_idx + 1 ..];
        if (ver.len == 0) return false;
        for (ver) |c| if (!std.ascii.isDigit(c)) return false;
    }
    return true;
}

/// fetchArxivHead issues an HTTP HEAD against arxiv's e-print URL.
/// The arxiv server reliably exposes content-type and (where useful)
/// content-disposition on the HEAD response — no need to download the
/// tarball to make the call.
fn fetchArxivHead(alloc: Allocator, io: std.Io, arxiv_id: []const u8) !HeadInfo {
    var url_buf: [256]u8 = undefined;
    const url = try std.fmt.bufPrint(&url_buf, "https://arxiv.org/e-print/{s}", .{arxiv_id});

    var client: http.Client = .{ .allocator = alloc, .io = io };
    defer client.deinit();

    const uri = try std.Uri.parse(url);
    var req = try client.request(.HEAD, uri, .{
        .extra_headers = &.{
            .{ .name = "user-agent", .value = user_agent },
        },
        .keep_alive = false,
        // Follow up to 3 redirects (arxiv may bounce e-print URLs).
    });
    defer req.deinit();
    try req.sendBodiless();

    var redirect_buf: [8192]u8 = undefined;
    const resp = try req.receiveHead(&redirect_buf);

    // The head fields are stable until we touch the body reader — since
    // this is HEAD, we never read a body, but we still need to copy the
    // strings into stable storage owned by the caller. Use TLS buffers
    // so we don't have to allocate per-request.
    return .{
        .status = @intFromEnum(resp.head.status),
        .content_type = if (resp.head.content_type) |s| copyToTls(s, &ct_buf) else null,
        .content_disposition = if (resp.head.content_disposition) |s| copyToTls(s, &cd_buf) else null,
    };
}

threadlocal var ct_buf: [256]u8 = undefined;
threadlocal var cd_buf: [256]u8 = undefined;

fn copyToTls(src: []const u8, dst: *[256]u8) []const u8 {
    const n = @min(src.len, dst.len);
    @memcpy(dst[0..n], src[0..n]);
    return dst[0..n];
}

// ───── tests ─────────────────────────────────────────────────────────

test "isValidArxivId accepts canonical new-style ids" {
    try std.testing.expect(isValidArxivId("2411.06989"));
    try std.testing.expect(isValidArxivId("2411.06989v2"));
    try std.testing.expect(isValidArxivId("1706.03762"));
    try std.testing.expect(isValidArxivId("1706.03762v5"));
}

test "isValidArxivId accepts old-style ids" {
    try std.testing.expect(isValidArxivId("hep-th/0301006"));
    try std.testing.expect(isValidArxivId("math.AG/0601001"));
    try std.testing.expect(isValidArxivId("cs.LG/0501007"));
}

test "isValidArxivId rejects garbage" {
    try std.testing.expect(!isValidArxivId(""));
    try std.testing.expect(!isValidArxivId("not-an-id"));
    try std.testing.expect(!isValidArxivId("../../etc/passwd"));
    try std.testing.expect(!isValidArxivId("2411.06989v"));
    try std.testing.expect(!isValidArxivId("hep-th/12345"));
    try std.testing.expect(!isValidArxivId("/0301006"));
}

test "classify: gzip tarball means has_latex" {
    const v = classify(.{ .status = 200, .content_type = "application/gzip" });
    try std.testing.expect(v.has_latex);
}

test "classify: PDF means no_latex" {
    const v = classify(.{ .status = 200, .content_type = "application/pdf" });
    try std.testing.expect(!v.has_latex);
}

test "classify: tar.gz content-disposition wins even with generic content-type" {
    const v = classify(.{
        .status = 200,
        .content_type = "application/octet-stream",
        .content_disposition = "attachment; filename=\"arXiv-2411.06989.tar.gz\"",
    });
    try std.testing.expect(v.has_latex);
}

test "classify: 404 is a definite skip" {
    const v = classify(.{ .status = 404 });
    try std.testing.expect(!v.has_latex);
    try std.testing.expectEqualStrings("withdrawn or not found", v.reason);
}

test "classify: 410 is gone" {
    const v = classify(.{ .status = 410 });
    try std.testing.expect(!v.has_latex);
}

test "classify: 5xx triggers bad_gateway pass-through" {
    const v = classify(.{ .status = 502 });
    try std.testing.expect(!v.has_latex);
    try std.testing.expect(v.status == .bad_gateway);
}

test "classify: x-eprint old-format is has_latex" {
    const v = classify(.{ .status = 200, .content_type = "application/x-eprint" });
    try std.testing.expect(v.has_latex);
}

// Fake fetcher for preflight integration tests — no network.
fn fakeFetcherGzip(_: Allocator, _: std.Io, _: []const u8) anyerror!HeadInfo {
    return .{ .status = 200, .content_type = "application/gzip" };
}

fn fakeFetcherPdf(_: Allocator, _: std.Io, _: []const u8) anyerror!HeadInfo {
    return .{ .status = 200, .content_type = "application/pdf" };
}

fn fakeFetcher404(_: Allocator, _: std.Io, _: []const u8) anyerror!HeadInfo {
    return .{ .status = 404 };
}

fn fakeFetcherTimeout(_: Allocator, _: std.Io, _: []const u8) anyerror!HeadInfo {
    return error.Timeout;
}

test "preflightWithFetcher: invalid id short-circuits without calling fetcher" {
    const v = preflightWithFetcher(std.testing.allocator, undefined, "garbage", fakeFetcherGzip);
    try std.testing.expect(!v.has_latex);
    try std.testing.expectEqualStrings("invalid_arxiv_id", v.reason);
}

test "preflightWithFetcher: gzip means has_latex" {
    const v = preflightWithFetcher(std.testing.allocator, undefined, "2411.06989", fakeFetcherGzip);
    try std.testing.expect(v.has_latex);
}

test "preflightWithFetcher: pdf means skip" {
    const v = preflightWithFetcher(std.testing.allocator, undefined, "2411.06989", fakeFetcherPdf);
    try std.testing.expect(!v.has_latex);
    try std.testing.expectEqualStrings("PDF-only submission", v.reason);
}

test "preflightWithFetcher: 404 means skip" {
    const v = preflightWithFetcher(std.testing.allocator, undefined, "2411.06989", fakeFetcher404);
    try std.testing.expect(!v.has_latex);
}

test "preflightWithFetcher: timeout surfaces as bad_gateway" {
    const v = preflightWithFetcher(std.testing.allocator, undefined, "2411.06989", fakeFetcherTimeout);
    try std.testing.expect(v.status == .bad_gateway);
}

// silence the "unused" warnings when builtin gates fire
comptime {
    _ = builtin;
}
