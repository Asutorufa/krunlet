//go:build !linux && !darwin

package krunlet

func watchHelperParent() error {return nil}
