//go:build !linux

package config

import "errors"

func exchangeFiles(string, string) error {
	return errors.New("atomic file exchange is not supported on this platform")
}

// Without an atomic exchange, a conditional writer compares just before its
// rename; a hand edit landing in that window can still be overwritten.
const canExchangeFiles = false
