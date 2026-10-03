//go:build !darwin && !linux

package main

import "errors"

func processStart(int) processObservation {
	return processObservation{err: errors.New("process start time is not available on this platform")}
}
