// Package idempotency provides canonical command fingerprints and durable claim handling.
package idempotency

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

// Fingerprint is the SHA-256 identity of one normalized command request.
type Fingerprint [sha256.Size]byte

// Request describes only business-semantic fingerprint inputs. Body must be a
// validated typed value with operation defaults already applied.
type Request struct {
	Operation      string
	PathParameters map[string]string
	Body           any
}

// NewFingerprint computes a deterministic, length-framed request digest.
func NewFingerprint(request Request) (Fingerprint, error) {
	if err := validateOperation(request.Operation); err != nil {
		return Fingerprint{}, err
	}
	canonicalBody, err := canonicalJSON(request.Body)
	if err != nil {
		return Fingerprint{}, fmt.Errorf("canonicalize request body: %w", err)
	}
	hasher := sha256.New()
	writeFrame(hasher, []byte(request.Operation))
	keys := make([]string, 0, len(request.PathParameters))
	for key := range request.PathParameters {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	writeUint64(hasher, uint64(len(keys)))
	for _, key := range keys {
		if key == "" {
			return Fingerprint{}, errors.New("path parameter name must not be empty")
		}
		writeFrame(hasher, []byte(key))
		writeFrame(hasher, []byte(request.PathParameters[key]))
	}
	writeFrame(hasher, canonicalBody)
	var result Fingerprint
	copy(result[:], hasher.Sum(nil))
	return result, nil
}

func canonicalJSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("request body contains trailing JSON")
	}
	return json.Marshal(decoded)
}

func writeFrame(writer io.Writer, value []byte) {
	writeUint64(writer, uint64(len(value)))
	_, _ = writer.Write(value)
}

func writeUint64(writer io.Writer, value uint64) {
	var buffer [8]byte
	binary.BigEndian.PutUint64(buffer[:], value)
	_, _ = writer.Write(buffer[:])
}

func (f Fingerprint) String() string { return hex.EncodeToString(f[:]) }
func (f Fingerprint) Bytes() []byte  { return append([]byte(nil), f[:]...) }

func fingerprintFromBytes(value []byte) (Fingerprint, error) {
	if len(value) != sha256.Size {
		return Fingerprint{}, errors.New("stored request hash must be 32 bytes")
	}
	var result Fingerprint
	copy(result[:], value)
	return result, nil
}
