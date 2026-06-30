package account

import (
	"context"
	"errors"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountv1 "github.com/double-nibble/telosmud/api/gen/telosmud/account/v1"
	"github.com/double-nibble/telosmud/internal/passphrase"
)

// passphrase.go — Phase-14.5 passphrase login (ACCOUNT.md §5): VerifyPassphrase (with per-account lockout +
// per-IP throttle) and SetPassphrase. The per-account lockout lives in account_auth (the store); the per-IP
// throttle is an in-memory sliding window here, so a single source can't brute-force across many accounts.

// ipThrottle is a simple per-key fixed-window limiter: at most `limit` attempts per `window`.
type ipThrottle struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string]ipWindow
}

type ipWindow struct {
	count int
	start time.Time
}

func newIPThrottle(limit int, window time.Duration) *ipThrottle {
	return &ipThrottle{limit: limit, window: window, hits: map[string]ipWindow{}}
}

// allow records an attempt from key and reports whether it is within the limit. An empty key (no conn info)
// is always allowed (the throttle is best-effort defense-in-depth atop the per-account lockout).
func (t *ipThrottle) allow(key string) bool {
	if key == "" {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	w := t.hits[key]
	if now.Sub(w.start) > t.window {
		w = ipWindow{start: now}
	}
	w.count++
	t.hits[key] = w
	return w.count <= t.limit
}

// VerifyPassphrase checks a name+passphrase login. It resolves the character's account, enforces the per-IP
// throttle + the per-account lockout, verifies the Argon2id hash, and updates the failure/lockout state. To
// avoid leaking which names exist, an unknown name / unset passphrase / wrong passphrase all return the same
// "bad_credentials" (never "no such account").
func (s *Service) VerifyPassphrase(ctx context.Context, req *accountv1.VerifyPassphraseRequest) (*accountv1.VerifyPassphraseResponse, error) {
	if req.GetName() == "" || req.GetPassphrase() == "" {
		return nil, status.Error(codes.InvalidArgument, "name and passphrase required")
	}
	if s.ipThrottle != nil && !s.ipThrottle.allow(req.GetConnInfo()) {
		//nolint:gosec // window is a positive throttle duration; its ms value is non-negative
		return &accountv1.VerifyPassphraseResponse{Ok: false, Reason: "locked", RetryAfterMs: uint64(s.ipThrottle.window.Milliseconds())}, nil
	}
	accountID, found, err := s.store.CharacterAccount(ctx, req.GetName())
	if err != nil {
		s.log.Error("VerifyPassphrase: resolve account", "err", err)
		return nil, status.Error(codes.Internal, "auth failed")
	}
	if !found {
		return &accountv1.VerifyPassphraseResponse{Ok: false, Reason: "bad_credentials"}, nil
	}
	auth, hasAuth, err := s.store.AccountAuth(ctx, accountID)
	if err != nil {
		s.log.Error("VerifyPassphrase: read auth", "err", err)
		return nil, status.Error(codes.Internal, "auth failed")
	}
	if !hasAuth || auth.Hash == "" {
		return &accountv1.VerifyPassphraseResponse{Ok: false, Reason: "bad_credentials"}, nil
	}
	if !auth.LockedUntil.IsZero() && auth.LockedUntil.After(s.now()) {
		retry := time.Until(auth.LockedUntil)
		//nolint:gosec // retry > 0 here (LockedUntil is in the future); its ms value is non-negative
		return &accountv1.VerifyPassphraseResponse{Ok: false, Reason: "locked", RetryAfterMs: uint64(retry.Milliseconds())}, nil
	}
	if err := passphrase.Verify(req.GetPassphrase(), auth.Hash); err != nil {
		if !errors.Is(err, passphrase.ErrMismatch) {
			s.log.Warn("VerifyPassphrase: malformed stored hash", "account", accountID, "err", err)
		}
		if _, ferr := s.store.RecordAuthFailure(ctx, accountID, passphraseLockAfter, passphraseLockFor); ferr != nil {
			s.log.Error("VerifyPassphrase: record failure", "err", ferr)
		}
		return &accountv1.VerifyPassphraseResponse{Ok: false, Reason: "bad_credentials"}, nil
	}
	if err := s.store.ResetAuthFailures(ctx, accountID); err != nil {
		s.log.Error("VerifyPassphrase: reset failures", "err", err)
	}
	return &accountv1.VerifyPassphraseResponse{Ok: true, AccountId: accountID}, nil
}

// ResolveSSHKey maps an SSH key fingerprint to its account (Phase 14.6, the gate's SSH server calls this
// after the SSH layer has authenticated the key). found=false for an unknown key — the gate then falls back
// to interactive login over the encrypted channel.
func (s *Service) ResolveSSHKey(ctx context.Context, req *accountv1.ResolveSSHKeyRequest) (*accountv1.ResolveSSHKeyResponse, error) {
	if req.GetFingerprint() == "" {
		return nil, status.Error(codes.InvalidArgument, "fingerprint required")
	}
	accountID, found, err := s.store.ResolveSSHKey(ctx, req.GetFingerprint())
	if err != nil {
		s.log.Error("ResolveSSHKey", "err", err)
		return nil, status.Error(codes.Internal, "resolve failed")
	}
	return &accountv1.ResolveSSHKeyResponse{Found: found, AccountId: accountID}, nil
}

// SetPassphrase sets an account's passphrase (Argon2id-hashed). Called from the website. An empty passphrase
// is rejected here (clearing/disabling is a separate flow); a set resets any prior lockout (SetPassphraseHash).
func (s *Service) SetPassphrase(ctx context.Context, req *accountv1.SetPassphraseRequest) (*accountv1.SetPassphraseResponse, error) {
	if req.GetAccountId() == "" || req.GetPassphrase() == "" {
		return nil, status.Error(codes.InvalidArgument, "account_id and a non-empty passphrase required")
	}
	hash, err := passphrase.Hash(req.GetPassphrase(), s.hashParams)
	if err != nil {
		s.log.Error("SetPassphrase: hash", "err", err)
		return nil, status.Error(codes.Internal, "set passphrase failed")
	}
	if err := s.store.SetPassphraseHash(ctx, req.GetAccountId(), hash); err != nil {
		s.log.Error("SetPassphrase: store", "err", err)
		return nil, status.Error(codes.Internal, "set passphrase failed")
	}
	return &accountv1.SetPassphraseResponse{}, nil
}
