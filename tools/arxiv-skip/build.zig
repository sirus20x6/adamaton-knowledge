// build.zig for the arxiv-skip sidecar.
//
// Two top-level steps:
//   zig build           — produce zig-out/bin/arxiv-skip for the host
//   zig build test      — run unit tests in src/main.zig
//
// Cross-compile for the Pi via the Makefile's pi-arxiv-skip target:
//   zig build -Dtarget=aarch64-linux-musl -Doptimize=ReleaseSmall
//
// Static musl is on purpose: the Dockerfile uses alpine and the binary
// is dropped in as-is — no glibc/musl version-skew worries on the Pi.

const std = @import("std");

pub fn build(b: *std.Build) void {
    const target = b.standardTargetOptions(.{});
    const optimize = b.standardOptimizeOption(.{});

    const exe = b.addExecutable(.{
        .name = "arxiv-skip",
        .root_module = b.createModule(.{
            .root_source_file = b.path("src/main.zig"),
            .target = target,
            .optimize = optimize,
        }),
    });
    b.installArtifact(exe);

    const run_step = b.step("run", "Run arxiv-skip locally");
    const run_cmd = b.addRunArtifact(exe);
    run_cmd.step.dependOn(b.getInstallStep());
    if (b.args) |args| run_cmd.addArgs(args);
    run_step.dependOn(&run_cmd.step);

    const tests = b.addTest(.{
        .root_module = exe.root_module,
    });
    const run_tests = b.addRunArtifact(tests);
    const test_step = b.step("test", "Run arxiv-skip unit tests");
    test_step.dependOn(&run_tests.step);
}
