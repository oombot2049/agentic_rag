package redis

import (
	"context"
	"testing"
	"time"
)

func TestValidStreamIDRequiresCanonicalPositiveSequence(t *testing.T) {
	valid := []string{"0-0", "1-0", "9223372036854775807-0"}
	invalid := []string{"", "0-1", "00-0", "+1-0", "-1-0", "1", "1-0-0", "9223372036854775808-0"}
	for _, value := range valid {
		if !validStreamID(value) {
			t.Errorf("validStreamID(%q) = false", value)
		}
	}
	for _, value := range invalid {
		if validStreamID(value) {
			t.Errorf("validStreamID(%q) = true", value)
		}
	}
}

func TestStreamAppendNextValidatesInputWithoutRedis(t *testing.T) {
	client := &Client{maxTTL: time.Hour}
	if _, err := client.StreamAppendNext(context.Background(), Key{}, []byte("x"), time.Minute); err == nil {
		t.Fatal("empty key accepted")
	}
	if _, err := client.StreamAppendNext(context.Background(), Key{value: "key"}, nil, time.Minute); err == nil {
		t.Fatal("empty payload accepted")
	}
}
