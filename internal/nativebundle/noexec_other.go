//go:build !linux

package nativebundle

func checkExecutableCache(string) error { return nil }
