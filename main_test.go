package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSourcesJSON(t *testing.T) {
	data, err := os.ReadFile("sources.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseSources(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 2 {
		t.Fatalf("source catalog is unexpectedly small: %d", len(got))
	}
	for _, src := range got {
		if (src.URL == "") == (src.LocalPath == "") {
			t.Fatalf("source %q must have exactly one location", src.ID)
		}
	}
}

func TestParseSourcesRejectsInvalidCatalog(t *testing.T) {
	for _, data := range []string{
		`{`,
		`[]`,
		`[{"id":"x","name":"x","url":"https://example.invalid","size":1,"sha256":"00","extraction":["raw"]}]`,
		`[{"id":"x","name":"x","local_path":"x","extraction":["unknown"]}]`,
	} {
		if _, err := parseSources([]byte(data)); err == nil {
			t.Fatalf("invalid catalog was accepted: %s", data)
		}
	}
}

func TestReadFileProgressLimit(t *testing.T) {
	path := t.TempDir() + "/large.bin"
	if err := os.WriteFile(path, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readFileProgress(path, "test", 4); err == nil {
		t.Fatal("oversized local file was accepted")
	}
}

func TestRankRemoteSources(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/slow" {
			time.Sleep(80 * time.Millisecond)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})}

	list := []source{
		{ID: "slow", Name: "slow", URL: "https://example.invalid/slow"},
		{ID: "fast", Name: "fast", URL: "https://example.invalid/fast"},
		{ID: "custom", Name: "custom", URL: "https://example.invalid/slow", priority: true},
		{ID: "local", Name: "local", LocalPath: "ignored"},
	}
	got := rankRemoteSources(client, list)
	if len(got) != 3 || got[0].ID != "custom" || got[1].ID != "fast" || got[2].ID != "slow" {
		t.Fatalf("unexpected probe order: %#v", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestVerifyDownload(t *testing.T) {
	data := []byte("source package")
	h := sha256.Sum256(data)
	src := source{Size: int64(len(data)), SHA256: hex.EncodeToString(h[:])}
	if err := verifyDownload(src, data); err != nil {
		t.Fatal(err)
	}
	if err := verifyDownload(src, append(data, 0)); err == nil {
		t.Fatal("size mismatch was accepted")
	}
}

func TestInnoFixture(t *testing.T) {
	path := os.Getenv("K223FETCH_INNO_FIXTURE")
	if path == "" {
		t.Skip("set K223FETCH_INNO_FIXTURE to test a real Inno installer")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := findInno(b, &extractBudget{bytes: maxExtracted, entries: maxEntries, innoStreams: maxInnoStreams})
	if !valid(got) {
		t.Fatal("known firmware not found in Inno fixture")
	}
}

func TestRARFixture(t *testing.T) {
	path := os.Getenv("K223FETCH_RAR_FIXTURE")
	if path == "" {
		t.Skip("set K223FETCH_RAR_FIXTURE to test a real RAR package")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := extract(b, path, 0)
	if err != nil || !valid(got) {
		t.Fatalf("known firmware not found in RAR fixture: %v", err)
	}
}

func TestPlannedPackageFixture(t *testing.T) {
	path := os.Getenv("K223FETCH_PACKAGE_FIXTURE")
	planText := os.Getenv("K223FETCH_EXTRACTION_PLAN")
	if path == "" || planText == "" {
		t.Skip("set K223FETCH_PACKAGE_FIXTURE and K223FETCH_EXTRACTION_PLAN to test a real source plan")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	budget := &extractBudget{bytes: maxExtracted, entries: maxEntries, innoStreams: maxInnoStreams}
	got, err := extractPlanned(b, path, 0, strings.Split(planText, ","), budget)
	if err != nil || !valid(got) {
		t.Fatalf("planned extraction failed: %v", err)
	}
}

func knownFirmware(t *testing.T) []byte {
	t.Helper()
	path := os.Getenv("K223FETCH_FIRMWARE_FIXTURE")
	if path == "" {
		t.Skip("set K223FETCH_FIRMWARE_FIXTURE to test extraction with real firmware")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !valid(b) {
		t.Fatal("repository fixture has unexpected hash")
	}
	return b
}

func TestRawAndZIPExtraction(t *testing.T) {
	fw := knownFirmware(t)
	raw := append(bytes.Repeat([]byte{0x55}, 321), fw...)
	got, err := extract(raw, "raw", 0)
	if err != nil || !bytes.Equal(got, fw) {
		t.Fatalf("raw extraction: %v", err)
	}
	budget := &extractBudget{bytes: maxExtracted, entries: maxEntries, innoStreams: maxInnoStreams}
	got, err = extractPlanned(raw, "updater.exe", 0, []string{"pe", "raw"}, budget)
	if err != nil || !bytes.Equal(got, fw) {
		t.Fatalf("planned PE/raw extraction: %v", err)
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("payload.dmg")
	_, _ = w.Write(raw)
	_ = zw.Close()
	got, err = extract(buf.Bytes(), "package.zip", 0)
	if err != nil || !bytes.Equal(got, fw) {
		t.Fatalf("ZIP extraction: %v", err)
	}
	budget = &extractBudget{bytes: maxExtracted, entries: maxEntries, innoStreams: maxInnoStreams}
	got, err = extractPlanned(buf.Bytes(), "package.zip", 0, []string{"zip", "dmg", "raw"}, budget)
	if err != nil || !bytes.Equal(got, fw) {
		t.Fatalf("planned ZIP/DMG/raw extraction: %v", err)
	}
}
