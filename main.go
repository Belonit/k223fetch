package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nwaples/rardecode/v2"
	"github.com/ulikunitz/xz/lzma"
)

const (
	firmwareName   = "K223RGB_V10003.bin"
	firmwareSize   = 64 * 1024
	firmwareHash   = "c4f88f331420b678c2826e05e18be34b1848659bbd2c1891d804a990c8e86d5e"
	metadata       = "M252,01,KB,FL,K223RGB,V1.00.03"
	maxDownload    = 100 << 20
	maxInnoOutput  = 256 << 20
	maxExtracted   = 512 << 20
	maxEntries     = 4096
	maxInnoStreams = 16
)

type source struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Size       int64    `json:"size"`
	SHA256     string   `json:"sha256"`
	Extraction []string `json:"extraction"`
	URL        string   `json:"url"`
	LocalPath  string   `json:"local_path"`
	priority   bool
}

func loadSourcesNextToExecutable() ([]source, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate executable: %w", err)
	}
	path := filepath.Join(filepath.Dir(executable), "sources.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read source catalog %s: %w", path, err)
	}
	result, err := parseSources(data)
	if err != nil {
		return nil, fmt.Errorf("read source catalog %s: %w", path, err)
	}
	return result, nil
}

func parseSources(data []byte) ([]source, error) {
	var result []source
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if len(result) == 0 {
		return nil, errors.New("catalog contains no sources")
	}
	ids := make(map[string]bool, len(result))
	for i, src := range result {
		if src.ID == "" || src.Name == "" || ids[src.ID] || len(src.Extraction) == 0 ||
			(src.URL == "") == (src.LocalPath == "") {
			return nil, fmt.Errorf("invalid source %d", i+1)
		}
		for _, step := range src.Extraction {
			switch step {
			case "raw", "intel-hex", "zip", "rar", "inno-lzma1", "pe", "dmg":
			case "auto":
				if len(src.Extraction) != 1 {
					return nil, fmt.Errorf("auto must be the only extraction step for source %d", i+1)
				}
			default:
				return nil, fmt.Errorf("invalid extraction step %q for source %d", step, i+1)
			}
		}
		if src.URL != "" && (src.Size <= 0 || src.Size > maxDownload || len(src.SHA256) != sha256.Size*2) {
			return nil, fmt.Errorf("invalid remote source %d", i+1)
		}
		if src.LocalPath != "" && src.Size > maxDownload {
			return nil, fmt.Errorf("invalid local source %d: size exceeds %d bytes", i+1, maxDownload)
		}
		if src.SHA256 != "" {
			if _, err := hex.DecodeString(src.SHA256); err != nil {
				return nil, fmt.Errorf("invalid SHA-256 for source %d", i+1)
			}
		}
		ids[src.ID] = true
	}
	return result, nil
}

func main() {
	flag.Usage = usage
	out := flag.String("output", firmwareName, "output firmware path")
	url := flag.String("url", "", "try this URL before the built-in sources")
	timeout := flag.Duration("timeout", 2*time.Minute, "timeout per download")
	flag.Parse()

	if flag.NArg() > 0 && flag.Arg(0) == "extract" {
		if flag.NArg() != 2 {
			fatal(errors.New("usage: k223fetch [flags] extract PACKAGE"))
		}
		data, err := readFileProgress(flag.Arg(1), "Read package", maxDownload)
		if err != nil {
			fatal(err)
		}
		fw, err := extractProgress(data, filepath.Base(flag.Arg(1)), nil)
		if err != nil {
			fatal(err)
		}
		fatal(writeFirmware(*out, fw))
		return
	}

	sources, err := loadSourcesNextToExecutable()
	if err != nil {
		fatal(err)
	}
	if flag.NArg() > 0 && flag.Arg(0) == "list" {
		printSources(sources)
		return
	}
	if ok, err := existingFirmwareValid(*out); err != nil {
		fatal(err)
	} else if ok {
		fmt.Printf("%s already exists and is valid\n", absolutePath(*out))
		return
	}

	list := append([]source(nil), sources...)
	if *url != "" {
		list = append([]source{{ID: "custom", Name: "custom URL", Extraction: []string{"auto"}, URL: *url, priority: true}}, list...)
	}
	client := &http.Client{Timeout: *timeout}
	var failures []string
	for _, src := range sources {
		path, ok := resolveLocalPath(src.LocalPath)
		if !ok {
			continue
		}
		info, statErr := os.Stat(path)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			failures = append(failures, src.Name+": "+statErr.Error())
			continue
		}
		if info.Size() > maxDownload {
			failures = append(failures, fmt.Sprintf("%s: local package is too large: %d bytes (limit %d)", src.Name, info.Size(), maxDownload))
			continue
		}
		data, err := readFileProgress(path, "Read local package", maxDownload)
		if os.IsNotExist(err) {
			continue
		}
		fmt.Fprintf(os.Stderr, "Local source: %s\n", path)
		if err == nil {
			var fw []byte
			fw, err = extractProgress(data, filepath.Base(path), src.Extraction)
			if err == nil {
				err = writeFirmware(*out, fw)
				if err == nil {
					return
				}
			}
		}
		failures = append(failures, src.Name+": "+err.Error())
	}
	list = rankRemoteSources(client, list)
	for _, src := range list {
		if src.URL == "" {
			continue
		}
		fmt.Fprintf(os.Stderr, "Remote source: %s (%s)\n", src.Name, src.URL)
		data, err := download(client, src.URL, "Download")
		if err == nil {
			p := newProgress("Verify package", 1)
			err = verifyDownload(src, data)
			p.done(err)
		}
		if err == nil {
			var fw []byte
			fw, err = extractProgress(data, src.URL, src.Extraction)
			if err == nil {
				err = writeFirmware(*out, fw)
				if err == nil {
					return
				}
			}
		}
		failures = append(failures, src.Name+": "+err.Error())
	}
	fatal(fmt.Errorf("all sources failed:\n  %s", strings.Join(failures, "\n  ")))
}

type sourceProbe struct {
	source  source
	latency time.Duration
	err     error
	order   int
}

func rankRemoteSources(client *http.Client, list []source) []source {
	remote := make([]sourceProbe, 0, len(list))
	for i, src := range list {
		if src.URL != "" {
			remote = append(remote, sourceProbe{source: src, order: i})
		}
	}
	if len(remote) == 0 {
		return nil
	}

	probeTimeout := 10 * time.Second
	if client.Timeout > 0 && client.Timeout < probeTimeout {
		probeTimeout = client.Timeout
	}
	probeClient := *client
	probeClient.Timeout = probeTimeout
	progress := newProgress("Probe remote sources", int64(len(remote)))
	var wg sync.WaitGroup
	for i := range remote {
		wg.Add(1)
		go func(p *sourceProbe) {
			defer wg.Done()
			start := time.Now()
			p.err = probeURL(&probeClient, p.source.URL)
			p.latency = time.Since(start)
			progress.add(1)
		}(&remote[i])
	}
	wg.Wait()
	progress.done(nil)

	sort.SliceStable(remote, func(i, j int) bool {
		if remote[i].source.priority != remote[j].source.priority {
			return remote[i].source.priority
		}
		if (remote[i].err == nil) != (remote[j].err == nil) {
			return remote[i].err == nil
		}
		if remote[i].err == nil && remote[j].err == nil && remote[i].latency != remote[j].latency {
			return remote[i].latency < remote[j].latency
		}
		return remote[i].order < remote[j].order
	})
	result := make([]source, len(remote))
	for i, p := range remote {
		result[i] = p.source
		if p.err == nil {
			fmt.Fprintf(os.Stderr, "Probe %s: %s\n", p.source.Name, p.latency.Round(time.Millisecond))
		} else {
			fmt.Fprintf(os.Stderr, "Probe %s: unavailable (%v)\n", p.source.Name, p.err)
		}
	}
	return result
}

func probeURL(client *http.Client, url string) error {
	status, err := probeRequest(client, url, http.MethodHead)
	if err != nil {
		return err
	}
	if status < 200 || status >= 400 {
		status, err = probeRequest(client, url, http.MethodGet)
		if err != nil {
			return err
		}
	}
	if status < 200 || status >= 400 {
		return fmt.Errorf("HTTP %d %s", status, http.StatusText(status))
	}
	return nil
}

func probeRequest(client *http.Client, url, method string) (int, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "k223fetch/1.0")
	if method == http.MethodGet {
		req.Header.Set("Range", "bytes=0-0")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

func usage() {
	fmt.Fprintf(flag.CommandLine.Output(), "Usage:\n  k223fetch [flags]\n  k223fetch list\n  k223fetch [flags] extract PACKAGE\n\n")
	flag.PrintDefaults()
}

func printSources(sources []source) {
	for i, s := range sources {
		location := s.URL
		if s.LocalPath != "" {
			location = s.LocalPath
		}
		fmt.Printf("%d. %s\n", i+1, s.Name)
		if s.Size > 0 {
			fmt.Printf("   size: %d; sha256: %s\n", s.Size, s.SHA256)
		}
		fmt.Printf("   extraction: %s\n   %s\n", strings.Join(s.Extraction, " -> "), location)
	}
}

func verifyDownload(src source, data []byte) error {
	if src.Size == 0 && src.SHA256 == "" { // User-supplied URL.
		return nil
	}
	if int64(len(data)) != src.Size {
		return fmt.Errorf("download size mismatch: got %d, expected %d", len(data), src.Size)
	}
	h := sha256.Sum256(data)
	if hex.EncodeToString(h[:]) != src.SHA256 {
		return errors.New("download SHA-256 mismatch")
	}
	return nil
}

func download(c *http.Client, url, label string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "k223fetch/1.0")
	connect := newProgress("Connect", 1)
	resp, err := c.Do(req)
	if err != nil {
		connect.done(err)
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("HTTP %s", resp.Status)
		connect.done(err)
		return nil, err
	}
	connect.done(nil)
	if resp.ContentLength > maxDownload {
		return nil, fmt.Errorf("download is too large: %d bytes", resp.ContentLength)
	}
	total := resp.ContentLength
	if total < 0 {
		total = 0
	}
	p := newProgress(label, total)
	b, err := io.ReadAll(io.LimitReader(progressReader{r: resp.Body, p: p}, maxDownload+1))
	if err == nil && len(b) > maxDownload {
		err = fmt.Errorf("download exceeds %d bytes", maxDownload)
	}
	p.done(err)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func extractProgress(data []byte, name string, plan []string) ([]byte, error) {
	p := newProgress("Extract firmware", 1)
	if len(plan) == 0 {
		plan = []string{"auto"}
	}
	fw, err := extractPlanned(data, name, 0, plan, &extractBudget{bytes: maxExtracted, entries: maxEntries, innoStreams: maxInnoStreams})
	p.done(err)
	return fw, err
}

func extract(data []byte, name string, depth int) ([]byte, error) {
	budget := &extractBudget{bytes: maxExtracted, entries: maxEntries, innoStreams: maxInnoStreams}
	return extractPlanned(data, name, depth, []string{"auto"}, budget)
}

type extractBudget struct {
	bytes       int64
	entries     int
	innoStreams int
}

func (b *extractBudget) consumeEntry(size int64) error {
	if b.entries == 0 {
		return fmt.Errorf("archive contains more than %d entries", maxEntries)
	}
	if size < 0 || size > b.bytes {
		return fmt.Errorf("archive extraction exceeds %d bytes", maxExtracted)
	}
	b.entries--
	b.bytes -= size
	return nil
}

func extractPlanned(data []byte, name string, depth int, plan []string, budget *extractBudget) ([]byte, error) {
	if depth > 4 {
		return nil, errors.New("archive nesting is too deep")
	}
	if len(plan) == 0 {
		if valid(data) {
			return append([]byte(nil), data...), nil
		}
		return nil, fmt.Errorf("%s: extraction plan ended before finding %s", name, firmwareName)
	}
	if plan[0] == "auto" {
		return extractWithBudget(data, name, depth, budget)
	}
	step, rest := plan[0], plan[1:]
	switch step {
	case "raw":
		if valid(data) {
			return append([]byte(nil), data...), nil
		}
		if fw := findRaw(data); fw != nil {
			return fw, nil
		}
	case "intel-hex":
		if fw := findIntelHEX(data); fw != nil {
			return fw, nil
		}
	case "pe", "dmg":
		// Firmware resources are scanned directly in these container bytes; no
		// executable code or filesystem image needs to be mounted.
		return extractPlanned(data, name, depth, rest, budget)
	case "inno-lzma1":
		if fw := findInnoPlanned(data, name, depth, rest, budget); fw != nil {
			return fw, nil
		}
	case "zip":
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, fmt.Errorf("%s: invalid ZIP: %w", name, err)
		}
		for _, f := range zr.File {
			if f.FileInfo().IsDir() || f.UncompressedSize64 > maxDownload {
				continue
			}
			if err := budget.consumeEntry(int64(f.UncompressedSize64)); err != nil {
				return nil, err
			}
			r, err := f.Open()
			if err != nil {
				continue
			}
			entry, readErr := io.ReadAll(io.LimitReader(r, maxDownload+1))
			r.Close()
			if readErr != nil || len(entry) > maxDownload {
				continue
			}
			if fw, err := extractPlanned(entry, f.Name, depth+1, rest, budget); err == nil {
				return fw, nil
			}
		}
	case "rar":
		rr, err := rardecode.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("%s: invalid RAR: %w", name, err)
		}
		for {
			h, nextErr := rr.Next()
			if nextErr != nil {
				break
			}
			if h.IsDir || h.Encrypted || h.UnKnownSize || h.UnPackedSize < 0 || h.UnPackedSize > maxDownload {
				continue
			}
			if err := budget.consumeEntry(h.UnPackedSize); err != nil {
				return nil, err
			}
			entry, readErr := io.ReadAll(io.LimitReader(rr, maxDownload+1))
			if readErr != nil || len(entry) > maxDownload {
				continue
			}
			if fw, err := extractPlanned(entry, h.Name, depth+1, rest, budget); err == nil {
				return fw, nil
			}
		}
	default:
		return nil, fmt.Errorf("unsupported extraction step %q", step)
	}
	return nil, fmt.Errorf("%s: %s extraction did not find %s", name, step, firmwareName)
}

func extractWithBudget(data []byte, name string, depth int, budget *extractBudget) ([]byte, error) {
	if depth > 4 {
		return nil, errors.New("archive nesting is too deep")
	}
	if valid(data) {
		return append([]byte(nil), data...), nil
	}
	if fw := findRaw(data); fw != nil {
		return fw, nil
	}
	if fw := findIntelHEX(data); fw != nil {
		return fw, nil
	}
	if fw := findInno(data, budget); fw != nil {
		return fw, nil
	}

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err == nil {
		for _, f := range zr.File {
			if f.FileInfo().IsDir() || f.UncompressedSize64 > maxDownload {
				continue
			}
			if err := budget.consumeEntry(int64(f.UncompressedSize64)); err != nil {
				return nil, err
			}
			r, e := f.Open()
			if e != nil {
				continue
			}
			b, e := io.ReadAll(io.LimitReader(r, maxDownload+1))
			r.Close()
			if e != nil || len(b) > maxDownload {
				continue
			}
			if fw, e := extractWithBudget(b, f.Name, depth+1, budget); e == nil {
				return fw, nil
			}
		}
	}

	rr, err := rardecode.NewReader(bytes.NewReader(data))
	if err == nil {
		for {
			h, nextErr := rr.Next()
			if nextErr != nil {
				break
			}
			if h.IsDir || h.Encrypted || h.UnKnownSize || h.UnPackedSize < 0 || h.UnPackedSize > maxDownload {
				continue
			}
			if err := budget.consumeEntry(h.UnPackedSize); err != nil {
				return nil, err
			}
			b, readErr := io.ReadAll(io.LimitReader(rr, maxDownload+1))
			if readErr != nil || len(b) > maxDownload {
				continue
			}
			if fw, e := extractWithBudget(b, h.Name, depth+1, budget); e == nil {
				return fw, nil
			}
		}
	}
	return nil, fmt.Errorf("%s: verified %s not found", name, firmwareName)
}

func findInno(data []byte, budget *extractBudget) []byte {
	return scanInno(data, budget, func(unpacked []byte) []byte {
		if fw := findRaw(unpacked); fw != nil {
			return fw
		}
		return findIntelHEX(unpacked)
	})
}

func findInnoPlanned(data []byte, name string, depth int, plan []string, budget *extractBudget) []byte {
	return scanInno(data, budget, func(unpacked []byte) []byte {
		fw, _ := extractPlanned(unpacked, name, depth+1, plan, budget)
		return fw
	})
}

func scanInno(data []byte, budget *extractBudget, inspect func([]byte) []byte) []byte {
	magic := []byte{'z', 'l', 'b', 0x1a}
	for from := 0; ; {
		i := bytes.Index(data[from:], magic)
		if i < 0 {
			return nil
		}
		i += from
		if budget.innoStreams == 0 || budget.bytes == 0 {
			return nil
		}
		budget.innoStreams--
		// Inno LZMA1 chunks contain the standard five property bytes but omit
		// the eight-byte LZMA-Alone size field. Add the unknown-size marker so
		// the bounded pure-Go decoder can consume the solid stream.
		if i+9 <= len(data) {
			header := make([]byte, 13)
			copy(header, data[i+4:i+9])
			for j := 5; j < len(header); j++ {
				header[j] = 0xff
			}
			stream := io.MultiReader(bytes.NewReader(header), bytes.NewReader(data[i+9:]))
			r, err := (lzma.ReaderConfig{DictCap: 64 << 20}).NewReader(stream)
			if err == nil {
				limit := int64(maxInnoOutput)
				if budget.bytes < limit {
					limit = budget.bytes
				}
				unpacked, readErr := io.ReadAll(io.LimitReader(r, limit+1))
				if readErr == nil && int64(len(unpacked)) <= limit {
					budget.bytes -= int64(len(unpacked))
					if fw := inspect(unpacked); fw != nil {
						return fw
					}
				}
			}
		}
		from = i + len(magic)
	}
}

func findRaw(data []byte) []byte {
	needle := []byte(metadata)
	for from := 0; ; {
		i := bytes.Index(data[from:], needle)
		if i < 0 {
			return nil
		}
		i += from
		// Metadata begins at APROM address 0xFE1C.
		base := i - 0xfe1c
		if base >= 0 && base+firmwareSize <= len(data) && valid(data[base:base+firmwareSize]) {
			return append([]byte(nil), data[base:base+firmwareSize]...)
		}
		from = i + 1
	}
}

func findIntelHEX(data []byte) []byte {
	lines := bytes.Split(data, []byte{'\n'})
	image := make([]byte, firmwareSize)
	seen := make([]bool, firmwareSize)
	base, count := uint32(0), 0
	reset := func() { clear(image); clear(seen); base, count = 0, 0 }
	for _, raw := range lines {
		line := bytes.TrimSpace(raw)
		if len(line) < 11 || line[0] != ':' {
			continue
		}
		rec, err := hex.DecodeString(string(line[1:]))
		if err != nil || len(rec) < 5 || int(rec[0])+5 != len(rec) || checksum(rec) != 0 {
			continue
		}
		n, addr, typ := int(rec[0]), uint32(rec[1])<<8|uint32(rec[2]), rec[3]
		switch typ {
		case 0:
			for i, v := range rec[4 : 4+n] {
				a := base + addr + uint32(i)
				if a < firmwareSize {
					image[a] = v
					if !seen[a] {
						seen[a], count = true, count+1
					}
				}
			}
		case 1:
			if count > 0 && valid(image) {
				return append([]byte(nil), image...)
			}
			reset()
		case 4:
			if n == 2 {
				base = (uint32(rec[4])<<8 | uint32(rec[5])) << 16
			}
		}
	}
	if count > 0 && valid(image) {
		return image
	}
	return nil
}

func checksum(b []byte) byte {
	var n byte
	for _, v := range b {
		n += v
	}
	return n
}
func valid(b []byte) bool {
	if len(b) != firmwareSize {
		return false
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]) == firmwareHash
}

func existingFirmwareValid(path string) (bool, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	p := newProgress("Verify existing firmware", 1)
	if !info.Mode().IsRegular() || info.Size() != firmwareSize {
		p.done(nil)
		return false, nil
	}
	b, err := os.ReadFile(path)
	p.done(err)
	if err != nil {
		return false, err
	}
	return valid(b), nil
}

func absolutePath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

func writeFirmware(path string, fw []byte) error {
	if !valid(fw) {
		return errors.New("internal error: firmware hash mismatch")
	}
	if old, err := os.ReadFile(path); err == nil {
		p := newProgress("Verify existing file", 1)
		isValid := valid(old)
		if isValid {
			p.done(nil)
			fmt.Printf("%s already exists and is valid\n", absolutePath(path))
			return nil
		}
		err = fmt.Errorf("refusing to overwrite existing unrecognized file %s", path)
		p.done(err)
		return err
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	p := newProgress("Write firmware", int64(len(fw)))
	_, writeErr := io.Copy(f, progressReader{r: bytes.NewReader(fw), p: p})
	closeErr := f.Close()
	if writeErr == nil {
		writeErr = closeErr
	}
	p.done(writeErr)
	if writeErr != nil {
		_ = os.Remove(path)
		return writeErr
	}
	fmt.Printf("Wrote %s (%d bytes, SHA-256 %s)\n", path, len(fw), firmwareHash)
	return nil
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
