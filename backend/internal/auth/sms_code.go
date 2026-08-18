package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	smsCooldown       = 60 * time.Second
	smsPhoneHourlyMax = 5
	smsPhoneDailyMax  = 10
	smsIPMinuteMax    = 10
	smsIPHourlyMax    = 50
	smsVerifyAttempts = 5
)

var reserveSMSCode = redis.NewScript(`
if redis.call("EXISTS", KEYS[1]) == 1 then return 1 end
if tonumber(redis.call("GET", KEYS[2]) or "0") >= tonumber(ARGV[2]) then return 2 end
if tonumber(redis.call("GET", KEYS[3]) or "0") >= tonumber(ARGV[3]) then return 3 end
if tonumber(redis.call("GET", KEYS[4]) or "0") >= tonumber(ARGV[4]) then return 4 end
if tonumber(redis.call("GET", KEYS[5]) or "0") >= tonumber(ARGV[5]) then return 5 end
redis.call("SET", KEYS[1], "1", "EX", ARGV[1])
local phone_hour = redis.call("INCR", KEYS[2])
if phone_hour == 1 then redis.call("EXPIRE", KEYS[2], 3600) end
local phone_day = redis.call("INCR", KEYS[3])
if phone_day == 1 then redis.call("EXPIRE", KEYS[3], 86400) end
local ip_minute = redis.call("INCR", KEYS[4])
if ip_minute == 1 then redis.call("EXPIRE", KEYS[4], 60) end
local ip_hour = redis.call("INCR", KEYS[5])
if ip_hour == 1 then redis.call("EXPIRE", KEYS[5], 3600) end
return 0
`)

var verifySMSCode = redis.NewScript(`
local stored = redis.call("GET", KEYS[1])
if not stored then return 0 end
if stored == ARGV[1] then
  redis.call("DEL", KEYS[1], KEYS[2])
  return 1
end
local attempts = redis.call("INCR", KEYS[2])
if attempts == 1 then
  local ttl = redis.call("PTTL", KEYS[1])
  if ttl > 0 then redis.call("PEXPIRE", KEYS[2], ttl) end
end
if attempts >= tonumber(ARGV[2]) then redis.call("DEL", KEYS[1], KEYS[2]) end
return -1
`)

type RedisSMSCodeStore struct {
	rdb *redis.Client
}

func NewRedisSMSCodeStore(rdb *redis.Client) *RedisSMSCodeStore {
	return &RedisSMSCodeStore{rdb: rdb}
}

func (s *RedisSMSCodeStore) Reserve(ctx context.Context, phone, purpose, clientIP string) error {
	if s == nil || s.rdb == nil {
		return errors.New("redis is not configured")
	}
	phoneKey := hashIdentifier(phone)
	ipKey := hashIdentifier(clientIP)
	result, err := reserveSMSCode.Run(ctx, s.rdb, []string{
		"auth:sms:cooldown:" + purpose + ":" + phoneKey,
		"auth:sms:phone-hour:" + phoneKey,
		"auth:sms:phone-day:" + phoneKey,
		"auth:sms:ip-minute:" + ipKey,
		"auth:sms:ip-hour:" + ipKey,
	}, int(smsCooldown/time.Second), smsPhoneHourlyMax, smsPhoneDailyMax, smsIPMinuteMax, smsIPHourlyMax).Int()
	if err != nil {
		return err
	}
	if result != 0 {
		return ErrSMSRateLimited
	}
	return nil
}

func (s *RedisSMSCodeStore) Save(ctx context.Context, phone, purpose, codeHash string, ttl time.Duration) error {
	return s.rdb.Set(ctx, "auth:sms:code:"+purpose+":"+hashIdentifier(phone), codeHash, ttl).Err()
}

func (s *RedisSMSCodeStore) Verify(ctx context.Context, phone, purpose, codeHash string) (bool, error) {
	result, err := verifySMSCode.Run(ctx, s.rdb, []string{
		"auth:sms:code:" + purpose + ":" + hashIdentifier(phone),
		"auth:sms:attempts:" + purpose + ":" + hashIdentifier(phone),
	}, codeHash, smsVerifyAttempts).Int()
	if err != nil {
		return false, err
	}
	return result == 1, nil
}

func hashIdentifier(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}
