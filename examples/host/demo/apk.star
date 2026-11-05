# The demo capability file, loaded at startup as the prefix apk (its id
# in bundle.yaml). It supersedes the Go-registered legacy jadx capability
# for every projection, with no extra wiring.
def _decompile(ctx, input):
    ctx.progress(stage = "pull-image", done = 1, total = 1, unit = "steps")
    art = ctx.call("storage.artifact.get", id = input["artifact"])
    ctx.progress(stage = "decompile", done = 50, total = 100, unit = "files", data = {"tool": "apk.star"})
    ctx.progress(stage = "decompile", done = 100, total = 100, unit = "files")
    ctx.progress(stage = "publish-output")
    ctx.progress(stage = "finalise")
    return {"artifact": art["name"], "sources": ["Main.java", "Util.java"], "via": "apk.star"}

capability(
    name = "apk.decompile",
    title = "Decompile an APK (script)",
    description = "Decompile an APK to sources and resources in an isolated container",
    supersedes = ["jadx.decompile"],
    long = True,
    effects = ["open_world"],
    tags = ["agent", "analysis"],
    input = {"artifact": schema.string(required = True, description = "artifact id")},
    run = _decompile,
)
