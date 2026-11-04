# Shared helpers: loaded by apk.star and by its tests.

def java_sources(name):
    base = name.split(".")[0].capitalize()
    return [base + "/Main.java", base + "/Util.java"]
