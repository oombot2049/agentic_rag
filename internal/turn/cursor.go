package turn

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
)

const (
	cursorVersion = byte(1)
	maximumCursor = 512
)

type CursorCodec struct{ key []byte }

func NewCursorCodec(key []byte) (*CursorCodec, error) {
	if len(key) < 32 || len(key) > 4096 {
		return nil, errors.New("cursor signing key must contain 32..4096 bytes")
	}
	return &CursorCodec{key: append([]byte(nil), key...)}, nil
}

func (c *CursorCodec) Encode(tenantID domain.TenantID, sessionID domain.SessionID, cursor MessageCursor) (string, error) {
	if c == nil || len(c.key) < 32 {
		return "", errors.New("cursor codec is not initialized")
	}
	if _, err := domain.ParseTenantID(tenantID.String()); err != nil {
		return "", err
	}
	if _, err := domain.ParseSessionID(sessionID.String()); err != nil {
		return "", err
	}
	if _, err := domain.ParseMessageID(cursor.ID.String()); err != nil {
		return "", err
	}
	if cursor.CreatedAt.IsZero() || cursor.CreatedAt.UnixNano() <= 0 {
		return "", errors.New("cursor time is invalid")
	}
	id := []byte(cursor.ID.String())
	payload := make([]byte, 1+8+2+len(id))
	payload[0] = cursorVersion
	binary.BigEndian.PutUint64(payload[1:9], uint64(cursor.CreatedAt.UTC().UnixNano()))
	binary.BigEndian.PutUint16(payload[9:11], uint16(len(id)))
	copy(payload[11:], id)
	signed := append(payload, c.sign(tenantID, sessionID, payload)...)
	return base64.RawURLEncoding.EncodeToString(signed), nil
}

func (c *CursorCodec) Decode(tenantID domain.TenantID, sessionID domain.SessionID, token string) (MessageCursor, error) {
	if c == nil || len(c.key) < 32 || len(token) == 0 || len(token) > maximumCursor {
		return MessageCursor{}, errors.New("cursor is invalid")
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != token || len(raw) < 11+8+sha256.Size {
		return MessageCursor{}, errors.New("cursor encoding is invalid")
	}
	payload, suppliedMAC := raw[:len(raw)-sha256.Size], raw[len(raw)-sha256.Size:]
	expectedMAC := c.sign(tenantID, sessionID, payload)
	if subtle.ConstantTimeCompare(suppliedMAC, expectedMAC) != 1 || payload[0] != cursorVersion {
		return MessageCursor{}, errors.New("cursor signature is invalid")
	}
	idLength := int(binary.BigEndian.Uint16(payload[9:11]))
	if idLength < 8 || len(payload) != 11+idLength {
		return MessageCursor{}, errors.New("cursor payload is invalid")
	}
	id, err := domain.ParseMessageID(string(payload[11:]))
	if err != nil {
		return MessageCursor{}, fmt.Errorf("cursor message ID: %w", err)
	}
	nanoseconds := int64(binary.BigEndian.Uint64(payload[1:9]))
	if nanoseconds <= 0 {
		return MessageCursor{}, errors.New("cursor time is invalid")
	}
	return MessageCursor{CreatedAt: time.Unix(0, nanoseconds).UTC(), ID: id}, nil
}

func (c *CursorCodec) sign(tenantID domain.TenantID, sessionID domain.SessionID, payload []byte) []byte {
	mac := hmac.New(sha256.New, c.key)
	for _, value := range [][]byte{[]byte(tenantID.String()), []byte(sessionID.String()), payload} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		_, _ = mac.Write(length[:])
		_, _ = mac.Write(value)
	}
	return mac.Sum(nil)
}
