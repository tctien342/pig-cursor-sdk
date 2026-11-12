package cursor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	sdkpb "github.com/tctien342/pig-cursor-sdk/gen/sdk/v1"
)

// ErrorCode is a stable failure class, mapped from the bridge's
// sdk.v1.SdkErrorCode plus the Connect code when no detail is attached.
type ErrorCode string

const (
	CodeUnknown         ErrorCode = "unknown"
	CodeAuth            ErrorCode = "auth"
	CodePlanRequired    ErrorCode = "plan_required"
	CodeNotFound        ErrorCode = "not_found"
	CodeRateLimit       ErrorCode = "rate_limit"
	CodeUsageLimit      ErrorCode = "usage_limit"
	CodeBusy            ErrorCode = "busy"
	CodeValidation      ErrorCode = "validation"
	CodeConflict        ErrorCode = "conflict"
	CodeUnavailable     ErrorCode = "unavailable"
	CodeUpstream        ErrorCode = "upstream"
	CodeTimeout         ErrorCode = "timeout"
	CodeCancelled       ErrorCode = "cancelled"
	CodeBridgeDied      ErrorCode = "bridge_died"
	CodeContextOverflow ErrorCode = "context_overflow"
)

// Error is the one error type callers branch on. Message is safe to show a
// user; RequestID and RetryAfter are for logs and backoff.
type Error struct {
	Code       ErrorCode
	Message    string
	Op         string
	SDKCode    string
	RequestID  string
	HelpURL    string
	Provider   string
	RetryAfter string
	Rate       *RateLimit // present when Cursor described the window it refused on
	Cause      error
}

func (e *Error) Error() string {
	if e.Op != "" {
		return fmt.Sprintf("cursor %s: %s (%s)", e.Op, e.Message, e.Code)
	}
	return fmt.Sprintf("cursor: %s (%s)", e.Message, e.Code)
}

func (e *Error) Unwrap() error { return e.Cause }

// CodeOf reports the failure class of any error, CodeUnknown for foreign ones.
func CodeOf(err error) ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	switch {
	case errors.Is(err, context.Canceled):
		return CodeCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return CodeTimeout
	}
	return CodeUnknown
}

// Retryable reports whether re-sending the same request is reasonable. Tool
// execution is never retried automatically: it may already have side effects.
func Retryable(err error) bool {
	switch CodeOf(err) {
	case CodeRateLimit, CodeBusy, CodeUnavailable, CodeUpstream, CodeTimeout:
		return true
	}
	return false
}

// Wrap maps a transport failure onto the taxonomy.
func Wrap(op string, err error) error { return wrapErr(op, err) }

func wrapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		e := &Error{Op: op, Message: cerr.Message(), Cause: err, SDKCode: cerr.Code().String()}
		for _, d := range cerr.Details() {
			val, derr := d.Value()
			if derr != nil {
				continue
			}
			det, ok := val.(*sdkpb.SdkErrorDetails)
			if !ok {
				continue
			}
			e.SDKCode = det.GetSdkErrorCode().String()
			if det.GetMessage() != "" {
				e.Message = det.GetMessage()
			}
			e.RequestID = det.GetRequestId()
			e.HelpURL = det.GetHelpUrl()
			e.Provider = det.GetProvider()
			if ra := det.GetRetryAfter(); ra != nil {
				e.RetryAfter = ra.AsDuration().String()
			}
			if rl := det.GetRateLimit(); rl != nil {
				r := &RateLimit{}
				if v := rl.GetLimit(); v != 0 {
					r.Limit, r.HasLimit = int64(v), true
				}
				if v := rl.GetRemaining(); v != 0 {
					r.Remaining, r.HasRemaining = int64(v), true
				}
				if v := rl.GetResetEpochSeconds(); v != 0 {
					r.ResetAt, r.HasReset = time.Unix(int64(v), 0), true
				}
				e.Rate = r
			}
		}
		e.Code = classify(det(e.SDKCode), cerr.Code(), cerr.Message())
		return e
	}
	if code := CodeOf(err); code != CodeUnknown {
		return &Error{Op: op, Code: code, Message: err.Error(), Cause: err}
	}
	msg := err.Error()
	e := &Error{Op: op, Message: msg, Cause: err}
	switch {
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "EOF"),
		strings.Contains(msg, "connection reset"), strings.Contains(msg, "no such host"):
		e.Code = CodeBridgeDied
		e.Message = "the Cursor bridge process stopped responding"
	default:
		e.Code = classify("", connect.CodeUnknown, msg)
	}
	return e
}

func det(s string) string { return s }

func classify(sdkCode string, code connect.Code, msg string) ErrorCode {
	switch sdkCode {
	case "SDK_ERROR_CODE_UNAUTHORIZED", "SDK_ERROR_CODE_API_KEY_NOT_FOUND", "SDK_ERROR_CODE_ROLE_FORBIDDEN":
		return CodeAuth
	case "SDK_ERROR_CODE_PLAN_REQUIRED":
		return CodePlanRequired
	case "SDK_ERROR_CODE_AGENT_NOT_FOUND", "SDK_ERROR_CODE_RUN_NOT_FOUND":
		return CodeNotFound
	case "SDK_ERROR_CODE_VALIDATION_ERROR", "SDK_ERROR_CODE_INVALID_MODEL",
		"SDK_ERROR_CODE_INVALID_BRANCH_NAME", "SDK_ERROR_CODE_REPOSITORY_REQUIRED",
		"SDK_ERROR_CODE_REPOSITORY_ACCESS", "SDK_ERROR_CODE_PR_RESOLUTION_FAILED":
		return CodeValidation
	case "SDK_ERROR_CODE_USAGE_LIMIT_EXCEEDED":
		// A plan allotment, not a burst: worth telling apart because the fix is
		// waiting for the window to reset, not backing off.
		return CodeUsageLimit
	case "SDK_ERROR_CODE_RATE_LIMIT_EXCEEDED":
		return CodeRateLimit
	case "SDK_ERROR_CODE_AGENT_BUSY", "SDK_ERROR_CODE_RUN_NOT_CANCELLABLE", "SDK_ERROR_CODE_AGENT_ARCHIVED":
		return CodeBusy
	case "SDK_ERROR_CODE_FEATURE_UNAVAILABLE":
		return CodeUnavailable
	case "SDK_ERROR_CODE_UPSTREAM_ERROR":
		return CodeUpstream
	case "SDK_ERROR_CODE_INTERNAL_ERROR":
		return CodeUnavailable
	case "SDK_ERROR_CODE_CLIENT_CANCELLED":
		return CodeCancelled
	}
	switch code {
	case connect.CodeUnauthenticated, connect.CodePermissionDenied:
		return CodeAuth
	case connect.CodeNotFound:
		return CodeNotFound
	case connect.CodeResourceExhausted:
		return CodeRateLimit
	case connect.CodeInvalidArgument:
		return CodeValidation
	case connect.CodeAlreadyExists, connect.CodeAborted, connect.CodeFailedPrecondition:
		return CodeConflict
	case connect.CodeUnavailable:
		return CodeUnavailable
	case connect.CodeDeadlineExceeded:
		return CodeTimeout
	case connect.CodeCanceled:
		return CodeCancelled
	}
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "api key"):
		return CodeAuth
	case strings.Contains(lower, "context length"), strings.Contains(lower, "context window"),
		strings.Contains(lower, "context overflow"):
		return CodeContextOverflow
	case strings.Contains(lower, "plan"), strings.Contains(lower, "upgrade"):
		return CodePlanRequired
	}
	return CodeUnknown
}

// RateLimit is the usage window Cursor reports when it refuses a call: what the
// window allows, what is left, and when it resets.
//
// This is the only limit data sdk.v1 exposes, and it arrives only on a refusal —
// there is no proactive "how much of my plan have I used" RPC. Callers show it
// where it is useful (the error message and the footer) and say nothing otherwise,
// rather than implying silence means headroom.
type RateLimit struct {
	Limit        int64
	Remaining    int64
	ResetAt      time.Time
	HasLimit     bool
	HasRemaining bool
	HasReset     bool
}

// Summary renders the window as one line: "0 of 500 left · resets 21:05". Missing
// fields are left out instead of guessed.
func (r *RateLimit) Summary(now time.Time) string {
	if r == nil {
		// A bare window report still reads better than a raw error code.
		return ""
	}
	parts := []string{}
	switch {
	case r.HasLimit && r.HasRemaining:
		parts = append(parts, fmt.Sprintf("%d of %d left", r.Remaining, r.Limit))
	case r.HasRemaining:
		parts = append(parts, fmt.Sprintf("%d left", r.Remaining))
	case r.HasLimit:
		parts = append(parts, fmt.Sprintf("limit %d", r.Limit))
	}
	if r.HasReset {
		until := r.ResetAt.Sub(now)
		if until < 0 {
			until = 0
		}
		parts = append(parts, "resets "+humanDuration(until)+" ("+r.ResetAt.Local().Format("15:04")+")")
	}
	return strings.Join(parts, " · ")
}

func humanDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("in %dd", int(d.Hours())/24)
	case d >= time.Hour:
		return fmt.Sprintf("in %dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("in %dm", int(d.Minutes()))
	default:
		return "now"
	}
}

// RateOf returns the usage window carried by an error, or nil.
func RateOf(err error) *RateLimit {
	var e *Error
	if errors.As(err, &e) {
		return e.Rate
	}
	return nil
}
