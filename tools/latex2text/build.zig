const std = @import("std");

pub fn build(b: *std.Build) void {
    const target = b.standardTargetOptions(.{});
    const optimize = b.standardOptimizeOption(.{});

    // Reusable parser module so tests can pull it in cleanly.
    const parser_mod = b.createModule(.{
        .root_source_file = b.path("src/parser.zig"),
        .target = target,
        .optimize = optimize,
    });

    const exe_mod = b.createModule(.{
        .root_source_file = b.path("src/main.zig"),
        .target = target,
        .optimize = optimize,
    });
    exe_mod.addImport("parser", parser_mod);

    const exe = b.addExecutable(.{
        .name = "latex2text",
        .root_module = exe_mod,
    });
    b.installArtifact(exe);

    const run_cmd = b.addRunArtifact(exe);
    run_cmd.step.dependOn(b.getInstallStep());
    if (b.args) |args| run_cmd.addArgs(args);
    const run_step = b.step("run", "Run latex2text");
    run_step.dependOn(&run_cmd.step);

    // ---- Tests ----------------------------------------------------------
    const test_mod = b.createModule(.{
        .root_source_file = b.path("src/test_main.zig"),
        .target = target,
        .optimize = optimize,
    });
    test_mod.addImport("parser", parser_mod);

    const unit_tests = b.addTest(.{ .root_module = test_mod });
    const run_unit_tests = b.addRunArtifact(unit_tests);
    // Integration tests need access to fixtures and the cached arxiv
    // tarball under tests/. Run from the build root so the hard-coded
    // relative paths resolve.
    run_unit_tests.setCwd(b.path("."));
    const test_step = b.step("test", "Run unit tests");
    test_step.dependOn(&run_unit_tests.step);

    // Also run parser.zig's inline tests.
    const parser_tests = b.addTest(.{ .root_module = parser_mod });
    const run_parser_tests = b.addRunArtifact(parser_tests);
    test_step.dependOn(&run_parser_tests.step);
}
