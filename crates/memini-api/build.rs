use std::{env, fs, path::PathBuf};

fn main() {
    println!("cargo:rerun-if-changed=../../ui/dist/index.html");
    println!("cargo:rerun-if-changed=../../ui/dist/assets/index.js");
    println!("cargo:rerun-if-changed=../../ui/dist/assets/index.css");

    let output = PathBuf::from(env::var_os("OUT_DIR").expect("OUT_DIR")).join("ui");
    fs::create_dir_all(&output).expect("create UI output directory");
    let source = PathBuf::from("../../ui/dist");
    copy_or_fallback(
        source.join("index.html"),
        output.join("index.html"),
        b"<!doctype html><html><head></head><body><div id=\"app\"></div><script type=\"module\" src=\"/assets/index.js\"></script></body></html>",
    );
    copy_or_fallback(
        source.join("assets/index.js"),
        output.join("index.js"),
        b"console.warn('memini admin UI was not built; run `mise run ui` before packaging');",
    );
    copy_or_fallback(
        source.join("assets/index.css"),
        output.join("index.css"),
        b"",
    );
}

fn copy_or_fallback(source: PathBuf, destination: PathBuf, fallback: &[u8]) {
    if source.is_file() {
        fs::copy(source, destination).expect("copy built UI asset");
    } else {
        fs::write(destination, fallback).expect("write fallback UI asset");
    }
}
