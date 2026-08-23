package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/agenticrag/agenticrag/tests/load/spec"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "loadreport:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("loadreport", flag.ContinueOnError)
	profilePath := flags.String("profile", "", "versioned load profile")
	measurementPath := flags.String("measurement", "", "normalized measurement")
	outputPath := flags.String("output", "", "report destination (stdout when empty)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *profilePath == "" || *measurementPath == "" {
		return errors.New("-profile and -measurement are required")
	}
	profileData, err := os.ReadFile(*profilePath)
	if err != nil {
		return fmt.Errorf("read profile: %w", err)
	}
	profile, err := spec.LoadProfile(profileData)
	if err != nil {
		return fmt.Errorf("validate profile: %w", err)
	}
	measurementData, err := os.ReadFile(*measurementPath)
	if err != nil {
		return fmt.Errorf("read measurement: %w", err)
	}
	measurement, err := spec.LoadMeasurement(measurementData, profile)
	if err != nil {
		return fmt.Errorf("validate measurement: %w", err)
	}
	report, err := spec.Analyze(profile, measurement)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if *outputPath == "" {
		_, err = os.Stdout.Write(encoded)
	} else {
		err = os.WriteFile(*outputPath, encoded, 0o600)
	}
	if err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	if !report.Passed {
		return errors.New("capacity thresholds failed")
	}
	return nil
}
