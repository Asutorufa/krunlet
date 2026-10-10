package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestParseNetworkRule(t *testing.T) {
	for _, tt := range []struct {
		spec, cidr string
		port       uint16
		proto      string
	}{
		{"1.1.1.1", "1.1.1.1", 0, ""},
		{"2001:db8::/32,443,tcp", "2001:db8::/32", 443, "tcp"},
		{"8.8.8.8,53,udp", "8.8.8.8", 53, "udp"},
	} {
		r, err := parseNetworkRule(tt.spec)
		if err != nil || r.CIDR != tt.cidr || r.Port != tt.port || r.Protocol != tt.proto {
			t.Fatalf("%q parsed %+v: %v", tt.spec, r, err)
		}
	}
	for _, spec := range []string{"", ",443,tcp", "1.1.1.1,0", "1.1.1.1,65536", "1.1.1.1,abc", "1.1.1.1,443,tcp,extra"} {
		if _, err := parseNetworkRule(spec); err == nil {
			t.Fatalf("accepted %q", spec)
		}
	}
}

func TestLicensesCLIContainsCompleteTerms(t *testing.T) {
	beforeArgs, beforeOut := os.Args, os.Stdout
	defer func() { os.Args, os.Stdout = beforeArgs, beforeOut }()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	os.Args = []string{"krunlet", "licenses"}
	status := run()
	_ = writer.Close()
	result, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if status != 0 {
		t.Fatalf("licenses exited %d", status)
	}
	for _, fragment := range []string{"Apache License", "GNU LESSER GENERAL PUBLIC LICENSE", "GNU GENERAL PUBLIC LICENSE"} {
		if !strings.Contains(string(result), fragment) {
			t.Fatalf("missing %s", fragment)
		}
	}
}
