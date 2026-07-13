fn main() {
    for name in [
        "MEMINI_BUILD_VERSION",
        "MEMINI_BUILD_REVISION",
        "MEMINI_BUILD_DATE",
    ] {
        println!("cargo:rerun-if-env-changed={name}");
    }
}
