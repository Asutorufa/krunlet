//go:build !linux && !darwin

package nativebundle

func checkExecutableCache(string) error { return nil }
