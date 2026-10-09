//go:build !linux && !darwin

package krunlet

import (
	"errors"
	"os"
)

func lockTempMarker(_ *os.File, _ bool) error {
	return errors.New("temporary rootfs locks require Unix")
}
func openTempMarker(path string) (*os.File, error) {
	return os.Open(path)
}
