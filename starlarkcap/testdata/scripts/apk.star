# apk.star defines the apk.* capabilities (prefix = contribution id "apk").
load("//lib/names.star", "java_sources")

def _decompile(ctx, input):
    ctx.progress(stage = "decompile", done = 1, total = 1, unit = "files")
    art = ctx.call("storage.artifact.get", id = input["artifact"])
    return {"artifact": art["got"], "sources": java_sources(art["got"])}

capability(
    name = "apk.decompile",
    title = "Decompile an APK (test fixture)",
    input = {"artifact": schema.string(required = True, description = "artifact id")},
    effects = ["read_only"],
    run = _decompile,
)
