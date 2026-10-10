package krunlet

import (
	"encoding/json"
	"testing"
)

func TestDoctorReportJSONPreservesZeroExitCode(t *testing.T) {
	report := DoctorReport{SmokeAttempted: true, SmokeExitCode: 0}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	exit, ok := result["smoke_exit_code"]
	if !ok || exit != float64(0) {
		t.Fatalf("successful smoke exit code omitted from doctor JSON: %s", data)
	}
}
