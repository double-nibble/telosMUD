// Package account implements telos-account: the accounts/auth service (docs/ACCOUNT.md). It is the only
// service that touches OAuth providers + credentials; the gate reaches it over the Account gRPC API. Phase
// 14.1 lands the service skeleton + the character RPCs (list/reserve/create); the auth-backend RPCs
// (link codes, passphrase, SSH) return Unimplemented until their slices (14.2/14.5/14.6).
package account

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountv1 "github.com/double-nibble/telosmud/api/gen/telosmud/account/v1"
	"github.com/double-nibble/telosmud/internal/store"
)

// CharStore is the persistence surface the service needs (the subset of store.Pool it calls). An interface
// so tests can drive the service with an in-memory fake — no Postgres required for the RPC-shape tests.
type CharStore interface {
	AccountCharacters(ctx context.Context, accountID string) ([]store.CharacterSummary, error)
	NameAvailable(ctx context.Context, name string) (bool, error)
	CreateAccountCharacter(ctx context.Context, accountID, name, zoneRef, roomRef string, state []byte) (string, error)
}

// Service implements the Account gRPC server. It is transport-thin: validation + a store call + a mapping to
// the proto types. The auth backends (Redis link codes, Argon2id, SSH) attach to it in later slices.
type Service struct {
	accountv1.UnimplementedAccountServer
	store CharStore
	log   *slog.Logger
	// startZone/startRoom are the default spawn location a freshly-created character lands in (the demo
	// pack's start room). Config-supplied; later chargen (14.8) may let content choose a starting zone.
	startZone string
	startRoom string
}

// New builds the Account service over a character store.
func New(cs CharStore, log *slog.Logger, startZone, startRoom string) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: cs, log: log, startZone: startZone, startRoom: startRoom}
}

// ListCharacters returns the characters owned by an account (the select menu).
func (s *Service) ListCharacters(ctx context.Context, req *accountv1.ListCharactersRequest) (*accountv1.ListCharactersResponse, error) {
	if req.GetAccountId() == "" {
		return nil, status.Error(codes.InvalidArgument, "account_id required")
	}
	chars, err := s.store.AccountCharacters(ctx, req.GetAccountId())
	if err != nil {
		s.log.Error("ListCharacters", "account", req.GetAccountId(), "err", err)
		return nil, status.Error(codes.Internal, "list characters failed")
	}
	return &accountv1.ListCharactersResponse{Characters: toProtoChars(chars)}, nil
}

// ReserveName checks a candidate character name: the format rules + current availability. It does not write a
// row (creation reserves via the unique constraint); it is the pre-commit check the chargen UI shows.
func (s *Service) ReserveName(ctx context.Context, req *accountv1.ReserveNameRequest) (*accountv1.ReserveNameResponse, error) {
	if reason, ok := ValidateCharacterName(req.GetName()); !ok {
		return &accountv1.ReserveNameResponse{Ok: false, Reason: reason}, nil
	}
	free, err := s.store.NameAvailable(ctx, req.GetName())
	if err != nil {
		s.log.Error("ReserveName", "name", req.GetName(), "err", err)
		return nil, status.Error(codes.Internal, "name check failed")
	}
	if !free {
		return &accountv1.ReserveNameResponse{Ok: false, Reason: "taken"}, nil
	}
	return &accountv1.ReserveNameResponse{Ok: true}, nil
}

// CreateCharacter creates a character on an account. 14.1 lands the name + location write; applying the
// chosen content bundles' grants into the initial state is wired in 14.8 (the chargen front-end), so the
// bundles field is validated + recorded but the grant application is a TODO until then.
func (s *Service) CreateCharacter(ctx context.Context, req *accountv1.CreateCharacterRequest) (*accountv1.CreateCharacterResponse, error) {
	if req.GetAccountId() == "" {
		return nil, status.Error(codes.InvalidArgument, "account_id required")
	}
	if reason, ok := ValidateCharacterName(req.GetName()); !ok {
		return nil, status.Errorf(codes.InvalidArgument, "invalid name: %s", reason)
	}
	// Phase 14.8 will marshal the chosen bundles' applied grants into the initial state; for now a new
	// character starts with empty content state (the established backward-compat default).
	id, err := s.store.CreateAccountCharacter(ctx, req.GetAccountId(), req.GetName(), s.startZone, s.startRoom, nil)
	if err != nil {
		if errors.Is(err, store.ErrNameTaken) {
			return nil, status.Error(codes.AlreadyExists, "name taken")
		}
		s.log.Error("CreateCharacter", "account", req.GetAccountId(), "name", req.GetName(), "err", err)
		return nil, status.Error(codes.Internal, "create character failed")
	}
	return &accountv1.CreateCharacterResponse{Character: &accountv1.Character{
		Id: id, Name: req.GetName(), ZoneRef: s.startZone, RoomRef: s.startRoom,
	}}, nil
}

// toProtoChars maps store summaries onto the proto Character list.
func toProtoChars(chars []store.CharacterSummary) []*accountv1.Character {
	out := make([]*accountv1.Character, 0, len(chars))
	for _, c := range chars {
		out = append(out, &accountv1.Character{Id: c.ID, Name: c.Name, ZoneRef: c.ZoneRef, RoomRef: c.RoomRef})
	}
	return out
}
