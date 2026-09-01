package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/svlocks/sheets/internal/domain"
)

const (
	revisionCursorVersion   = 1
	maxRevisionCursorLength = 1024
	revisionCursorDomain    = "sheets/revision-cursor/checksum/v1\x00"
	revisionPredicate       = "revisions:sealed=1"
)

var revisionPredicateFingerprint = fingerprintRevisionCursorPart(revisionPredicate)

// revisionCursorPayload is deliberately small and canonical. Generation is
// the exact schema fingerprint rather than merely the user_version so a cursor
// cannot silently cross a schema rebuild with different revision semantics.
type revisionCursorPayload struct {
	Version    int    `json:"v"`
	Schema     int    `json:"s"`
	Generation string `json:"g"`
	Order      string `json:"o"`
	Predicate  string `json:"p"`
	Boundary   uint64 `json:"b"`
}

func encodeRevisionCursor(order domain.RevisionOrder, boundary domain.Revision) string {
	payload := revisionCursorPayload{
		Version:    revisionCursorVersion,
		Schema:     schemaVersion,
		Generation: expectedSchemaFingerprint,
		Order:      order.String(),
		Predicate:  revisionPredicateFingerprint,
		Boundary:   uint64(boundary),
	}
	return encodeRevisionCursorPayload(payload)
}

func encodeRevisionCursorPayload(payload revisionCursorPayload) string {
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic("encode revision cursor: " + err.Error())
	}
	sum := checksumRevisionCursor(encoded)
	return base64.RawURLEncoding.EncodeToString(encoded) + "." + base64.RawURLEncoding.EncodeToString(sum[:])
}

func decodeRevisionCursor(cursor string, order domain.RevisionOrder, predicate string) (domain.Revision, error) {
	if len(cursor) == 0 || len(cursor) > maxRevisionCursorLength {
		return 0, invalidRevisionCursor()
	}
	encodedPayload, encodedChecksum, ok := strings.Cut(cursor, ".")
	if !ok || encodedPayload == "" || encodedChecksum == "" || strings.Contains(encodedChecksum, ".") {
		return 0, invalidRevisionCursor()
	}
	payloadBytes, err := base64.RawURLEncoding.Strict().DecodeString(encodedPayload)
	if err != nil || len(payloadBytes) == 0 || len(payloadBytes) > maxRevisionCursorLength {
		return 0, invalidRevisionCursor()
	}
	checksum, err := base64.RawURLEncoding.Strict().DecodeString(encodedChecksum)
	if err != nil || len(checksum) != sha256.Size {
		return 0, invalidRevisionCursor()
	}
	wantChecksum := checksumRevisionCursor(payloadBytes)
	if !bytes.Equal(checksum, wantChecksum[:]) {
		return 0, invalidRevisionCursor()
	}

	decoder := json.NewDecoder(bytes.NewReader(payloadBytes))
	decoder.DisallowUnknownFields()
	var payload revisionCursorPayload
	if err := decoder.Decode(&payload); err != nil {
		return 0, invalidRevisionCursor()
	}
	if err := expectJSONEOF(decoder); err != nil {
		return 0, invalidRevisionCursor()
	}
	canonical, err := json.Marshal(payload)
	if err != nil || !bytes.Equal(canonical, payloadBytes) {
		return 0, invalidRevisionCursor()
	}
	if payload.Version != revisionCursorVersion || payload.Schema != schemaVersion ||
		payload.Generation != expectedSchemaFingerprint || payload.Order != order.String() ||
		payload.Predicate != predicate || payload.Boundary == 0 || payload.Boundary > math.MaxInt64 {
		return 0, invalidRevisionCursor()
	}
	return domain.Revision(payload.Boundary), nil
}

func checksumRevisionCursor(payload []byte) [sha256.Size]byte {
	hash := sha256.New()
	_, _ = hash.Write([]byte(revisionCursorDomain))
	_, _ = hash.Write(payload)
	var sum [sha256.Size]byte
	copy(sum[:], hash.Sum(nil))
	return sum
}

func fingerprintRevisionCursorPart(value string) string {
	sum := sha256.Sum256([]byte("sheets/revision-cursor/fingerprint/v1\x00" + value))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func expectJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("extra JSON value")
		}
		return err
	}
	return nil
}

func invalidRevisionCursor() error {
	return fmt.Errorf("%w: invalid revision cursor", ErrInvalidArgument)
}

func upgradeLegacyRevisionCursor(cursor string) (string, bool) {
	if len(cursor) == 0 || len(cursor) > maxRevisionCursorLength || strings.Contains(cursor, ".") {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(cursor)
	if err != nil || len(decoded) == 0 || len(decoded) > 19 {
		return "", false
	}
	boundary, err := strconv.ParseUint(string(decoded), 10, 63)
	if err != nil || boundary == 0 || strconv.FormatUint(boundary, 10) != string(decoded) {
		return "", false
	}
	return encodeRevisionCursor(domain.RevisionOrderAscending, domain.Revision(boundary)), true
}
