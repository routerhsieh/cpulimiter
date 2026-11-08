package main

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveReportPreservesCompleteTree(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "reports with spaces")
	if err := os.MkdirAll(filepath.Join(dir, "attempt", "empty"), 0755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"summary.json": "{\"status\":\"FAIL\"}", "run.log": "failure details\n", "attempt/child.log": "child output\n", "attempt/測試 資料.json": "{\"cpu\":2.5}"}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{".runner", ".runner.exe"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("do not package executable"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	archive, err := archiveReport(dir)
	if err != nil {
		t.Fatal(err)
	}
	if archive != dir+".zip" {
		t.Fatalf("unexpected output: %s", archive)
	}
	r, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	seen := map[string]bool{}
	for _, entry := range r.File {
		if strings.Contains(entry.Name, "\\") || strings.HasPrefix(entry.Name, "/") {
			t.Fatalf("nonportable ZIP path: %s", entry.Name)
		}
		name := strings.TrimPrefix(entry.Name, filepath.Base(dir)+"/")
		seen[name] = true
		if entry.FileInfo().IsDir() {
			continue
		}
		want, ok := files[name]
		if !ok {
			t.Fatalf("unexpected ZIP file: %s", name)
		}
		f, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != want {
			t.Fatalf("changed content: %s", name)
		}
		if entry.Method != zip.Deflate {
			t.Errorf("file not compressed: %s", name)
		}
	}
	for name := range files {
		if !seen[name] {
			t.Errorf("missing ZIP entry: %s", name)
		}
	}
	if !seen["attempt/empty/"] {
		t.Error("empty directory omitted")
	}
	if data, err := os.ReadFile(filepath.Join(dir, "run.log")); err != nil || string(data) != files["run.log"] {
		t.Fatal("original reports were modified")
	}
}

func TestArchiveReportFailureLeavesNoPartialZIP(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "reports")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.log"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	// A directory at the destination forces finalization to fail on all OSes.
	if err := os.Mkdir(dir+".zip", 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := archiveReport(dir); err == nil {
		t.Fatal("expected ZIP finalization failure")
	}
	partials, err := filepath.Glob(filepath.Join(filepath.Dir(dir), "."+filepath.Base(dir)+"-*.zip.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(partials) != 0 {
		t.Fatalf("partial ZIP left behind: %v", partials)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "run.log")); err != nil || string(data) != "keep" {
		t.Fatal("original reports lost")
	}
}

func TestArchiveReportRejectsMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	if _, err := archiveReport(dir); err == nil {
		t.Fatal("missing report directory accepted")
	}
	if _, err := os.Stat(dir + ".zip"); !os.IsNotExist(err) {
		t.Fatal("unexpected ZIP for missing reports")
	}
}
