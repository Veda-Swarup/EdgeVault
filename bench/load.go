// Command load runs a small reproducible upload/read workload against EdgeVault.
// It reports measurements but intentionally does not claim target performance.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type timing struct {
	upload, read time.Duration
	bytes        int64
}

func main() {
	base := flag.String("url", "http://localhost:8080", "EdgeVault service base URL")
	volume := flag.String("volume", "benchmark", "new volume name for this run")
	files := flag.Int("files", 100, "number of files")
	size := flag.Int("size-bytes", 65536, "bytes per file")
	workers := flag.Int("workers", 8, "concurrent workers")
	ransomwareFiles := flag.Int("ransomware-files", 3, "files used in the ransomware detection and restore measurement (minimum 3)")
	flag.Parse()
	if *files < 1 || *size < 1 || *workers < 1 || *ransomwareFiles < 3 {
		fmt.Fprintln(os.Stderr, "files, size-bytes, and workers must be positive; ransomware-files must be at least 3")
		os.Exit(2)
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	ctx := context.Background()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, *base+"/v1/volumes", bytes.NewBufferString(fmt.Sprintf(`{"name":%q}`, *volume)))
	resp, err := client.Do(req)
	must(err)
	drainClose(resp)
	if resp.StatusCode != http.StatusCreated {
		fail("create volume: HTTP %d", resp.StatusCode)
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	var done atomic.Int64
	var headSeq atomic.Int64
	results := make(chan timing, *files)
	started := time.Now()
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				data := make([]byte, *size)
				for j := range data {
					data[j] = byte((i*31 + j*17) % 251)
				}
				t0 := time.Now()
				commitSeq, e := upload(client, *base, *volume, fmt.Sprintf("file-%06d.bin", i), data, 0)
				if e != nil {
					fail("PUT %d: %v", i, e)
				}
				updateMax(&headSeq, commitSeq)
				upload := time.Since(t0)
				getURL := fmt.Sprintf("%s/v1/volumes/%s/files/file-%06d.bin", *base, *volume, i)
				t0 = time.Now()
				gr, e := client.Get(getURL)
				if e != nil {
					fail("GET: %v", e)
				}
				body, e := io.ReadAll(gr.Body)
				gr.Body.Close()
				if e != nil || gr.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
					fail("GET verification %d: HTTP %d err=%v", i, gr.StatusCode, e)
				}
				results <- timing{upload: upload, read: time.Since(t0), bytes: int64(len(data))}
				done.Add(1)
			}
		}()
	}
	go func() {
		for i := 0; i < *files; i++ {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()
	var totalBytes int64
	var uploadSum, readSum time.Duration
	var uploadMax, readMax time.Duration
	for r := range results {
		totalBytes += r.bytes
		uploadSum += r.upload
		readSum += r.read
		if r.upload > uploadMax {
			uploadMax = r.upload
		}
		if r.read > readMax {
			readMax = r.read
		}
	}
	workloadElapsed := time.Since(started)
	// Seed low-entropy versions, then encrypt-like random replacements. The
	// clean commit sequence is the restore point and all bytes are verified.
	cleanFiles := make(map[string][]byte, *ransomwareFiles)
	for i := 0; i < *ransomwareFiles; i++ {
		name := fmt.Sprintf("attack/file-%03d.txt", i)
		clean := bytes.Repeat([]byte("c"), *size)
		cleanFiles[name] = clean
		seq, err := upload(client, *base, *volume, name, clean, 0)
		must(err)
		updateMax(&headSeq, seq)
	}
	cleanSeq := headSeq.Load()
	attackStarted := time.Now()
	for name := range cleanFiles {
		attack := make([]byte, *size)
		if _, err := rand.Read(attack); err != nil {
			fail("generate attack payload: %v", err)
		}
		if _, err := upload(client, *base, *volume, name, attack, 1); err != nil {
			fail("attack PUT %s: %v", name, err)
		}
	}
	// A valid leased write must be rejected after the detector opens the alert.
	probeName := "attack-probe.txt"
	token, err := acquireLease(client, *base, *volume, probeName)
	must(err)
	probe, _ := http.NewRequest(http.MethodPut, *base+"/v1/volumes/"+*volume+"/files/"+probeName, bytes.NewReader([]byte("probe")))
	probe.Header.Set("X-Edge-Node", "bench")
	probe.Header.Set("X-Edge-Lease-Token", strconv.FormatInt(token, 10))
	probe.Header.Set("If-Match", "0")
	probeResp, err := client.Do(probe)
	must(err)
	drainClose(probeResp)
	if probeResp.StatusCode != http.StatusLocked {
		fail("ransomware detector did not quarantine writes: probe HTTP %d", probeResp.StatusCode)
	}
	detectionTime := time.Since(attackStarted)
	restoreBody := fmt.Sprintf(`{"as_of_seq":%d}`, cleanSeq)
	restoreReq, _ := http.NewRequest(http.MethodPost, *base+"/v1/volumes/"+*volume+"/restore", strings.NewReader(restoreBody))
	restoreReq.Header.Set("Content-Type", "application/json")
	restoreStarted := time.Now()
	restoreResp, err := client.Do(restoreReq)
	must(err)
	drainClose(restoreResp)
	if restoreResp.StatusCode != http.StatusOK {
		fail("restore as_of_seq=%d: HTTP %d", cleanSeq, restoreResp.StatusCode)
	}
	restoreTime := time.Since(restoreStarted)
	for name, expected := range cleanFiles {
		verify, err := client.Get(*base + "/v1/volumes/" + *volume + "/files/" + name)
		must(err)
		got, err := io.ReadAll(verify.Body)
		verify.Body.Close()
		if err != nil || verify.StatusCode != http.StatusOK || !bytes.Equal(got, expected) {
			fail("restore verification %s: HTTP %d err=%v", name, verify.StatusCode, err)
		}
	}
	out := map[string]any{"files": done.Load(), "bytes": totalBytes, "workers": *workers, "elapsed_seconds": workloadElapsed.Seconds(), "throughput_mib_s": float64(totalBytes) / (1 << 20) / workloadElapsed.Seconds(), "mean_upload_ms": float64(uploadSum.Microseconds()) / 1000 / float64(max64(done.Load(), 1)), "max_upload_ms": float64(uploadMax.Microseconds()) / 1000, "mean_read_ms": float64(readSum.Microseconds()) / 1000 / float64(max64(done.Load(), 1)), "max_read_ms": float64(readMax.Microseconds()) / 1000, "ransomware_files": *ransomwareFiles, "detection_ms": float64(detectionTime.Microseconds()) / 1000, "restore_ms": float64(restoreTime.Microseconds()) / 1000, "restore_bytes_verified": len(cleanFiles) * *size}
	if mr, err := client.Get(*base + "/metrics"); err == nil {
		raw, _ := io.ReadAll(mr.Body)
		mr.Body.Close()
		metrics := parseMetrics(string(raw))
		out["cache_hit_ratio"] = metrics["edge_cache_hits_total"] / maxMetric(metrics["edge_cache_hits_total"]+metrics["edge_cache_misses_total"])
		out["dedup_ratio"] = metrics["edge_dedup_ratio"]
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}

func acquireLease(client *http.Client, base, volume, path string) (int64, error) {
	leaseReq, _ := http.NewRequest(http.MethodPost, base+"/v1/volumes/"+volume+"/files/"+path+"/lease", nil)
	leaseReq.Header.Set("X-Edge-Node", "bench")
	lr, err := client.Do(leaseReq)
	if err != nil {
		return 0, err
	}
	defer lr.Body.Close()
	var lease struct {
		Token int64 `json:"token"`
	}
	if err := json.NewDecoder(lr.Body).Decode(&lease); err != nil {
		return 0, err
	}
	if lr.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("lease response: HTTP %d", lr.StatusCode)
	}
	return lease.Token, nil
}

func upload(client *http.Client, base, volume, path string, data []byte, expected int64) (int64, error) {
	token, err := acquireLease(client, base, volume, path)
	if err != nil {
		return 0, err
	}
	putReq, _ := http.NewRequest(http.MethodPut, base+"/v1/volumes/"+volume+"/files/"+path, bytes.NewReader(data))
	putReq.Header.Set("X-Edge-Node", "bench")
	putReq.Header.Set("X-Edge-Lease-Token", strconv.FormatInt(token, 10))
	putReq.Header.Set("If-Match", strconv.FormatInt(expected, 10))
	resp, err := client.Do(putReq)
	if err != nil {
		return 0, err
	}
	defer drainClose(resp)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("upload %s: HTTP %d: %s", path, resp.StatusCode, body)
	}
	var result struct {
		CommitSeq int64 `json:"commit_seq"`
	}
	// Consume/decode after buffering the response body.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, err
	}
	return result.CommitSeq, nil
}

func updateMax(dst *atomic.Int64, value int64) {
	for old := dst.Load(); value > old; old = dst.Load() {
		if dst.CompareAndSwap(old, value) {
			return
		}
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
func drainClose(r *http.Response) { _, _ = io.Copy(io.Discard, r.Body); _ = r.Body.Close() }
func must(err error) {
	if err != nil {
		fail("%v", err)
	}
}
func fail(format string, args ...any) {
	fmt.Printf("load: "+format+"\n", args...)
	panic("benchmark failed")
}
func parseMetrics(body string) map[string]float64 {
	out := map[string]float64{}
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			if v, e := strconv.ParseFloat(fields[1], 64); e == nil {
				out[fields[0]] = v
			}
		}
	}
	return out
}
func maxMetric(v float64) float64 {
	if v > 0 {
		return v
	}
	return 1
}
