package account

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"
)

// deviceauth.go — Phase-15 DEVICE AUTH (docs/PHASE15-PLAN.md), the terminal-native OAuth bridge. The inverse of
// a link code: the TELNET side mints a pending `device_code`; the BROWSER fulfills it. The gate shows a one-
// click link (`/login/<device_code>`), the player completes OAuth in a browser, the broker marks the session
// AUTHED with the resolved account, and the gate's poll picks it up. A device_code is the bearer capability for
// that auth, so it is HIGH-ENTROPY (256-bit, URL-safe) + short-TTL — unguessable, non-replayable.

// deviceCodeTTL is how long a pending device login is valid (a comfortable window to switch to a browser).
const deviceCodeTTL = 10 * time.Minute

// devicePollInterval is the suggested gate poll cadence (seconds), returned to the gate so it doesn't hammer.
const devicePollInterval = 2 * time.Second

// DeviceStatus is the lifecycle of a device login.
type DeviceStatus string

// The device-login lifecycle states.
const (
	DevicePending DeviceStatus = "pending" // minted, waiting for the browser to complete OAuth
	DeviceAuthed  DeviceStatus = "authed"  // the browser finished; account resolved
)

// DeviceAuthStore is the device-session store (Redis in prod, in-memory for tests). Start mints a pending
// session; Authorize (the browser callback) flips it to authed with the resolved account; Poll (the gate)
// reads the status and, once authed, CONSUMES the session (one-shot) and returns the account.
type DeviceAuthStore interface {
	Start(ctx context.Context, ttl time.Duration) (deviceCode string, err error)
	Authorize(ctx context.Context, deviceCode, accountID string) (ok bool, err error)
	Poll(ctx context.Context, deviceCode string) (status DeviceStatus, accountID string, found bool, err error)
}

// newDeviceCode returns a fresh 256-bit URL-safe device code (it rides in a URL path segment, so it must be
// URL-safe and unguessable — far more entropy than a human-typed link code).
func newDeviceCode() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("account: device-code entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// memDeviceAuth is an in-memory DeviceAuthStore for tests + a single-process dev run (no Redis).
type memDeviceAuth struct {
	mu      sync.Mutex
	entries map[string]memDeviceEntry
}

type memDeviceEntry struct {
	status    DeviceStatus
	accountID string
	expires   time.Time
}

// NewMemDeviceAuth builds an in-memory device-auth store (tests / no-Redis dev).
func NewMemDeviceAuth() DeviceAuthStore {
	return &memDeviceAuth{entries: map[string]memDeviceEntry{}}
}

func (s *memDeviceAuth) Start(_ context.Context, ttl time.Duration) (string, error) {
	code, err := newDeviceCode()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[code] = memDeviceEntry{status: DevicePending, expires: time.Now().Add(ttl)}
	return code, nil
}

func (s *memDeviceAuth) Authorize(_ context.Context, deviceCode, accountID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[deviceCode]
	if !ok || time.Now().After(e.expires) {
		return false, nil // unknown / expired
	}
	e.status, e.accountID = DeviceAuthed, accountID
	s.entries[deviceCode] = e
	return true, nil
}

func (s *memDeviceAuth) Poll(_ context.Context, deviceCode string) (DeviceStatus, string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[deviceCode]
	if !ok || time.Now().After(e.expires) {
		return "", "", false, nil // unknown / expired
	}
	if e.status == DeviceAuthed {
		delete(s.entries, deviceCode) // one-shot: consume once the gate picks up the authed result
		return DeviceAuthed, e.accountID, true, nil
	}
	return DevicePending, "", true, nil
}
