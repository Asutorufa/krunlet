package krunlet

import (
 "context"
 "encoding/json"
 "os"
 "path/filepath"
 "reflect"
 "runtime"
 "strings"
 "testing"

 "github.com/Asutorufa/krunlet/internal/krunffi"
)

func expectedReviewContainment() string {
 if runtime.GOOS == "linux" || runtime.GOOS == "darwin" { return "process_group" }
 return "none"
}
func assertReviewJSON(t *testing.T, value any, key string, expected string) {
 t.Helper()
 buf, err := json.Marshal(value)
 if err != nil { t.Fatal(err) }
 var record map[string]any
 if err := json.Unmarshal(buf, &record); err != nil { t.Fatal(err) }
 if got, ok := record[key]; !ok || got != expected {
  t.Fatalf("%s: want %s=%q, got %s", key, key, expected, buf)
 }
}
func TestReviewResultShowsContainment(t *testing.T) {
 root := t.TempDir()
 helper := filepath.Join(t.TempDir(), "helper")
 if err := os.WriteFile(helper, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil { t.Fatal(err) }
 var stats Stats
 r, err := New(Options{RootFS: root, HelperPath: helper, OnStats: func(s Stats){ stats = s }})
 if err != nil { t.Fatal(err) }
 result, err := r.Run(context.Background(), Request{Command: []string{"/bin/true"}})
 if err != nil { t.Fatal(err) }
 assertReviewJSON(t, result, "containment", expectedReviewContainment())
 assertReviewJSON(t, stats, "containment", expectedReviewContainment())
}
func TestReviewDoctorShowsContainmentWithoutLibrary(t *testing.T) {
 report, _ := DoctorDetailed(context.Background(), "/definitely/no/libkrun", "")
 assertReviewJSON(t, report, "containment", expectedReviewContainment())
}
func TestReviewMemoryStatsHaveDistinctUnitsAndNullablePeak(t *testing.T) {
 st := reflect.TypeOf(Stats{})
 field, ok := st.FieldByName("MemoryPeakBytes")
 if !ok || field.Type.Kind()!=reflect.Ptr || field.Type.Elem().Kind()!=reflect.Uint64 {
  t.Fatal("Stats.MemoryPeakBytes must be nullable *uint64")
 }
 if _, ok := st.FieldByName("HelperRSSBytes"); !ok {
  t.Fatal("Stats.HelperRSSBytes must be reported in bytes")
 }
 b, err := json.Marshal(Stats{})
 if err != nil { t.Fatal(err) }
 if strings.Contains(string(b), "memory_peak_bytes") {
  t.Fatalf("missing cgroup peak must be omitted, not zero: %s", b)
 }
}
func TestReviewLoadedLibraryVersionStatusIsInspectable(t *testing.T) {
 typ := reflect.TypeOf(krunffi.NativeInspection{})
 if _, ok := typ.FieldByName("VersionStatus"); !ok {
  t.Fatal("native inspection must report actual loaded-library version status")
 }
}
func TestReviewReleaseInstallerRequiresSignature(t *testing.T) {
 workflow, err := os.ReadFile(".github/workflows/release.yml")
 if err != nil { t.Fatal(err) }
 installer, err := os.ReadFile("install.sh")
 if err != nil { t.Fatal(err) }
 if !strings.Contains(string(workflow), "checksums.txt.sig") {
  t.Fatal("release workflow does not sign checksum manifest")
 }
 if !strings.Contains(string(installer), "ssh-keygen -Y verify") || !strings.Contains(string(installer), "checksums.txt.sig") {
  t.Fatal("installer does not verify pinned signing identity before checksums")
 }
}
