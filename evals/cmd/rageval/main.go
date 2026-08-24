// Command rageval evaluates RAG predictions against a digest-bound Golden Set.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/agenticrag/agenticrag/evals/rag"
)

type paths struct {
	golden      string
	predictions string
	baseline    string
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("rageval", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var files paths
	flags.StringVar(&files.golden, "golden", "", "path to versioned Golden Set JSON")
	flags.StringVar(&files.predictions, "predictions", "", "path to prediction JSON")
	flags.StringVar(&files.baseline, "baseline", "", "path to digest-bound baseline JSON")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if files.golden == "" || files.predictions == "" || files.baseline == "" || flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "golden, predictions, and baseline paths are required")
		return 2
	}
	report, err := evaluate(files)
	if report.SchemaVersion != 0 {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if encodeErr := encoder.Encode(report); encodeErr != nil {
			_, _ = fmt.Fprintln(stderr, "write evaluation report failed")
			return 1
		}
	}
	if err != nil {
		if errors.Is(err, rag.ErrGateFailed) {
			_, _ = fmt.Fprintln(stderr, "RAG evaluation gate failed")
		} else {
			_, _ = fmt.Fprintln(stderr, "RAG evaluation input is invalid")
		}
		return 1
	}
	return 0
}

func evaluate(files paths) (rag.Report, error) {
	goldenData, err := os.ReadFile(files.golden)
	if err != nil {
		return rag.Report{}, err
	}
	dataset, err := rag.LoadDataset(goldenData)
	if err != nil {
		return rag.Report{}, err
	}
	predictionData, err := os.ReadFile(files.predictions)
	if err != nil {
		return rag.Report{}, err
	}
	predictions, err := rag.LoadPredictions(predictionData, dataset)
	if err != nil {
		return rag.Report{}, err
	}
	baselineData, err := os.ReadFile(files.baseline)
	if err != nil {
		return rag.Report{}, err
	}
	baseline, err := rag.LoadBaseline(baselineData)
	if err != nil {
		return rag.Report{}, err
	}
	report, err := rag.Evaluate(dataset, predictions)
	if err != nil {
		return rag.Report{}, err
	}
	return rag.ApplyBaseline(report, baseline)
}
