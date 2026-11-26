package server

import (
	"errors"
	"strings"

	sdk "github.com/github/copilot-sdk/go"
	harnessv1 "github.com/lonegunmanb/sofa/pkg/harnessplugin/v1"
)

func classifyError(err error) *harnessv1.ErrorDetail {
	if err == nil {
		return nil
	}

	var statusCode int32
	var statusProvider interface{ HTTPStatusCode() int }
	if errors.As(err, &statusProvider) {
		if code := statusProvider.HTTPStatusCode(); code > 0 {
			statusCode = int32(code)
		}
	}

	msg := err.Error()
	lower := strings.ToLower(msg)

	if strings.Contains(lower, "session not found") {
		if statusCode == 0 {
			statusCode = 404
		}
		return &harnessv1.ErrorDetail{
			Category:       harnessv1.ErrorCategory_ERROR_CATEGORY_SESSION_NOT_FOUND,
			HttpStatusCode: statusCode,
			Message:        msg,
		}
	}

	var modelErr *modelCallError
	if errors.As(err, &modelErr) {
		cat := categoryFromSessionErrorType(modelErr.data.ErrorType, statusCode, lower)
		if statusCode == 0 {
			statusCode = defaultStatusForCategory(cat)
		}
		return &harnessv1.ErrorDetail{
			Category:       cat,
			HttpStatusCode: statusCode,
			Message:        msg,
		}
	}

	var rpcErr *sdk.RPCError
	if errors.As(err, &rpcErr) && strings.Contains(strings.ToLower(rpcErr.Message), "session not found") {
		return &harnessv1.ErrorDetail{
			Category:       harnessv1.ErrorCategory_ERROR_CATEGORY_SESSION_NOT_FOUND,
			HttpStatusCode: 404,
			Message:        msg,
		}
	}

	cat := categoryFromStatusAndMessage(statusCode, lower)
	if statusCode == 0 {
		statusCode = defaultStatusForCategory(cat)
	}
	return &harnessv1.ErrorDetail{
		Category:       cat,
		HttpStatusCode: statusCode,
		Message:        msg,
	}
}

func classifySessionErrorData(data *sdk.SessionErrorData) *harnessv1.ErrorDetail {
	if data == nil {
		return nil
	}
	var statusCode int32
	if data.StatusCode != nil && *data.StatusCode > 0 {
		statusCode = *data.StatusCode
	}
	cat := categoryFromSessionErrorType(data.ErrorType, statusCode, strings.ToLower(data.Message))
	if statusCode == 0 {
		statusCode = defaultStatusForCategory(cat)
	}
	return &harnessv1.ErrorDetail{
		Category:       cat,
		HttpStatusCode: statusCode,
		Message:        data.Message,
	}
}

func categoryFromSessionErrorType(errType string, statusCode int32, lowerMsg string) harnessv1.ErrorCategory {
	switch strings.ToLower(strings.TrimSpace(errType)) {
	case "rate_limit":
		return harnessv1.ErrorCategory_ERROR_CATEGORY_RATE_LIMIT
	case "authentication":
		return harnessv1.ErrorCategory_ERROR_CATEGORY_AUTHENTICATION
	case "authorization":
		return harnessv1.ErrorCategory_ERROR_CATEGORY_AUTHORIZATION
	case "quota":
		return harnessv1.ErrorCategory_ERROR_CATEGORY_QUOTA
	case "context_limit":
		return harnessv1.ErrorCategory_ERROR_CATEGORY_CONTEXT_LIMIT
	case "unsupported_capability":
		return harnessv1.ErrorCategory_ERROR_CATEGORY_UNSUPPORTED_CAPABILITY
	default:
		return categoryFromStatusAndMessage(statusCode, lowerMsg)
	}
}

func categoryFromStatusAndMessage(statusCode int32, lowerMsg string) harnessv1.ErrorCategory {
	switch {
	case statusCode == 429:
		return harnessv1.ErrorCategory_ERROR_CATEGORY_RATE_LIMIT
	case statusCode == 401:
		return harnessv1.ErrorCategory_ERROR_CATEGORY_AUTHENTICATION
	case statusCode == 403:
		return harnessv1.ErrorCategory_ERROR_CATEGORY_AUTHORIZATION
	case statusCode == 408 || statusCode >= 500:
		return harnessv1.ErrorCategory_ERROR_CATEGORY_TRANSIENT
	}

	switch {
	case strings.Contains(lowerMsg, "session not found"):
		return harnessv1.ErrorCategory_ERROR_CATEGORY_SESSION_NOT_FOUND
	case strings.Contains(lowerMsg, "rate_limit") ||
		strings.Contains(lowerMsg, "rate limit") ||
		strings.Contains(lowerMsg, "too many requests") ||
		strings.Contains(lowerMsg, "http 429") ||
		strings.Contains(lowerMsg, "status 429"):
		return harnessv1.ErrorCategory_ERROR_CATEGORY_RATE_LIMIT
	case strings.Contains(lowerMsg, "context_limit") ||
		strings.Contains(lowerMsg, "context length") ||
		strings.Contains(lowerMsg, "context window") ||
		strings.Contains(lowerMsg, "maximum context"):
		return harnessv1.ErrorCategory_ERROR_CATEGORY_CONTEXT_LIMIT
	case strings.Contains(lowerMsg, "quota") ||
		strings.Contains(lowerMsg, "insufficient_quota"):
		return harnessv1.ErrorCategory_ERROR_CATEGORY_QUOTA
	case strings.Contains(lowerMsg, "authentication") ||
		strings.Contains(lowerMsg, "unauthorized") ||
		strings.Contains(lowerMsg, "invalid api key") ||
		strings.Contains(lowerMsg, "401"):
		return harnessv1.ErrorCategory_ERROR_CATEGORY_AUTHENTICATION
	case strings.Contains(lowerMsg, "authorization") ||
		strings.Contains(lowerMsg, "forbidden") ||
		strings.Contains(lowerMsg, "403"):
		return harnessv1.ErrorCategory_ERROR_CATEGORY_AUTHORIZATION
	case strings.Contains(lowerMsg, "unsupported"):
		return harnessv1.ErrorCategory_ERROR_CATEGORY_UNSUPPORTED_CAPABILITY
	case isTransientMessage(lowerMsg):
		return harnessv1.ErrorCategory_ERROR_CATEGORY_TRANSIENT
	default:
		return harnessv1.ErrorCategory_ERROR_CATEGORY_INTERNAL
	}
}

func isTransientMessage(lowerMsg string) bool {
	for _, needle := range []string{
		"connection reset",
		"connection refused",
		"broken pipe",
		"unexpected eof",
		"timeout",
		"timed out",
		"temporarily unavailable",
		"service unavailable",
		"bad gateway",
		"gateway timeout",
		"deadline exceeded",
	} {
		if strings.Contains(lowerMsg, needle) {
			return true
		}
	}
	return false
}

func defaultStatusForCategory(cat harnessv1.ErrorCategory) int32 {
	switch cat {
	case harnessv1.ErrorCategory_ERROR_CATEGORY_RATE_LIMIT:
		return 429
	case harnessv1.ErrorCategory_ERROR_CATEGORY_AUTHENTICATION:
		return 401
	case harnessv1.ErrorCategory_ERROR_CATEGORY_AUTHORIZATION:
		return 403
	case harnessv1.ErrorCategory_ERROR_CATEGORY_SESSION_NOT_FOUND:
		return 404
	case harnessv1.ErrorCategory_ERROR_CATEGORY_UNSUPPORTED_CAPABILITY:
		return 400
	default:
		return 0
	}
}
