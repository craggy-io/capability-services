# Tests for apk.star, run by starlarkcap.Test against a registry that has
# loaded it: capabilities are called exactly as scripts call them.
load("//lib/names.star", "java_sources")

def test_decompile_through_registry():
    out = apk.decompile(artifact = "app.apk")
    assert.eq(out["artifact"], "app.apk")
    assert.eq(out["sources"], java_sources("app.apk"))
    assert.eq(out["sources"][0], "App/Main.java")

def test_call_and_jobs():
    assert.eq(call("apk.decompile", artifact = "x.apk")["sources"][0], "X/Main.java")
    job = jobs.start("apk.decompile", artifact = "y.apk")
    assert.eq(job.wait(timeout_seconds = 5)["status"]["state"], "succeeded")

def test_input_is_validated():
    assert.fails(lambda: apk.decompile(artifakt = "x"), "invalid")
