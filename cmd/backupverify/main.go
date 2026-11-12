// Command backupverify verifies whether a copy of a voice-timeline data
// directory is a restorable backup, generates content manifests, and performs
// a guarded restore into an empty target.
//
// Subcommands:
//
//	verify         Check structural and cross-file consistency of a copy.
//	manifest       Write a SHA-256 manifest of every regular file in a root.
//	check-manifest Recompute a manifest and report any drift.
//	restore        Refuse-to-overwrite restore of a verified copy into an
//	               empty or non-existent target, followed by re-verification.
//
// Exit codes: 0 = valid, 2 = verification found blocking inconsistencies,
// 1 = usage or runtime error.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"voice-timeline-renderer/internal/backupverify"
)

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	command := os.Args[1]
	args := os.Args[2:]
	var err error
	switch command {
	case "verify":
		err = runVerify(args)
	case "manifest":
		err = runManifest(args)
	case "check-manifest":
		err = runCheckManifest(args)
	case "restore":
		err = runRestore(args)
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		err = &exitError{code: 1, err: fmt.Errorf("unknown subcommand %q", command)}
	}
	if err != nil {
		var exitErr *exitError
		if errors.As(err, &exitErr) {
			fmt.Fprintf(os.Stderr, "backupverify %s: %v\n", command, exitErr.err)
			os.Exit(exitErr.code)
		}
		fmt.Fprintf(os.Stderr, "backupverify %s: %v\n", command, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `backupverify - backup verification and guarded restore for the voice-timeline data directory

Usage:
  backupverify verify -root DIR [-manifest FILE] [-json FILE]
  backupverify manifest -root DIR -out FILE
  backupverify check-manifest -root DIR -manifest FILE [-json FILE]
  backupverify restore -from DIR -to DIR [-json FILE]

Exit codes: 0 valid, 2 blocking inconsistencies found, 1 usage/runtime error.
`)
}

func runVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	root := flags.String("root", "", "data-directory copy to verify")
	manifestPath := flags.String("manifest", "", "optional manifest to cross-check")
	jsonOut := flags.String("json", "", "write the full JSON report here")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *root == "" {
		return &exitError{1, fmt.Errorf("-root is required")}
	}
	rep, err := backupverify.VerifyRoot(*root)
	if err != nil {
		return &exitError{1, err}
	}
	if *manifestPath != "" {
		manifestIssues, err := backupverify.CompareManifest(*root, *manifestPath)
		if err != nil {
			return &exitError{1, err}
		}
		rep.Issues = append(rep.Issues, manifestIssues...)
		rep.Status = backupverify.DeriveStatus(rep.Issues)
	}
	if err := writeReport(rep, *jsonOut); err != nil {
		return &exitError{1, err}
	}
	printSummary(rep)
	if rep.Status != "valid" {
		return &exitError{2, fmt.Errorf("backup copy %s is not restorable: blocking inconsistencies found", *root)}
	}
	return nil
}

func runManifest(args []string) error {
	flags := flag.NewFlagSet("manifest", flag.ContinueOnError)
	root := flags.String("root", "", "data directory to record")
	out := flags.String("out", "", "manifest output path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *root == "" || *out == "" {
		return &exitError{1, fmt.Errorf("-root and -out are required")}
	}
	count, err := backupverify.WriteManifest(*root, *out)
	if err != nil {
		return &exitError{1, err}
	}
	fmt.Printf("manifest written: %d files -> %s\n", count, *out)
	return nil
}

func runCheckManifest(args []string) error {
	flags := flag.NewFlagSet("check-manifest", flag.ContinueOnError)
	root := flags.String("root", "", "data directory to check")
	manifestPath := flags.String("manifest", "", "manifest to compare against")
	jsonOut := flags.String("json", "", "write the full JSON report here")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *root == "" || *manifestPath == "" {
		return &exitError{1, fmt.Errorf("-root and -manifest are required")}
	}
	issues, err := backupverify.CompareManifest(*root, *manifestPath)
	if err != nil {
		return &exitError{1, err}
	}
	rep := &backupverify.Report{
		Tool:          "backupverify check-manifest",
		SchemaVersion: backupverify.SchemaVersion,
		Root:          *root,
		GeneratedAt:   time.Now().UTC(),
		Status:        backupverify.DeriveStatus(issues),
		Issues:        issues,
	}
	if err := writeReport(rep, *jsonOut); err != nil {
		return &exitError{1, err}
	}
	printSummary(rep)
	if rep.Status != "valid" {
		return &exitError{2, fmt.Errorf("manifest drift detected in %s", *root)}
	}
	return nil
}

func runRestore(args []string) error {
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	from := flags.String("from", "", "verified backup copy")
	to := flags.String("to", "", "restore target (must not exist or be empty)")
	jsonOut := flags.String("json", "", "write the post-restore report here")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *from == "" || *to == "" {
		return &exitError{1, fmt.Errorf("-from and -to are required")}
	}
	rep, err := backupverify.Restore(*from, *to)
	if err != nil {
		if rep != nil && rep.Status == "invalid" {
			printSummary(rep)
			return &exitError{2, err}
		}
		return &exitError{1, err}
	}
	if err := writeReport(rep, *jsonOut); err != nil {
		return &exitError{1, err}
	}
	printSummary(rep)
	fmt.Printf("restore OK: %s -> %s\n", *from, *to)
	if len(rep.InFlight) > 0 {
		fmt.Printf("WARNING: %d in-flight job(s) were captured mid-progress and must be re-submitted with new render IDs: %v\n",
			len(rep.InFlight), rep.InFlight)
	}
	return nil
}

func writeReport(rep *backupverify.Report, path string) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o640)
}

func printSummary(rep *backupverify.Report) {
	fmt.Printf("backupverify status=%s root=%s\n", rep.Status, rep.Root)
	fmt.Printf("closed history: sources=%d timelines=%d completed_renders=%d failed_renders=%d\n",
		rep.Counts.Sources, rep.Counts.Timelines, rep.Counts.CompletedRenders, rep.Counts.FailedRenders)
	fmt.Printf("in-flight jobs (not promised as completed): %v\n", rep.InFlight)
	blockers, warnings, infos := 0, 0, 0
	for _, item := range rep.Issues {
		switch item.Severity {
		case backupverify.SeverityBlocker:
			blockers++
		case backupverify.SeverityWarning:
			warnings++
		default:
			infos++
		}
	}
	fmt.Printf("issues: blockers=%d warnings=%d info=%d\n", blockers, warnings, infos)
	for _, item := range rep.Issues {
		fmt.Printf("  [%s] %s %s: %s\n", item.Severity, item.Code, item.Path, item.Message)
	}
}
