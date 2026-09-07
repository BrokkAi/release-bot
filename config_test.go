package releasebot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfiguration(t *testing.T) {
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{`{"remote":"git@github.com:org/repo.git"}`, true},
		{`{"remote":"https://github.com/org/repo.git"}`, true},
		{`{"remote":"https://example.com/org/repo.git"}`, true},
		{`{"remote":"https://example.com/org/repo.git","verify":["/opt/check"]}`, true},
		{`{"remote":"git@github.com:org/repo.git","pol":"3m"}`, false},
		{`{"remote":"git@github.com:org/repo.git","poll":"0s"}`, false},
		{`{"remote":"git@github.com:org/repo.git","directory":"same","state_directory":"same/state"}`, false},
		{`{"remote":"git@github.com:org/repo.git"}{}`, false},
	} {
		dir := t.TempDir()
		file := filepath.Join(dir, "config.json")
		if err := os.WriteFile(file, []byte(tc.body), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := ReadConfig(file)
		if (err == nil) != tc.valid {
			t.Fatalf("%s: %v", tc.body, err)
		}
		if err == nil && !filepath.IsAbs(cfg.Directory) {
			t.Fatal("relative directory not resolved")
		}
	}
}
func TestSymlinkDirectoryOverlap(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "real"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "config.json")
	if err := os.WriteFile(file, []byte(`{"remote":"git@github.com:o/r.git","directory":"real","state_directory":"alias/state"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(file); err == nil {
		t.Fatal("symlink bypassed directory isolation")
	}
}
