package auth

import (
	"context"
	"strings"
	"time"
)

// RotateResult is the outcome of an atomic refresh-token rotation attempt.
type RotateResult int

const (
	// RotateOK means the presented token id matched the stored one and the
	// store now holds the new token id.
	RotateOK RotateResult = iota
	// RotateGraceReuse means the presented token id matched the previous
	// (just-rotated) one within the grace window. The chain does NOT advance;
	// the caller must re-issue a token pair bound to the returned current
	// token id. This keeps double-fire refreshes (multi-tab races, lost
	// responses retried) from being punished as reuse.
	RotateGraceReuse
	// RotateMismatch means the presented token id matches neither the stored
	// one nor the grace-window previous one: the token was rotated too long
	// ago (potential reuse).
	RotateMismatch
	// RotateMissing means there is no rotation record for the key (session
	// predates rotation, or the record expired/was lost).
	RotateMissing
)

// RefreshRotationStore tracks the currently valid refresh token id (jti) per
// session/admin so refresh tokens rotate on every use and reuse of an old
// token can be detected.
type RefreshRotationStore interface {
	// Register stores the currently valid refresh token id for the key,
	// starting a fresh chain: any previously rotated id must not be accepted
	// by a later Rotate.
	Register(ctx context.Context, key, tokenID string, ttl time.Duration) error
	// Rotate atomically compares the stored token id with presentedTokenID
	// and, on match, replaces it with newTokenID (with a fresh TTL). When it
	// returns RotateGraceReuse the chain did not advance and currentTokenID
	// holds the stored id the caller must re-issue with; otherwise
	// currentTokenID is empty.
	Rotate(ctx context.Context, key, presentedTokenID, newTokenID string, ttl time.Duration) (result RotateResult, currentTokenID string, err error)
}

// RefreshRotationKey builds the rotation store key for a scope
// (e.g. projectID + sessionID, or "admin" + adminID).
func RefreshRotationKey(parts ...string) string {
	return "Torchwood:refresh:" + strings.Join(parts, ":")
}
