// Package redis provides tenant-safe Redis/Valkey transient infrastructure.
package redis

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const maxKeyBytes = 512

var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type Namespace string

const (
	NamespaceCache       Namespace = "cache"
	NamespaceRate        Namespace = "rate"
	NamespaceEvents      Namespace = "events"
	NamespaceLock        Namespace = "lock"
	NamespaceIdempotency Namespace = "idempotency"
)

func (n Namespace) valid() bool {
	switch n {
	case NamespaceCache, NamespaceRate, NamespaceEvents, NamespaceLock, NamespaceIdempotency:
		return true
	default:
		return false
	}
}

type Key struct{ value string }

func (k Key) String() string { return k.value }

type KeyBuilder struct{ environment string }

func NewKeyBuilder(environment string) (KeyBuilder, error) {
	environment = strings.TrimSpace(environment)
	if !segmentPattern.MatchString(environment) {
		return KeyBuilder{}, errors.New("redis key environment is invalid")
	}
	return KeyBuilder{environment: environment}, nil
}

func (b KeyBuilder) Build(tenantID string, namespace Namespace, segments ...string) (Key, error) {
	if !segmentPattern.MatchString(tenantID) {
		return Key{}, errors.New("redis key tenant is invalid")
	}
	if !namespace.valid() {
		return Key{}, errors.New("redis key namespace is invalid")
	}
	if len(segments) == 0 {
		return Key{}, errors.New("redis key requires a logical segment")
	}
	parts := make([]string, 0, len(segments)+4)
	parts = append(parts, "ar", b.environment, "{"+tenantID+"}", string(namespace))
	for _, segment := range segments {
		if !segmentPattern.MatchString(segment) {
			return Key{}, errors.New("redis key logical segment is invalid")
		}
		parts = append(parts, segment)
	}
	encoded := strings.Join(parts, ":")
	if len(encoded) > maxKeyBytes {
		return Key{}, fmt.Errorf("redis key exceeds %d bytes", maxKeyBytes)
	}
	return Key{value: encoded}, nil
}
