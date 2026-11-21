package domain

import (
	"errors"
	"fmt"
)

// Protocol error codes exposed on the wire. The strings must stay identical to
// the documented PrintBridgeErrorCode union; business systems already branch
// on these values, so renames are a protocol break.
const (
	CodeOriginNotAllowed      = "ORIGIN_NOT_ALLOWED"
	CodeInvalidMessage        = "INVALID_MESSAGE"
	CodePrinterNotConfigured  = "PRINTER_NOT_CONFIGURED"
	CodePrinterNotFound       = "PRINTER_NOT_FOUND"
	CodePaperNotConfigured    = "PAPER_NOT_CONFIGURED"
	CodePaperNotFound         = "PAPER_NOT_FOUND"
	CodeDownloadFailed        = "DOWNLOAD_FAILED"
	CodeFileTooLarge          = "FILE_TOO_LARGE"
	CodeUnsupportedFormat     = "UNSUPPORTED_FORMAT"
	CodeFormatMismatch        = "FORMAT_MISMATCH"
	CodeOfficeConvertFailed   = "OFFICE_CONVERT_FAILED"
	CodePrintFailed           = "PRINT_FAILED"
	CodeJobDuplicated         = "JOB_DUPLICATED"
	CodeBatchDuplicated       = "BATCH_DUPLICATED"
	CodeBatchTooLarge         = "BATCH_TOO_LARGE"
	CodeCopiesOutOfRange      = "COPIES_OUT_OF_RANGE"
	CodeServicePortInUse      = "SERVICE_PORT_IN_USE"
	CodeInternalError         = "INTERNAL_ERROR"
	CodeRendererUnavailable   = "RENDERER_UNAVAILABLE"
	CodeCancelled             = "CANCELLED"
	CodeQueueFull             = "QUEUE_FULL"
	CodeJobNotFound           = "JOB_NOT_FOUND"
	CodeJobNotCancellable     = "JOB_NOT_CANCELLABLE"
	CodeProtocolVersion       = "PROTOCOL_VERSION_MISMATCH"
	CodeUnsupportedCommand    = "UNSUPPORTED_COMMAND"
	CodeReadOnly              = "READ_ONLY"
	CodePermission            = "PERMISSION_DENIED"
	CodeConfigInvalid         = "CONFIG_INVALID"
	CodeConflict              = "CONFLICT"
	CodeAgentNotRunning       = "AGENT_NOT_RUNNING"
	CodeDoctorFailed          = "DOCTOR_FAILED"
	CodeArgumentInvalid       = "ARGUMENT_INVALID"
	CodeNotImplementedYet     = "NOT_IMPLEMENTED"
	CodeRemotePaused          = "REMOTE_PAUSED"
	CodeRemoteUnavailable     = "REMOTE_UNAVAILABLE"
	CodePrinterBackendFailure = "PRINTER_BACKEND_FAILURE"
)

// ProtocolError is a failure that carries a stable, client-visible error code.
// Every layer may wrap it with more context; CodeOf walks the chain so the
// original classification survives wrapping.
type ProtocolError struct {
	Code    string
	Message string
	Err     error
}

func NewProtocolError(code, message string) *ProtocolError {
	return &ProtocolError{Code: code, Message: message}
}

func NewProtocolErrorf(code, format string, args ...any) *ProtocolError {
	return &ProtocolError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithCause attaches the underlying error without changing the visible message.
func (e *ProtocolError) WithCause(err error) *ProtocolError {
	if e == nil {
		return nil
	}
	clone := *e
	clone.Err = err
	return &clone
}

// WithMessage replaces the visible message while keeping code and cause.
func (e *ProtocolError) WithMessage(message string) *ProtocolError {
	if e == nil {
		return nil
	}
	clone := *e
	clone.Message = message
	return &clone
}

func (e *ProtocolError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Code
}

func (e *ProtocolError) Unwrap() error { return e.Err }

// HasCode reports whether the error chain carries exactly this code.
func HasCode(err error, code string) bool {
	for current := err; current != nil; current = errors.Unwrap(current) {
		var pe *ProtocolError
		if errors.As(current, &pe) && pe.Code == code {
			return true
		}
	}
	return false
}

// CodeOf returns the first protocol code found in the chain, or INTERNAL_ERROR
// when the error carries no classification.
func CodeOf(err error) string {
	if err == nil {
		return ""
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		var pe *ProtocolError
		if errors.As(current, &pe) {
			return pe.Code
		}
	}
	return CodeInternalError
}

// MessageOf returns the human readable message for an error, preferring the
// outermost protocol error's message.
func MessageOf(err error) string {
	if err == nil {
		return ""
	}
	var pe *ProtocolError
	if errors.As(err, &pe) {
		return pe.Error()
	}
	return err.Error()
}
