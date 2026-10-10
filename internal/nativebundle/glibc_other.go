//go:build !linux

package nativebundle

func checkGlibcBaseline(string) error { return nil }
