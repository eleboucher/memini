use std::process::Command;

#[test]
fn version_is_a_one_shot_command() {
    let output = Command::new(env!("CARGO_BIN_EXE_memini"))
        .arg("version")
        .output()
        .unwrap();
    assert!(output.status.success());
    let stdout = String::from_utf8(output.stdout).unwrap();
    assert!(stdout.contains(env!("CARGO_PKG_VERSION")));
    assert!(stdout.contains('('));
    assert!(!stdout.contains("listening"));
}

#[test]
fn export_is_a_one_shot_command() {
    let dir = tempfile::tempdir().unwrap();
    let output = Command::new(env!("CARGO_BIN_EXE_memini"))
        .args(["export", "--namespace", "empty"])
        .env("MEMINI_SQLITE_PATH", dir.path().join("db"))
        .env("MEMINI_EMBED_MODEL", "test-model")
        .env_remove("MEMINI_NAMESPACE")
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let value: serde_json::Value = serde_json::from_slice(&output.stdout).unwrap();
    assert_eq!(value, serde_json::json!({"memories":[]}));
}
