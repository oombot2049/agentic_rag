package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"strconv"

	"github.com/agenticrag/agenticrag/tests/load/spec"
)

func main() {
	profilePath := flag.String("profile", "", "profile path")
	field := flag.String("field", "", "field name")
	stageName := flag.String("stage", "", "stage name")
	flag.Parse()
	data, err := os.ReadFile(*profilePath)
	if err != nil {
		fail(err)
	}
	profile, err := spec.LoadProfile(data)
	if err != nil {
		fail(err)
	}
	if *field == "evidence-class" {
		fmt.Println(profile.EvidenceClass)
		return
	}
	var selected *spec.Stage
	for index := range profile.Stages {
		if profile.Stages[index].Name == *stageName {
			selected = &profile.Stages[index]
			break
		}
	}
	if selected == nil {
		fail(fmt.Errorf("unknown stage %q", *stageName))
	}
	switch *field {
	case "turn-rps":
		fmt.Println(strconv.FormatFloat(selected.TurnRPS, 'f', -1, 64))
	case "duration-seconds":
		fmt.Println(selected.DurationSeconds)
	case "expected-requests":
		fmt.Println(int(math.Ceil(selected.TurnRPS * float64(selected.DurationSeconds))))
	case "sse-connections":
		fmt.Println(selected.SSEConnections)
	case "sse-reconnect-rps":
		fmt.Println(strconv.FormatFloat(selected.SSEReconnectRPS, 'f', -1, 64))
	case "has-reconnect":
		fmt.Println(selected.SSEReconnectRPS > 0)
	default:
		fail(fmt.Errorf("unsupported field %q", *field))
	}
}

func fail(err error) { fmt.Fprintln(os.Stderr, "profilemeta:", err); os.Exit(1) }
