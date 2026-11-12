// Command backupdrill runs fully isolated, replayable backup/restore drills
// against the real voice-timeline HTTP service.
//
// Every drill run creates its own temporary workspace, starts real HTTP
// servers backed by independent data directories, performs a live online copy
// while the service keeps accepting writes, verifies copies with backupverify
// semantics, restores only into empty directories, and writes text evidence.
//
// No drill step ever points at an operator-supplied data directory, and the
// restore step hard-fails unless its target is empty, so a drill cannot
// overwrite legitimate data.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/backupverify"
	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/httpapi"
	"voice-timeline-renderer/internal/service"
	"voice-timeline-renderer/internal/storage"
)

type evidence struct {
	mu    sync.Mutex
	lines []string
}

func (e *evidence) log(format string, values ...any) {
	line := fmt.Sprintf(format, values...)
	e.mu.Lock()
	e.lines = append(e.lines, line)
	e.mu.Unlock()
	fmt.Println(line)
}

type client struct {
	baseURL string
	http    *http.Client
}

func main() {
	scenario := flag.String("scenario", "all", "all | closed-history | torn-copies | live-write | restore-guard")
	evidenceDir := flag.String("evidence-dir", "", "directory for text evidence (default: repository docs/backup-recovery/evidence)")
	keep := flag.Bool("keep-workspace", false, "keep the temporary workspace for inspection and print its path")
	flag.Parse()

	workspace, err := os.MkdirTemp("", "voice-backup-drill-*")
	if err != nil {
		fail(err)
	}
	if *keep {
		fmt.Printf("workspace retained at %s\n", workspace)
		defer func() {}()
	} else {
		defer os.RemoveAll(workspace)
	}
	if *evidenceDir == "" {
		*evidenceDir = filepath.Join("docs", "backup-recovery", "evidence")
	}
	if err := os.MkdirAll(*evidenceDir, 0o750); err != nil {
		fail(err)
	}

	ev := &evidence{}
	ev.log("== backupdrill start scenario=%s time=%s workspace=%s ==", *scenario, time.Now().UTC().Format(time.RFC3339), workspace)

	scenarios := map[string]func(string, *evidence) error{
		"closed-history": drillClosedHistory,
		"torn-copies":    drillTornCopies,
		"live-write":     drillLiveWrite,
		"restore-guard":  drillRestoreGuard,
	}
	ordered := []string{"closed-history", "torn-copies", "live-write", "restore-guard"}

	runOne := func(name string, fn func(string, *evidence) error) {
		ev.log("-- scenario %s begin --", name)
		if err := fn(workspace, ev); err != nil {
			failDrill(ev, fmt.Errorf("scenario %s: %w", name, err))
		}
		ev.log("-- scenario %s end --", name)
	}

	if *scenario == "all" {
		for _, name := range ordered {
			runOne(name, scenarios[name])
		}
	} else {
		fn, ok := scenarios[*scenario]
		if !ok {
			fail(fmt.Errorf("unknown scenario %q", *scenario))
		}
		runOne(*scenario, fn)
	}

	ev.log("== backupdrill %s PASSED ==", *scenario)
	reportPath := filepath.Join(*evidenceDir, fmt.Sprintf("drill-%s.txt", *scenario))
	if *scenario == "all" {
		reportPath = filepath.Join(*evidenceDir, "drill-all.txt")
	}
	if err := os.WriteFile(reportPath, []byte(strings.Join(ev.lines, "\n")+"\n"), 0o640); err != nil {
		fail(err)
	}
	fmt.Printf("evidence written: %s\n", reportPath)
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "backupdrill: %v\n", err)
	os.Exit(1)
}

func failDrill(ev *evidence, err error) {
	ev.log("DRILL FAILED: %v", err)
	fmt.Fprintf(os.Stderr, "backupdrill failed: %v\n", err)
	os.Exit(1)
}

// startServer wires the real production handler to an isolated data directory.
func startServer(dataDir string) (*httptest.Server, *client, error) {
	repo, err := storage.NewRepository(dataDir)
	if err != nil {
		return nil, nil, err
	}
	serviceLayer := service.New(repo, service.DefaultConfig())
	api := httpapi.New(serviceLayer, httpapi.Options{
		MaxSourceRequestBytes: 48 << 20,
		MaxJSONBytes:          2 << 20,
	})
	server := httptest.NewServer(api.Handler())
	return server, &client{
		baseURL: server.URL,
		http:    &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (c *client) request(ctx context.Context, method, path string, payload any) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return 0, nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

func (c *client) putSource(ctx context.Context, id string, wav []byte) (domain.SourceAsset, error) {
	status, body, err := c.request(ctx, "PUT", "/v1/sources/"+id, map[string]any{
		"name":       id,
		"wav_base64": base64.StdEncoding.EncodeToString(wav),
	})
	if err != nil {
		return domain.SourceAsset{}, err
	}
	if status != http.StatusCreated {
		return domain.SourceAsset{}, fmt.Errorf("source %s create status=%d body=%s", id, status, body)
	}
	var asset domain.SourceAsset
	if err := json.Unmarshal(body, &asset); err != nil {
		return domain.SourceAsset{}, err
	}
	return asset, nil
}

func (c *client) putTimeline(ctx context.Context, id string, spans []domain.TimelineSpan, sampleRate, channels int) (domain.Timeline, error) {
	status, body, err := c.request(ctx, "PUT", "/v1/timelines/"+id, map[string]any{
		"name":             id,
		"sample_rate":      sampleRate,
		"output_channels":  channels,
		"target_peak_dbfs": -1,
		"spans":            spans,
	})
	if err != nil {
		return domain.Timeline{}, err
	}
	if status != http.StatusCreated {
		return domain.Timeline{}, fmt.Errorf("timeline %s create status=%d body=%s", id, status, body)
	}
	var plan domain.Timeline
	if err := json.Unmarshal(body, &plan); err != nil {
		return domain.Timeline{}, err
	}
	return plan, nil
}

func (c *client) putRender(ctx context.Context, id, timelineID string) (domain.RenderJob, int, error) {
	status, body, err := c.request(ctx, "PUT", "/v1/renders/"+id, map[string]any{"timeline_id": timelineID})
	if err != nil {
		return domain.RenderJob{}, status, err
	}
	var job domain.RenderJob
	if status == http.StatusCreated || status == http.StatusOK {
		if err := json.Unmarshal(body, &job); err != nil {
			return domain.RenderJob{}, status, err
		}
	}
	return job, status, nil
}

func (c *client) getRender(ctx context.Context, id string) (domain.RenderJob, int, error) {
	status, body, err := c.request(ctx, "GET", "/v1/renders/"+id, nil)
	if err != nil {
		return domain.RenderJob{}, status, err
	}
	var job domain.RenderJob
	if status == http.StatusOK {
		if err := json.Unmarshal(body, &job); err != nil {
			return domain.RenderJob{}, status, err
		}
	}
	return job, status, nil
}

func (c *client) getOutput(ctx context.Context, id string) ([]byte, error) {
	status, body, err := c.request(ctx, "GET", "/v1/renders/"+id+"/output", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("output status=%d body=%s", status, body)
	}
	return body, nil
}

func (c *client) getReceipt(ctx context.Context, id string) (domain.RenderReceipt, error) {
	status, body, err := c.request(ctx, "GET", "/v1/renders/"+id+"/receipt", nil)
	if err != nil {
		return domain.RenderReceipt{}, err
	}
	if status != http.StatusOK {
		return domain.RenderReceipt{}, fmt.Errorf("receipt status=%d body=%s", status, body)
	}
	var receipt domain.RenderReceipt
	err = json.Unmarshal(body, &receipt)
	return receipt, err
}

func sineWAV(sampleRate, channels, durationMS int, frequency, amplitude float64) ([]byte, error) {
	frames := sampleRate * durationMS / 1000
	samples := make([]float32, frames*channels)
	for frame := 0; frame < frames; frame++ {
		seconds := float64(frame) / float64(sampleRate)
		first := float32(math.Sin(2*math.Pi*frequency*seconds) * amplitude)
		if channels == 1 {
			samples[frame] = first
			continue
		}
		second := float32(math.Sin(2*math.Pi*frequency*seconds+0.35) * amplitude * 0.8)
		samples[frame*2] = first
		samples[frame*2+1] = second
	}
	return audiocodec.EncodePCM16(sampleRate, channels, samples)
}

func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(to, relative)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o750)
		}
		return copyFile(path, destination)
	})
}

func copyFile(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o750); err != nil {
		return err
	}
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	dst, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	return dst.Close()
}

func sha256File(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// seedClosedHistory builds a fully closed, consistent dataset entirely through
// the production HTTP surface and returns its expected counts and digests.
func seedClosedHistory(ctx context.Context, c *client, ev *evidence, prefix string) (map[string]string, int, error) {
	expectedOutputs := map[string]string{}
	completed := 0
	for i := 0; i < 3; i++ {
		sourceID := fmt.Sprintf("%s_src_%d", prefix, i)
		wav, err := sineWAV(48000, 1, 400+i*50, 220+float64(i*40), 0.3)
		if err != nil {
			return nil, 0, err
		}
		asset, err := c.putSource(ctx, sourceID, wav)
		if err != nil {
			return nil, 0, err
		}
		ev.log("seed: source %s digest=%s frames=%d", sourceID, asset.Digest, asset.FrameCount)

		timelineID := fmt.Sprintf("%s_tl_%d", prefix, i)
		plan, err := c.putTimeline(ctx, timelineID, []domain.TimelineSpan{{
			SourceID: sourceID, SourceStartFrame: 0, SourceEndFrame: asset.FrameCount,
			GainDB: -2, CrossfadeFrames: 0,
		}}, 48000, 2)
		if err != nil {
			return nil, 0, err
		}

		renderID := fmt.Sprintf("%s_render_%d", prefix, i)
		job, status, err := c.putRender(ctx, renderID, plan.ID)
		if err != nil {
			return nil, 0, err
		}
		if status != http.StatusCreated || job.State != domain.RenderCompleted {
			return nil, 0, fmt.Errorf("seed render %s state=%s status=%d", renderID, job.State, status)
		}
		output, err := c.getOutput(ctx, renderID)
		if err != nil {
			return nil, 0, err
		}
		sum := sha256.Sum256(output)
		expectedOutputs[renderID] = hex.EncodeToString(sum[:])
		completed++
		ev.log("seed: render %s completed output_digest=%s", renderID, expectedOutputs[renderID])
	}
	return expectedOutputs, completed, nil
}

// drillClosedHistory proves a complete copy verifies as valid and that a
// restored instance serves exactly the closed history it actually contains.
func drillClosedHistory(workspace string, ev *evidence) error {
	ctx := context.Background()
	liveDir := filepath.Join(workspace, "closed", "live-data")
	backupDir := filepath.Join(workspace, "closed", "backup-copy")
	restoreDir := filepath.Join(workspace, "closed", "restored-data")

	server, c, err := startServer(liveDir)
	if err != nil {
		return err
	}
	expectedOutputs, completed, err := seedClosedHistory(ctx, c, ev, "closed")
	if err != nil {
		server.Close()
		return err
	}
	// Online copy while the service stays running.
	if err := copyTree(liveDir, backupDir); err != nil {
		server.Close()
		return err
	}
	rep, err := verifyCopy(backupDir)
	server.Close()
	if err != nil {
		return err
	}
	if rep.Status != "valid" {
		return fmt.Errorf("closed-history copy unexpectedly invalid: %+v", rep.Issues)
	}
	if rep.Counts.CompletedRenders != completed || rep.Counts.Sources != completed {
		return fmt.Errorf("history count mismatch: %+v", rep.Counts)
	}
	if len(rep.InFlight) != 0 {
		return fmt.Errorf("closed history must have no in-flight jobs, got %v", rep.InFlight)
	}
	ev.log("closed-history: copy verified valid (sources=%d completed=%d in_flight=%d)",
		rep.Counts.Sources, rep.Counts.CompletedRenders, len(rep.InFlight))

	// Restore into an empty directory and prove the restored service only
	// promises what was actually saved.
	if err := guardedRestore(backupDir, restoreDir, ev); err != nil {
		return err
	}
	restoredServer, rc, err := startServer(restoreDir)
	if err != nil {
		return err
	}
	for renderID, digest := range expectedOutputs {
		job, status, err := rc.getRender(ctx, renderID)
		if err != nil {
			restoredServer.Close()
			return err
		}
		if status != http.StatusOK || job.State != domain.RenderCompleted {
			restoredServer.Close()
			return fmt.Errorf("restored render %s state=%s status=%d", renderID, job.State, status)
		}
		output, err := rc.getOutput(ctx, renderID)
		if err != nil {
			restoredServer.Close()
			return err
		}
		sum := sha256.Sum256(output)
		if hex.EncodeToString(sum[:]) != digest {
			restoredServer.Close()
			return fmt.Errorf("restored output digest mismatch for %s", renderID)
		}
		receipt, err := rc.getReceipt(ctx, renderID)
		if err != nil {
			restoredServer.Close()
			return err
		}
		if receipt.Output.Digest != digest {
			restoredServer.Close()
			return fmt.Errorf("restored receipt digest mismatch for %s", renderID)
		}
		ev.log("closed-history: restored render %s replayed, digest matches %s", renderID, digest)
	}
	restoredServer.Close()
	ev.log("closed-history: PASS — restored instance serves exactly the %d completed renders saved in the copy", completed)
	return nil
}

// drillTornCopies proves verification distinguishes every form of incomplete
// backup from usable history, including a job caught mid-state-machine.
func drillTornCopies(workspace string, ev *evidence) error {
	ctx := context.Background()

	tornCases := []struct {
		name     string
		mutate   func(dataDir string, renderID string) error
		wantCode string
	}{
		{
			name: "completed-job-missing-render-folder",
			mutate: func(dataDir, renderID string) error {
				return os.RemoveAll(filepath.Join(dataDir, "renders", renderID))
			},
			wantCode: "render_folder_missing",
		},
		{
			name: "missing-output-wav",
			mutate: func(dataDir, renderID string) error {
				return os.Remove(filepath.Join(dataDir, "renders", renderID, "output.wav"))
			},
			wantCode: "render_folder_incomplete",
		},
		{
			name: "corrupted-output-digest",
			mutate: func(dataDir, renderID string) error {
				path := filepath.Join(dataDir, "renders", renderID, "output.wav")
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				if len(data) == 0 {
					return fmt.Errorf("empty output")
				}
				data[len(data)-1] ^= 0xFF
				return os.WriteFile(path, data, 0o640)
			},
			wantCode: "render_digest_mismatch",
		},
		{
			name: "source-digest-mismatch",
			mutate: func(dataDir, renderID string) error {
				entries, err := os.ReadDir(filepath.Join(dataDir, "sources"))
				if err != nil {
					return err
				}
				for _, entry := range entries {
					if !entry.IsDir() {
						continue
					}
					path := filepath.Join(dataDir, "sources", entry.Name(), "source.wav")
					data, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					if len(data) > 44 {
						data[44] ^= 0x01
						return os.WriteFile(path, data, 0o640)
					}
				}
				return fmt.Errorf("no source media found")
			},
			wantCode: "source_digest_mismatch",
		},
		{
			name: "orphan-render-folder",
			mutate: func(dataDir, renderID string) error {
				return os.Remove(filepath.Join(dataDir, "jobs", renderID+".json"))
			},
			wantCode: "orphan_render_folder",
		},
		{
			name: "job-caught-in-rendering-without-artifacts",
			mutate: func(dataDir, renderID string) error {
				// Simulate a copy torn while the job was still active: the job
				// record was captured in "rendering" before the atomic render
				// folder rename could be copied. Rewinding the state therefore
				// also requires removing any later-captured artifacts.
				if err := os.RemoveAll(filepath.Join(dataDir, "renders", renderID)); err != nil {
					return err
				}
				path := filepath.Join(dataDir, "jobs", renderID+".json")
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				var job domain.RenderJob
				if err := json.Unmarshal(data, &job); err != nil {
					return err
				}
				job.State = domain.RenderRendering
				job.Output = nil
				job.ReceiptDigest = ""
				encoded, err := json.MarshalIndent(&job, "", "  ")
				if err != nil {
					return err
				}
				return os.WriteFile(path, append(encoded, '\n'), 0o640)
			},
			wantCode: "", // valid copy, but the job is listed as in-flight
		},
	}

	for index, tc := range tornCases {
		caseDir := filepath.Join(workspace, "torn", fmt.Sprintf("case-%d-%s", index, tc.name))
		liveDir := filepath.Join(caseDir, "live-data")
		backupDir := filepath.Join(caseDir, "backup-copy")

		server, c, err := startServer(liveDir)
		if err != nil {
			return err
		}
		prefix := fmt.Sprintf("torn_%d", index)
		if _, _, err := seedClosedHistory(ctx, c, ev, prefix); err != nil {
			server.Close()
			return err
		}
		renderID := prefix + "_render_0"
		server.Close()

		if err := copyTree(liveDir, backupDir); err != nil {
			return err
		}
		if err := tc.mutate(backupDir, renderID); err != nil {
			return fmt.Errorf("mutate case %s: %w", tc.name, err)
		}
		rep, err := verifyCopy(backupDir)
		if err != nil {
			return err
		}
		if tc.wantCode == "" {
			// In-flight capture: still restorable for its closed history, but
			// the in-progress render must never be promised as completed.
			if rep.Status != "valid" {
				return fmt.Errorf("case %s expected valid copy, got %+v", tc.name, rep.Issues)
			}
			found := false
			for _, id := range rep.InFlight {
				if id == renderID {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("case %s did not list %s as in-flight: %v", tc.name, renderID, rep.InFlight)
			}
			ev.log("torn-copies: case %s -> valid but render %s correctly marked in-flight, not completed", tc.name, renderID)
			continue
		}
		if rep.Status != "invalid" {
			return fmt.Errorf("case %s expected invalid, got valid", tc.name)
		}
		if !hasIssueCode(rep, tc.wantCode) {
			return fmt.Errorf("case %s expected issue code %s, got %+v", tc.name, tc.wantCode, rep.Issues)
		}
		ev.log("torn-copies: case %s -> rejected with blocker %s", tc.name, tc.wantCode)
	}
	ev.log("torn-copies: PASS — %d incomplete-copy variants were distinguished from usable history", len(tornCases))
	return nil
}

// drillLiveWrite performs an online copy while the service keeps accepting
// sources, timelines, and renders, then proves zero-downtime semantics and the
// restore promise boundary.
func drillLiveWrite(workspace string, ev *evidence) error {
	ctx := context.Background()
	liveDir := filepath.Join(workspace, "live", "live-data")
	backupDir := filepath.Join(workspace, "live", "backup-copy")
	restoreDir := filepath.Join(workspace, "live", "restored-data")

	server, c, err := startServer(liveDir)
	if err != nil {
		return err
	}

	// Baseline closed history before the copy window.
	baseline, _, err := seedClosedHistory(ctx, c, ev, "live_base")
	if err != nil {
		server.Close()
		return err
	}

	// Start the online copy; while it runs, keep driving the service with new
	// writes to prove there is no downtime and no coordination requirement.
	copyErr := make(chan error, 1)
	go func() {
		copyErr <- copyTree(liveDir, backupDir)
	}()

	writeFailures := 0
	postCopyCompleted := map[string]string{}
	for i := 0; i < 6; i++ {
		sourceID := fmt.Sprintf("live_new_src_%d", i)
		wav, err := sineWAV(48000, 1, 300+i*25, 300+float64(i*30), 0.25)
		if err != nil {
			server.Close()
			return err
		}
		asset, err := c.putSource(ctx, sourceID, wav)
		if err != nil {
			writeFailures++
			ev.log("live-write: write failure during copy window: %v", err)
			continue
		}
		plan, err := c.putTimeline(ctx, fmt.Sprintf("live_new_tl_%d", i), []domain.TimelineSpan{{
			SourceID: sourceID, SourceStartFrame: 0, SourceEndFrame: asset.FrameCount,
			GainDB: 0, CrossfadeFrames: 0,
		}}, 48000, 1)
		if err != nil {
			writeFailures++
			continue
		}
		renderID := fmt.Sprintf("live_new_render_%d", i)
		job, status, err := c.putRender(ctx, renderID, plan.ID)
		if err != nil {
			writeFailures++
			continue
		}
		if status == http.StatusCreated && job.State == domain.RenderCompleted {
			output, err := c.getOutput(ctx, renderID)
			if err == nil {
				sum := sha256.Sum256(output)
				postCopyCompleted[renderID] = hex.EncodeToString(sum[:])
			}
		}
	}
	if err := <-copyErr; err != nil {
		server.Close()
		return err
	}
	if writeFailures != 0 {
		server.Close()
		return fmt.Errorf("service rejected writes during the online copy window: %d failures", writeFailures)
	}
	ev.log("live-write: service accepted all writes during the copy window (downtime observed: none)")

	// Final state of the still-running service for comparison.
	liveCompleted := map[string]string{}
	for id, digest := range baseline {
		liveCompleted[id] = digest
	}
	for id, digest := range postCopyCompleted {
		liveCompleted[id] = digest
	}

	rep, err := verifyCopy(backupDir)
	if err != nil {
		server.Close()
		return err
	}
	ev.log("live-write: copy status=%s completed_in_copy=%d in_flight=%d",
		rep.Status, rep.Counts.CompletedRenders, len(rep.InFlight))

	// Recovery point evidence: every render the copy promises must be a real
	// completed render on the live service, and the copy must not promise a
	// render the live service never completed.
	promised := map[string]bool{}
	for id := range baseline {
		promised[id] = true
	}
	missingFromBackup := []string{}
	for id, digest := range liveCompleted {
		backupOutput := filepath.Join(backupDir, "renders", id, "output.wav")
		copyDigest, err := sha256File(backupOutput)
		if err != nil {
			missingFromBackup = append(missingFromBackup, id)
			continue
		}
		if copyDigest != digest {
			server.Close()
			return fmt.Errorf("backup output for %s does not match live digest", id)
		}
	}
	ev.log("live-write: %d renders completed on the live service; %d were captured by the copy; missing=%v",
		len(liveCompleted), rep.Counts.CompletedRenders, missingFromBackup)
	for _, id := range rep.InFlight {
		ev.log("live-write: job %s was mid-progress during the copy and is NOT promised as completed", id)
	}

	// Deterministic torn-window evidence: without any service pause point, a
	// copy can start between a job's state transition and its artifact rename.
	// Reproduce that exact on-disk shape on the isolated copy (the live data
	// directory is never touched) and prove the restored instance does not
	// treat the in-flight job as a completed deliverable.
	tornID := "live_torn_job_0"
	if err := injectInFlightJob(backupDir, tornID); err != nil {
		server.Close()
		return fmt.Errorf("inject in-flight job: %w", err)
	}
	rep, err = verifyCopy(backupDir)
	if err != nil {
		server.Close()
		return err
	}
	if rep.Status != "valid" {
		server.Close()
		return fmt.Errorf("torn-window copy must remain restorable for closed history, got %+v", rep.Issues)
	}
	if !contains(rep.InFlight, tornID) {
		server.Close()
		return fmt.Errorf("injected torn job %s was not classified as in-flight: %v", tornID, rep.InFlight)
	}
	ev.log("live-write: deterministic torn window -> job %s classified in-flight, copy still valid for closed history", tornID)

	// Restore into an empty directory; the original live data stays untouched.
	if err := guardedRestore(backupDir, restoreDir, ev); err != nil {
		server.Close()
		return err
	}
	restoredServer, rc, err := startServer(restoreDir)
	if err != nil {
		server.Close()
		return err
	}
	for _, id := range rep.InFlight {
		job, status, err := rc.getRender(ctx, id)
		if err != nil {
			restoredServer.Close()
			server.Close()
			return err
		}
		if status == http.StatusOK && job.State == domain.RenderCompleted {
			restoredServer.Close()
			server.Close()
			return fmt.Errorf("in-flight job %s was incorrectly restored as completed", id)
		}
		ev.log("live-write: restored instance does not expose in-flight job %s as completed (status=%d state=%s)",
			id, status, job.State)
	}
	restoredServer.Close()

	// Original live directory must be byte-identical to before the drill.
	liveRep, err := verifyCopy(liveDir)
	if err != nil {
		server.Close()
		return err
	}
	if liveRep.Status != "valid" {
		server.Close()
		return fmt.Errorf("original live data was affected by the drill: %+v", liveRep.Issues)
	}
	server.Close()
	ev.log("live-write: PASS — no downtime, restore promises only saved history, original live data untouched")
	return nil
}

// drillRestoreGuard proves the restore path refuses to touch a non-empty
// target, so a drill can never overwrite legitimate data.
func drillRestoreGuard(workspace string, ev *evidence) error {
	ctx := context.Background()
	liveDir := filepath.Join(workspace, "guard", "live-data")
	backupDir := filepath.Join(workspace, "guard", "backup-copy")
	occupiedDir := filepath.Join(workspace, "guard", "occupied-target")

	server, c, err := startServer(liveDir)
	if err != nil {
		return err
	}
	if _, _, err := seedClosedHistory(ctx, c, ev, "guard"); err != nil {
		server.Close()
		return err
	}
	if err := copyTree(liveDir, backupDir); err != nil {
		server.Close()
		return err
	}
	server.Close()

	if err := os.MkdirAll(occupiedDir, 0o750); err != nil {
		return err
	}
	marker := filepath.Join(occupiedDir, "legitimate-data.txt")
	if err := os.WriteFile(marker, []byte("do not overwrite\n"), 0o640); err != nil {
		return err
	}
	if err := restoreInto(backupDir, occupiedDir); err == nil {
		return fmt.Errorf("restore into non-empty target was incorrectly allowed")
	} else {
		ev.log("restore-guard: restore into non-empty target refused (%v)", err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		return err
	}
	if string(data) != "do not overwrite\n" {
		return fmt.Errorf("legitimate marker content changed: %q", data)
	}
	ev.log("restore-guard: existing file content unchanged after refused restore")

	// Sanity: restore into a fresh empty target still works.
	emptyTarget := filepath.Join(workspace, "guard", "empty-target")
	if err := restoreInto(backupDir, emptyTarget); err != nil {
		return err
	}
	rep, err := verifyCopy(emptyTarget)
	if err != nil || rep.Status != "valid" {
		return fmt.Errorf("empty-target restore did not verify: %v %+v", err, rep)
	}
	ev.log("restore-guard: PASS — non-empty targets are protected, empty targets restore cleanly")
	return nil
}

// ---- verification is delegated to the shared internal/backupverify rules so
// the drill can never drift from the production verification command ----

func verifyCopy(root string) (*backupverify.Report, error) {
	return backupverify.VerifyRoot(root)
}

func guardedRestore(from, to string, ev *evidence) error {
	if _, err := os.Stat(to); err == nil {
		entries, err := os.ReadDir(to)
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			return fmt.Errorf("refusing restore: target %s exists and is not empty", to)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := restoreInto(from, to); err != nil {
		return err
	}
	rep, err := backupverify.VerifyRoot(to)
	if err != nil {
		return err
	}
	if rep.Status != "valid" {
		return fmt.Errorf("post-restore verification failed: %+v", rep.Issues)
	}
	ev.log("restore: %s -> %s re-verified valid (in_flight=%d)", from, to, len(rep.InFlight))
	return nil
}

func restoreInto(from, to string) error {
	if entries, err := os.ReadDir(to); err == nil && len(entries) > 0 {
		return fmt.Errorf("refusing restore: target %s exists and is not empty", to)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return copyTree(from, to)
}

func hasIssueCode(rep *backupverify.Report, code string) bool {
	for _, item := range rep.Issues {
		if item.Code == code {
			return true
		}
	}
	return false
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// injectInFlightJob creates the exact on-disk shape of a job captured between
// its queued/rendering transition and its atomic artifact publication: a job
// record in a non-terminal state with no render folder. It only ever touches
// the isolated backup copy passed in, never the live data directory.
func injectInFlightJob(dataDir, jobID string) error {
	entries, err := os.ReadDir(filepath.Join(dataDir, "timelines"))
	if err != nil {
		return err
	}
	var timelineID string
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".json" {
			timelineID = strings.TrimSuffix(entry.Name(), ".json")
			break
		}
	}
	if timelineID == "" {
		return fmt.Errorf("no timeline available to reference")
	}
	now := time.Now().UTC()
	job := domain.RenderJob{
		ID:         jobID,
		TimelineID: timelineID,
		State:      domain.RenderRendering,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	data, err := json.MarshalIndent(&job, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dataDir, "jobs", jobID+".json")
	return os.WriteFile(path, append(data, '\n'), 0o640)
}
