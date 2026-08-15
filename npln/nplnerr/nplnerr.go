// Package nplnerr builds the errors NPLN clients understand.
//
// An NPLN service does not just return a gRPC status: it attaches an
// `nn.npln.errdetails.NError` detail carrying a trace id and a fine-grained
// detail code. The client library surfaces that code, and some of them change
// the game's behaviour — "the session is full" makes it look for another room,
// while a bare Internal error shows the player a communication error.
//
// So every failure path in this server goes through a helper here. Returning a
// plain status.Error is a bug, not a shortcut.
package nplnerr

import (
	"crypto/rand"
	"encoding/hex"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	errdetails "github.com/NextendoNetwork/splatoon-3/gen/npln/errdetails"
)

// TraceID returns a fresh trace id for an error. NPLN puts one in every NError;
// it is also logged, so an operator can tie a player's error screen to a line in
// the log.
func TraceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// withDetail attaches the NError detail to a status, falling back to the bare
// status if the detail cannot be marshalled (which would only happen on a
// programming error, and losing the detail is better than losing the error).
func withDetail(code codes.Code, msg string, ne *errdetails.NError) error {
	st := status.New(code, msg)
	if withDetails, err := st.WithDetails(ne); err == nil {
		return withDetails.Err()
	}
	return st.Err()
}

// Unauthenticated is returned when the request carries no usable identity.
// reason picks the fine-grained code the client shows the player.
func Unauthenticated(msg string, reason errdetails.NError_UnauthenticatedDetailCode) error {
	return withDetail(codes.Unauthenticated, msg, &errdetails.NError{
		TraceId: TraceID(),
		DetailCodeType: &errdetails.NError_UnauthenticatedDetailCode_{
			UnauthenticatedDetailCode: reason,
		},
	})
}

// TokenInvalid is the usual authentication failure: a token we cannot verify.
func TokenInvalid(msg string) error {
	return Unauthenticated(msg, errdetails.NError_UNAUTHENTICATED_TOKEN_INVALID)
}

// TokenExpired tells the client to refresh its access token instead of showing
// an error — the difference between a seamless refresh and a kicked player.
func TokenExpired(msg string) error {
	return Unauthenticated(msg, errdetails.NError_UNAUTHENTICATED_TOKEN_EXPIRED)
}

// InvalidAccount is the fail-closed identity answer: the console presented a
// valid-looking token that belongs to no Nextendo account.
func InvalidAccount(msg string) error {
	return Unauthenticated(msg, errdetails.NError_UNAUTHENTICATED_INVALID_ACCOUNT)
}

// NotFound reports a missing resource.
func NotFound(msg string) error {
	return withDetail(codes.NotFound, msg, &errdetails.NError{
		TraceId:        TraceID(),
		DetailCodeType: &errdetails.NError_NotFoundDetailCode_{NotFoundDetailCode: errdetails.NError_NOT_FOUND_GENERIC},
	})
}

// UserNotFound reports a missing user, which the client handles differently from
// a generic NotFound (it stops retrying the lookup).
func UserNotFound(msg string) error {
	return withDetail(codes.NotFound, msg, &errdetails.NError{
		TraceId:        TraceID(),
		DetailCodeType: &errdetails.NError_NotFoundDetailCode_{NotFoundDetailCode: errdetails.NError_NOT_FOUND_USER_NOT_FOUND},
	})
}

// UserMismatch reports that the token's user is not the user named in the
// request — i.e. somebody is asking for somebody else's resource.
func UserMismatch(msg string) error {
	return withDetail(codes.NotFound, msg, &errdetails.NError{
		TraceId:        TraceID(),
		DetailCodeType: &errdetails.NError_NotFoundDetailCode_{NotFoundDetailCode: errdetails.NError_NOT_FOUND_USER_MISMATCH},
	})
}

// InvalidArgument reports a malformed request.
func InvalidArgument(msg string) error {
	return withDetail(codes.InvalidArgument, msg, &errdetails.NError{
		TraceId: TraceID(),
		DetailCodeType: &errdetails.NError_InvalidArgumentDetailCode_{
			InvalidArgumentDetailCode: errdetails.NError_INVALID_ARGUMENT_GENERIC,
		},
	})
}

// PermissionDenied reports an operation the caller may not perform.
func PermissionDenied(msg string) error {
	return withDetail(codes.PermissionDenied, msg, &errdetails.NError{
		TraceId: TraceID(),
		DetailCodeType: &errdetails.NError_PermissionDeniedDetailCode_{
			PermissionDeniedDetailCode: errdetails.NError_PERMISSION_DENIED_GENERIC,
		},
	})
}

// WrongPassword is the answer to JoinGameSession with a bad room password. The
// client shows "wrong code" instead of a communication error, so the specific
// code matters.
func WrongPassword(msg string) error {
	return withDetail(codes.PermissionDenied, msg, &errdetails.NError{
		TraceId: TraceID(),
		DetailCodeType: &errdetails.NError_PermissionDeniedDetailCode_{
			PermissionDeniedDetailCode: errdetails.NError_PERMISSION_DENIED_GAME_SESSION_WRONG_PASSWORD,
		},
	})
}

// SessionFull tells a joiner the room filled up while it was joining, which
// makes it search for another one instead of failing the whole match.
func SessionFull(msg string) error {
	return withDetail(codes.FailedPrecondition, msg, &errdetails.NError{
		TraceId: TraceID(),
		DetailCodeType: &errdetails.NError_FailedPreconditionDetailCode_{
			FailedPreconditionDetailCode: errdetails.NError_FAILED_PRECONDITION_GAME_SESSION_IS_FULL,
		},
	})
}

// FailedPrecondition reports a generic precondition failure.
func FailedPrecondition(msg string) error {
	return withDetail(codes.FailedPrecondition, msg, &errdetails.NError{
		TraceId: TraceID(),
		DetailCodeType: &errdetails.NError_FailedPreconditionDetailCode_{
			FailedPreconditionDetailCode: errdetails.NError_FAILED_PRECONDITION_GENERIC,
		},
	})
}

// SessionExpired reports a game session that was reaped (its host stopped
// syncing) — the client leaves the lobby instead of waiting forever.
func SessionExpired(msg string) error {
	return withDetail(codes.Aborted, msg, &errdetails.NError{
		TraceId: TraceID(),
		DetailCodeType: &errdetails.NError_AbortedDetailCode_{
			AbortedDetailCode: errdetails.NError_ABORTED_GAME_SESSION_EXPIRED,
		},
	})
}

// AlreadyExists reports a duplicate create.
func AlreadyExists(msg string) error {
	return withDetail(codes.AlreadyExists, msg, &errdetails.NError{
		TraceId: TraceID(),
		DetailCodeType: &errdetails.NError_AlreadyExistsDetailCode_{
			AlreadyExistsDetailCode: errdetails.NError_ALREADY_EXISTS_GENERIC,
		},
	})
}

// Internal reports a server-side failure.
func Internal(msg string) error {
	return withDetail(codes.Internal, msg, &errdetails.NError{
		TraceId:        TraceID(),
		DetailCodeType: &errdetails.NError_InternalDetailCode_{InternalDetailCode: errdetails.NError_INTERNAL_GENERIC},
	})
}

// Unimplemented is what an unknown method gets.
//
// Retail NPLN answers Unimplemented (12) when the metadata is invalid, so the
// client is used to this code; it aborts the operation cleanly rather than
// hanging. Every occurrence is logged with the full method path so bringing a
// title up is a matter of reading the log.
func Unimplemented(msg string) error {
	return withDetail(codes.Unimplemented, msg, &errdetails.NError{
		TraceId: TraceID(),
		DetailCodeType: &errdetails.NError_UnimplementedDetailCode_{
			UnimplementedDetailCode: errdetails.NError_UNIMPLEMENTED_GENERIC,
		},
	})
}

// UnderMaintenance is served while a maintenance window is configured, and is
// what makes the game show its own maintenance screen instead of an error.
func UnderMaintenance(msg string) error {
	return withDetail(codes.Unavailable, msg, &errdetails.NError{
		TraceId: TraceID(),
		DetailCodeType: &errdetails.NError_UnavailableDetailCode_{
			UnavailableDetailCode: errdetails.NError_UNAVAILABLE_UNDER_MAINTENANCE,
		},
	})
}

// Unavailable reports a dependency this server needs but cannot reach.
func Unavailable(msg string) error {
	return withDetail(codes.Unavailable, msg, &errdetails.NError{
		TraceId: TraceID(),
		DetailCodeType: &errdetails.NError_UnavailableDetailCode_{
			UnavailableDetailCode: errdetails.NError_UNAVAILABLE_GENERIC,
		},
	})
}
