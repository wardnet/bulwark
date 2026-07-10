package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadMissingFileReturnsDefault(t *testing.T) {
	got, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, Default()) {
		t.Fatalf("Load with no file = %+v, want Default() = %+v", got, Default())
	}
}

func TestLoadPartialOverrideKeepsOtherDefaults(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go:\n  exclude: [\"legacy\"]\n")

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Default()
	want.Go.Exclude = []string{"legacy"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load = %+v, want %+v", got, want)
	}
	// Rust/TypeScript/Semgrep, untouched by the file, must keep Default()'s values.
	if !got.Rust.Enabled || !got.TypeScript.Enabled || !got.Semgrep.Enabled {
		t.Fatalf("an untouched section lost its default enabled=true: %+v", got)
	}
}

func TestLoadDisableLanguage(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "rust:\n  enabled: false\n")

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Rust.Enabled {
		t.Fatal("rust.enabled: false in the file did not disable Rust")
	}
	if !got.Go.Enabled || !got.TypeScript.Enabled {
		t.Fatalf("disabling rust incorrectly disabled another language: %+v", got)
	}
}

func TestLoadSemgrepConfigOverride(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "semgrep:\n  config: p/security-audit\n")

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Semgrep.Config != "p/security-audit" {
		t.Fatalf("Semgrep.Config = %q, want %q", got.Semgrep.Config, "p/security-audit")
	}
	if !got.Semgrep.Enabled {
		t.Fatal("overriding config incorrectly disabled semgrep")
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "rust: [this is not a mapping\n")

	if _, err := Load(dir); err == nil {
		t.Fatal("expected an error parsing invalid YAML, got nil")
	}
}

func write(t *testing.T, dir, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
