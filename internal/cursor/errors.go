package cursor

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
	case "SDK_ERROR_CODE_USAGE_LIMIT_EXCEEDED", "SDK_ERROR_CODE_RATE_LIMIT_EXCEEDED":
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
