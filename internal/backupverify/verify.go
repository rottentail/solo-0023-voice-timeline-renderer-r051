// Package backupverify verifies whether a copy of a voice-timeline data
// directory is a restorable backup, generates content manifests, and performs
// guarded restores into empty targets.
//
// It relies only on the documented on-disk contract of the service: atomic
// source/render folders, JSON metadata, and the render job state machine. It
// never contacts a running service and never mutates the source root.
package backupverify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"voice-timeline-renderer/internal/domain"
)

const SchemaVersion = 1

type Severity string

const (
	SeverityBlocker Severity = "blocker"
	SeverityWarning Severity = "warning"
	SeverityInfo    Severity = "info"
)

type Issue struct {
	Severity Severity `json:"severity"`
	Code     string   `json:"code"`
	Path     string   `json:"path"`
	Message  string   `json:"message"`
}

type FileRecord struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Mode   string `json:"mode"`
	SHA256 string `json:"sha256"`
}

type ClosedHistory struct {
	Sources          int `json:"sources"`
	Timelines        int `json:"timelines"`
	CompletedRenders int `json:"completed_renders"`
	FailedRenders    int `json:"failed_renders"`
}

type Report struct {
	Tool          string        `json:"tool"`
	SchemaVersion int           `json:"schema_version"`
	Root          string        `json:"root"`
	GeneratedAt   time.Time     `json:"generated_at"`
	Status        string        `json:"status"` // valid | invalid
	Counts        ClosedHistory `json:"closed_history"`
	InFlight      []string      `json:"in_flight_jobs"`
	Issues        []Issue       `json:"issues"`
	Files         []FileRecord  `json:"files"`
}

type verifier struct {
	root   string
	report *Report
}

// VerifyRoot checks structural and cross-file consistency of a data-directory
// copy and returns a structured report with status "valid" or "invalid".
func VerifyRoot(root string) (*Report, error) {
	// Resolve symlinks so a path such as backups/current -> rounds/... is
	// treated as the real directory rather than read as a link.
	absolute, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve root: %w", err)
	}
	absolute, err = filepath.Abs(absolute)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, fmt.Errorf("cannot stat root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", absolute)
	}
	v := &verifier{
		root: absolute,
		report: &Report{
			Tool:          "backupverify verify",
			SchemaVersion: SchemaVersion,
			Root:          absolute,
			GeneratedAt:   time.Now().UTC(),
			Status:        "valid",
			InFlight:      []string{},
			Issues:        []Issue{},
			Files:         []FileRecord{},
		},
	}
	v.checkTopLevel()
	v.checkSources()
	v.checkTimelines()
	jobStates := v.checkJobs()
	v.checkRenders(jobStates)
	v.checkTmp()

	records, err := EnumerateFiles(absolute)
	if err != nil {
		return nil, err
	}
	v.report.Files = records
	v.report.Status = DeriveStatus(v.report.Issues)
	return v.report, nil
}

func DeriveStatus(issues []Issue) string {
	for _, item := range issues {
		if item.Severity == SeverityBlocker {
			return "invalid"
		}
	}
	return "valid"
}

func (v *verifier) add(sev Severity, code, path, format string, values ...any) {
	v.report.Issues = append(v.report.Issues, Issue{
		Severity: sev,
		Code:     code,
		Path:     path,
		Message:  fmt.Sprintf(format, values...),
	})
}

func rel(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(relative)
}

func (v *verifier) checkTopLevel() {
	expected := map[string]bool{
		"sources": true, "timelines": true, "jobs": true, "renders": true, "tmp": true,
	}
	entries, err := os.ReadDir(v.root)
	if err != nil {
		v.add(SeverityBlocker, "root_unreadable", ".", "cannot read data root: %v", err)
		return
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		seen[entry.Name()] = true
		if !expected[entry.Name()] {
			v.add(SeverityWarning, "unexpected_top_level_entry", entry.Name(),
				"entry is not part of the documented data layout")
		}
	}
	for name := range expected {
		if !seen[name] {
			v.add(SeverityBlocker, "missing_layout_directory", name,
				"required data directory is missing")
		} else if !isDir(filepath.Join(v.root, name)) {
			v.add(SeverityBlocker, "layout_entry_not_directory", name,
				"documented layout entry is not a directory")
		}
	}
}

func (v *verifier) checkSources() {
	sourceDir := filepath.Join(v.root, "sources")
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		v.add(SeverityBlocker, "sources_unreadable", "sources", "cannot read sources directory: %v", err)
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if isAtomicTemp(name) {
			v.add(SeverityInfo, "atomic_write_temp", filepath.ToSlash(filepath.Join("sources", name)),
				"temporary file from an in-progress atomic write at copy time")
			continue
		}
		if !entry.IsDir() {
			v.add(SeverityBlocker, "unexpected_source_entry", filepath.ToSlash(filepath.Join("sources", name)),
				"sources may only contain one directory per source id")
			continue
		}
		v.report.Counts.Sources++
		dir := filepath.Join(sourceDir, name)
		children, err := os.ReadDir(dir)
		if err != nil {
			v.add(SeverityBlocker, "source_dir_unreadable", rel(v.root, dir), "%v", err)
			continue
		}
		childNames := map[string]bool{}
		for _, child := range children {
			childNames[child.Name()] = true
		}
		for _, required := range []string{"metadata.json", "source.wav"} {
			if !childNames[required] {
				v.add(SeverityBlocker, "source_file_missing", rel(v.root, filepath.Join(dir, required)),
					"source folder is incomplete")
			}
		}
		for childName := range childNames {
			if childName != "metadata.json" && childName != "source.wav" {
				v.add(SeverityBlocker, "unexpected_source_file",
					rel(v.root, filepath.Join(dir, childName)),
					"source folder contains an undocumented file")
			}
		}
		var asset domain.SourceAsset
		if err := readJSONFile(filepath.Join(dir, "metadata.json"), &asset); err != nil {
			v.add(SeverityBlocker, "source_metadata_unreadable", rel(v.root, filepath.Join(dir, "metadata.json")),
				"%v", err)
			continue
		}
		if asset.ID != name {
			v.add(SeverityBlocker, "source_id_mismatch", rel(v.root, dir),
				"metadata id %q does not match folder name %q", asset.ID, name)
		}
		wavPath := filepath.Join(dir, "source.wav")
		digest, size, err := HashFile(wavPath)
		if err != nil {
			v.add(SeverityBlocker, "source_media_unreadable", rel(v.root, wavPath), "%v", err)
			continue
		}
		if digest != asset.Digest {
			v.add(SeverityBlocker, "source_digest_mismatch", rel(v.root, wavPath),
				"stored digest %s does not match actual bytes %s", asset.Digest, digest)
		}
		if asset.ByteCount != size {
			v.add(SeverityBlocker, "source_size_mismatch", rel(v.root, wavPath),
				"metadata byte_count %d does not match actual size %d", asset.ByteCount, size)
		}
	}
}

func (v *verifier) checkTimelines() {
	timelineDir := filepath.Join(v.root, "timelines")
	entries, err := os.ReadDir(timelineDir)
	if err != nil {
		v.add(SeverityBlocker, "timelines_unreadable", "timelines", "cannot read timelines directory: %v", err)
		return
	}
	sourceIDs := v.listSourceIDs()
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(timelineDir, name)
		if isAtomicTemp(name) {
			v.add(SeverityInfo, "atomic_write_temp", rel(v.root, path),
				"temporary file from an in-progress atomic write at copy time")
			continue
		}
		if entry.IsDir() || filepath.Ext(name) != ".json" {
			v.add(SeverityBlocker, "unexpected_timeline_entry", rel(v.root, path),
				"timelines may only contain <timeline_id>.json files")
			continue
		}
		var plan domain.Timeline
		if err := readJSONFile(path, &plan); err != nil {
			v.add(SeverityBlocker, "timeline_unreadable", rel(v.root, path), "%v", err)
			continue
		}
		v.report.Counts.Timelines++
		wantID := name[:len(name)-len(".json")]
		if plan.ID != wantID {
			v.add(SeverityBlocker, "timeline_id_mismatch", rel(v.root, path),
				"timeline id %q does not match file name %q", plan.ID, wantID)
		}
		if plan.State != "ready" {
			v.add(SeverityBlocker, "timeline_not_committed", rel(v.root, path),
				"persisted timeline state is %q, only fully committed ready timelines are restorable", plan.State)
		}
		for index, span := range plan.Spans {
			if !sourceIDs[span.SourceID] {
				v.add(SeverityBlocker, "timeline_source_missing", rel(v.root, path),
					"spans[%d] references source %q which is absent from this copy", index, span.SourceID)
			}
		}
	}
}

func (v *verifier) checkJobs() map[string]domain.RenderState {
	states := map[string]domain.RenderState{}
	jobDir := filepath.Join(v.root, "jobs")
	entries, err := os.ReadDir(jobDir)
	if err != nil {
		v.add(SeverityBlocker, "jobs_unreadable", "jobs", "cannot read jobs directory: %v", err)
		return states
	}
	sourceDigests := v.listSourceDigests()
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(jobDir, name)
		if isAtomicTemp(name) {
			v.add(SeverityInfo, "atomic_write_temp", rel(v.root, path),
				"temporary file from an in-progress atomic write at copy time")
			continue
		}
		if entry.IsDir() || filepath.Ext(name) != ".json" {
			v.add(SeverityBlocker, "unexpected_job_entry", rel(v.root, path),
				"jobs may only contain <render_id>.json files")
			continue
		}
		var job domain.RenderJob
		if err := readJSONFile(path, &job); err != nil {
			v.add(SeverityBlocker, "job_unreadable", rel(v.root, path), "%v", err)
			continue
		}
		wantID := name[:len(name)-len(".json")]
		if job.ID != wantID {
			v.add(SeverityBlocker, "job_id_mismatch", rel(v.root, path),
				"job id %q does not match file name %q", job.ID, wantID)
		}
		renderPath := filepath.Join(v.root, "renders", job.ID)
		_, renderErr := os.Stat(renderPath)
		artifactsExist := renderErr == nil

		switch job.State {
		case domain.RenderQueued, domain.RenderRendering, domain.RenderFinalizing:
			v.report.InFlight = append(v.report.InFlight, job.ID)
			if artifactsExist {
				v.add(SeverityBlocker, "inflight_job_with_artifacts", rel(v.root, renderPath),
					"job is in non-terminal state %s but a render folder already exists", job.State)
			}
		case domain.RenderCompleted:
			v.report.Counts.CompletedRenders++
			v.checkCompletedJob(job, renderPath, sourceDigests)
		case domain.RenderFailed:
			v.report.Counts.FailedRenders++
			if job.Error == nil || job.Error.Code == "" {
				v.add(SeverityBlocker, "failed_job_without_error", rel(v.root, path),
					"failed job has no stable error detail")
			}
			if job.Output != nil || job.ReceiptDigest != "" {
				v.add(SeverityBlocker, "failed_job_claims_output", rel(v.root, path),
					"failed job must not carry output metadata")
			}
			if artifactsExist {
				v.add(SeverityBlocker, "failed_job_with_artifacts", rel(v.root, renderPath),
					"a failed job must never expose a published render folder")
			}
		default:
			v.add(SeverityBlocker, "unknown_job_state", rel(v.root, path),
				"job state %q is outside the documented state machine", job.State)
		}
		states[job.ID] = job.State
	}
	sort.Strings(v.report.InFlight)
	return states
}

func (v *verifier) checkCompletedJob(job domain.RenderJob, renderPath string, sourceDigests map[string]string) {
	if job.Output == nil {
		v.add(SeverityBlocker, "completed_job_without_output", rel(v.root, filepath.Join("jobs", job.ID+".json")),
			"completed job is missing output metadata")
	}
	if job.ReceiptDigest == "" {
		v.add(SeverityBlocker, "completed_job_without_receipt_digest", rel(v.root, filepath.Join("jobs", job.ID+".json")),
			"completed job is missing receipt digest")
	}
	children, err := os.ReadDir(renderPath)
	if err != nil {
		v.add(SeverityBlocker, "render_folder_missing", rel(v.root, renderPath),
			"completed job has no published render folder: %v", err)
		return
	}
	names := map[string]bool{}
	for _, child := range children {
		names[child.Name()] = true
	}
	if !names["output.wav"] || !names["receipt.json"] {
		v.add(SeverityBlocker, "render_folder_incomplete", rel(v.root, renderPath),
			"render folder must contain output.wav and receipt.json")
	}
	for childName := range names {
		if childName != "output.wav" && childName != "receipt.json" {
			v.add(SeverityBlocker, "unexpected_render_file", rel(v.root, filepath.Join(renderPath, childName)),
				"render folder contains an undocumented file")
		}
	}
	var receipt domain.RenderReceipt
	if err := readJSONFile(filepath.Join(renderPath, "receipt.json"), &receipt); err != nil {
		v.add(SeverityBlocker, "receipt_unreadable", rel(v.root, filepath.Join(renderPath, "receipt.json")), "%v", err)
		return
	}
	if receipt.RenderID != job.ID {
		v.add(SeverityBlocker, "receipt_render_id_mismatch", rel(v.root, renderPath),
			"receipt render id %q does not match job id %q", receipt.RenderID, job.ID)
	}
	if receipt.TimelineID != job.TimelineID {
		v.add(SeverityBlocker, "receipt_timeline_mismatch", rel(v.root, renderPath),
			"receipt timeline %q does not match job timeline %q", receipt.TimelineID, job.TimelineID)
	}
	if _, err := os.Stat(filepath.Join(v.root, "timelines", job.TimelineID+".json")); err != nil {
		v.add(SeverityBlocker, "job_timeline_missing", rel(v.root, filepath.Join("jobs", job.ID+".json")),
			"job references timeline %q which is absent from this copy", job.TimelineID)
	}
	outputPath := filepath.Join(renderPath, "output.wav")
	digest, size, err := HashFile(outputPath)
	if err != nil {
		v.add(SeverityBlocker, "render_output_unreadable", rel(v.root, outputPath), "%v", err)
		return
	}
	if digest != receipt.Output.Digest {
		v.add(SeverityBlocker, "render_digest_mismatch", rel(v.root, outputPath),
			"output digest %s does not match receipt %s", digest, receipt.Output.Digest)
	}
	if job.ReceiptDigest != "" && digest != job.ReceiptDigest {
		v.add(SeverityBlocker, "job_receipt_digest_mismatch", rel(v.root, filepath.Join("jobs", job.ID+".json")),
			"job receipt digest %s does not match actual output %s", job.ReceiptDigest, digest)
	}
	if job.Output != nil && job.Output.Digest != digest {
		v.add(SeverityBlocker, "job_output_digest_mismatch", rel(v.root, filepath.Join("jobs", job.ID+".json")),
			"job output digest %s does not match actual output %s", job.Output.Digest, digest)
	}
	if size != receipt.Output.ByteCount {
		v.add(SeverityBlocker, "render_size_mismatch", rel(v.root, outputPath),
			"receipt byte count %d does not match actual size %d", receipt.Output.ByteCount, size)
	}
	for index, span := range receipt.Spans {
		got, ok := sourceDigests[span.SourceID]
		if !ok {
			v.add(SeverityBlocker, "receipt_source_missing", rel(v.root, renderPath),
				"receipt spans[%d] references source %q which is absent", index, span.SourceID)
			continue
		}
		if got != span.SourceDigest {
			v.add(SeverityBlocker, "receipt_source_digest_mismatch", rel(v.root, renderPath),
				"receipt spans[%d] records digest %s but source %s now has %s",
				index, span.SourceDigest, span.SourceID, got)
		}
	}
}

func (v *verifier) checkRenders(jobStates map[string]domain.RenderState) {
	renderDir := filepath.Join(v.root, "renders")
	entries, err := os.ReadDir(renderDir)
	if err != nil {
		v.add(SeverityBlocker, "renders_unreadable", "renders", "cannot read renders directory: %v", err)
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(renderDir, name)
		if isAtomicTemp(name) {
			v.add(SeverityInfo, "atomic_write_temp", rel(v.root, path),
				"temporary entry from an in-progress atomic write at copy time")
			continue
		}
		if !entry.IsDir() {
			v.add(SeverityBlocker, "unexpected_render_entry", rel(v.root, path),
				"renders may only contain one directory per render id")
			continue
		}
		state, ok := jobStates[name]
		if !ok {
			v.add(SeverityBlocker, "orphan_render_folder", rel(v.root, path),
				"published render folder has no corresponding job record")
			continue
		}
		if state != domain.RenderCompleted {
			v.add(SeverityBlocker, "render_folder_for_non_completed_job", rel(v.root, path),
				"render folder exists while job state is %s", state)
		}
	}
}

func (v *verifier) checkTmp() {
	entries, err := os.ReadDir(filepath.Join(v.root, "tmp"))
	if err != nil {
		v.add(SeverityBlocker, "tmp_unreadable", "tmp", "cannot read tmp directory: %v", err)
		return
	}
	for _, entry := range entries {
		v.add(SeverityInfo, "staging_leftover", filepath.ToSlash(filepath.Join("tmp", entry.Name())),
			"staging entry present at copy time; the service cleans these on its own")
	}
}

func (v *verifier) listSourceIDs() map[string]bool {
	ids := map[string]bool{}
	entries, err := os.ReadDir(filepath.Join(v.root, "sources"))
	if err != nil {
		return ids
	}
	for _, entry := range entries {
		if entry.IsDir() {
			ids[entry.Name()] = true
		}
	}
	return ids
}

func (v *verifier) listSourceDigests() map[string]string {
	out := map[string]string{}
	entries, err := os.ReadDir(filepath.Join(v.root, "sources"))
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		var asset domain.SourceAsset
		if err := readJSONFile(filepath.Join(v.root, "sources", entry.Name(), "metadata.json"), &asset); err != nil {
			continue
		}
		out[asset.ID] = asset.Digest
	}
	return out
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// isAtomicTemp matches temp names from writeJSONAtomic (".write-*"). The
// service's MkdirTemp staging directories use "source-*"/"render-*" prefixes
// and live only under tmp/.
func isAtomicTemp(name string) bool {
	return len(name) > len(".write-") && name[:len(".write-")] == ".write-"
}

func readJSONFile(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return nil
}

func HashFile(path string) (string, int64, error) {
	handle, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer handle.Close()
	hasher := sha256.New()
	size, err := io.Copy(hasher, handle)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), size, nil
}

func EnumerateFiles(root string) ([]FileRecord, error) {
	records := []FileRecord{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		digest, _, err := HashFile(path)
		if err != nil {
			return err
		}
		records = append(records, FileRecord{
			Path:   filepath.ToSlash(rel(root, path)),
			Size:   info.Size(),
			Mode:   fmt.Sprintf("%#o", info.Mode().Perm()),
			SHA256: digest,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Path < records[j].Path })
	return records, nil
}

type manifestDocument struct {
	Tool          string       `json:"tool"`
	SchemaVersion int          `json:"schema_version"`
	Root          string       `json:"root"`
	GeneratedAt   time.Time    `json:"generated_at"`
	Files         []FileRecord `json:"files"`
}

func WriteManifest(root, out string) (int, error) {
	records, err := EnumerateFiles(root)
	if err != nil {
		return 0, err
	}
	payload := manifestDocument{
		Tool:          "backupverify manifest",
		SchemaVersion: SchemaVersion,
		Root:          root,
		GeneratedAt:   time.Now().UTC(),
		Files:         records,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return 0, err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
		return 0, err
	}
	if err := os.WriteFile(out, data, 0o640); err != nil {
		return 0, err
	}
	return len(records), nil
}

func CompareManifest(root, manifestPath string) ([]Issue, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	var recorded manifestDocument
	if err := json.Unmarshal(data, &recorded); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	actual, err := EnumerateFiles(root)
	if err != nil {
		return nil, err
	}
	actualByPath := map[string]FileRecord{}
	for _, record := range actual {
		actualByPath[record.Path] = record
	}
	recordedByPath := map[string]FileRecord{}
	for _, record := range recorded.Files {
		recordedByPath[record.Path] = record
	}
	issues := []Issue{}
	for _, record := range recorded.Files {
		got, ok := actualByPath[record.Path]
		if !ok {
			issues = append(issues, Issue{Severity: SeverityBlocker, Code: "manifest_file_missing", Path: record.Path,
				Message: "file recorded in manifest is absent from the copy"})
			continue
		}
		if got.SHA256 != record.SHA256 {
			issues = append(issues, Issue{Severity: SeverityBlocker, Code: "manifest_digest_mismatch", Path: record.Path,
				Message: fmt.Sprintf("manifest digest %s does not match actual %s", record.SHA256, got.SHA256)})
		}
		if got.Size != record.Size {
			issues = append(issues, Issue{Severity: SeverityBlocker, Code: "manifest_size_mismatch", Path: record.Path,
				Message: fmt.Sprintf("manifest size %d does not match actual %d", record.Size, got.Size)})
		}
	}
	for path := range actualByPath {
		if _, ok := recordedByPath[path]; !ok {
			issues = append(issues, Issue{Severity: SeverityBlocker, Code: "manifest_extra_file", Path: path,
				Message: "file exists in the copy but not in the manifest"})
		}
	}
	sortIssues(issues)
	return issues, nil
}

// Restore copies a verified backup into target, which must not exist or be
// empty, then re-verifies the result. The source root is never modified.
func Restore(from, to string) (*Report, error) {
	rep, err := VerifyRoot(from)
	if err != nil {
		return nil, err
	}
	if rep.Status != "valid" {
		return rep, fmt.Errorf("refusing restore: source backup failed verification")
	}
	switch entries, err := os.ReadDir(to); {
	case err == nil && len(entries) > 0:
		return rep, fmt.Errorf("refusing restore: target %s exists and is not empty", to)
	case err == nil:
		// empty directory: acceptable
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(to, 0o750); err != nil {
			return rep, err
		}
	default:
		return rep, err
	}
	for _, child := range []string{"sources", "timelines", "jobs", "renders", "tmp"} {
		src := filepath.Join(from, child)
		dst := filepath.Join(to, child)
		if err := CopyTree(src, dst); err != nil {
			return rep, fmt.Errorf("copy %s: %w", child, err)
		}
	}
	post, err := VerifyRoot(to)
	if err != nil {
		return nil, err
	}
	if post.Status != "valid" {
		return post, fmt.Errorf("restore completed but re-verification of %s failed", to)
	}
	return post, nil
}

func CopyTree(from, to string) error {
	info, err := os.Stat(from)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return os.MkdirAll(to, 0o750)
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", from)
	}
	return filepath.WalkDir(from, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(to, relative)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o750)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		// Only permission bits are valid for open/chmod; DirEntry.Type()
		// carries type bits that would otherwise create a 0000-mode file.
		return copyFile(path, destination, info.Mode().Perm())
	})
}

func copyFile(from, to string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o750); err != nil {
		return err
	}
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	// umask can narrow the mode passed at open time; enforce the source mode.
	return os.Chmod(to, mode)
}

func sortIssues(issues []Issue) {
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Severity != issues[j].Severity {
			return issues[i].Severity < issues[j].Severity
		}
		return issues[i].Path < issues[j].Path
	})
}
