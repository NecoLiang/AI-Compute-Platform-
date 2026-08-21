package auth

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestRedisSMSCodeStoreLifecycle(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set TEST_REDIS_ADDR to run the Redis integration check")
	}

	rdb := redis.NewClient(&redis.Options{Addr: addr})
	defer rdb.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	phone := "13800139999"
	purpose := "login"
	phoneKey := hashIdentifier(phone)
	ipKey := hashIdentifier("127.0.0.99")
	keys := []string{
		"auth:sms:cooldown:" + purpose + ":" + phoneKey,
		"auth:sms:phone-hour:" + phoneKey,
		"auth:sms:phone-day:" + phoneKey,
		"auth:sms:ip-minute:" + ipKey,
		"auth:sms:ip-hour:" + ipKey,
		"auth:sms:code:" + purpose + ":" + phoneKey,
		"auth:sms:attempts:" + purpose + ":" + phoneKey,
	}
	defer rdb.Del(context.Background(), keys...)

	store := NewRedisSMSCodeStore(rdb)
	if err := store.Reserve(ctx, phone, purpose, "127.0.0.99"); err != nil {
		t.Fatal(err)
	}
	if err := store.Reserve(ctx, phone, purpose, "127.0.0.99"); !errors.Is(err, ErrSMSRateLimited) {
		t.Fatalf("second reservation error = %v", err)
	}
	if err := store.Save(ctx, phone, purpose, "expected-hash", time.Minute); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.Verify(ctx, phone, purpose, "wrong-hash"); err != nil || ok {
		t.Fatalf("wrong code: ok=%v err=%v", ok, err)
	}
	if ok, err := store.Verify(ctx, phone, purpose, "expected-hash"); err != nil || !ok {
		t.Fatalf("correct code: ok=%v err=%v", ok, err)
	}
	if ok, err := store.Verify(ctx, phone, purpose, "expected-hash"); err != nil || ok {
		t.Fatalf("replayed code: ok=%v err=%v", ok, err)
	}
}
