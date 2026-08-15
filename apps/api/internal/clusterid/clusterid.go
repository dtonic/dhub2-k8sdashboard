// Package clusterid defines the canonical identity accepted at every central
// platform data boundary. It intentionally matches the Helm schema.
package clusterid

import (
	"net"
	"regexp"
)

const MaxLength = 63

var pattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,61}[a-z0-9])?$`)
var hostPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

func Valid(value string) bool {
	return pattern.MatchString(value)
}

// ValidHost accepts an IP literal or a bounded DNS name with canonical labels.
func ValidHost(value string) bool {
	return net.ParseIP(value) != nil || len(value) <= 253 && hostPattern.MatchString(value)
}
