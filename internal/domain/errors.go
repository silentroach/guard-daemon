package domain

import "fmt"

type ErrorClass uint8

const (
	ErrorConfiguration ErrorClass = iota + 1
	ErrorRPCTransient
	ErrorRPCInvalidResponse
	ErrorSigning
	ErrorBroadcast
	ErrorPostcondition
	ErrorInternal
)

type ErrorCode string

type ClassifiedError struct {
	Operation string
	Class     ErrorClass
	Code      ErrorCode
	Retryable bool
	Ambiguous bool
	cause     error
}

func NewError(operation string, class ErrorClass, code ErrorCode, retryable, ambiguous bool, cause error) *ClassifiedError {
	return &ClassifiedError{
		Operation: operation,
		Class:     class,
		Code:      code,
		Retryable: retryable,
		Ambiguous: ambiguous,
		cause:     cause,
	}
}

func (e *ClassifiedError) Error() string {
	return fmt.Sprintf("%s: %s", e.Operation, e.Code)
}

func (e *ClassifiedError) Unwrap() error {
	return e.cause
}
