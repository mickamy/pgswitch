package pgwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// SQLSTATE codes the proxy reports on its own behalf.
const (
	CodeConnectionFailure      = "08006"
	CodeProtocolViolation      = "08P01"
	CodeConnectionRejection    = "08004"
	errorFieldSeverity         = 'S'
	errorFieldSeverityNonLocal = 'V'
	errorFieldCode             = 'C'
	errorFieldMessage          = 'M'
	errorFieldDetail           = 'D'
)

// FatalError encodes an ErrorResponse with severity FATAL.
func FatalError(code, message string) []byte {
	var fields bytes.Buffer
	for _, f := range []struct {
		typ   byte
		value string
	}{
		{typ: errorFieldSeverity, value: "FATAL"},
		{typ: errorFieldSeverityNonLocal, value: "FATAL"},
		{typ: errorFieldCode, value: code},
		{typ: errorFieldMessage, value: message},
	} {
		fields.WriteByte(f.typ)
		fields.WriteString(f.value)
		fields.WriteByte(0)
	}
	fields.WriteByte(0)

	b := []byte{MsgErrorResponse}
	b = binary.BigEndian.AppendUint32(b, uint32(4+fields.Len())) //nolint:gosec // short fixed fields

	return append(b, fields.Bytes()...)
}

// ServerError is an ErrorResponse from the server.
type ServerError struct {
	Severity string
	Code     string
	Message  string
	Detail   string
}

func (e ServerError) Error() string {
	s := fmt.Sprintf("%s %s: %s", e.Severity, e.Code, e.Message)
	if e.Detail != "" {
		s += " (" + e.Detail + ")"
	}

	return s
}

// ParseError parses the body of an ErrorResponse or NoticeResponse.
func ParseError(body []byte) (ServerError, error) {
	var e ServerError
	rest := body
	for {
		if len(rest) == 0 {
			return ServerError{}, errors.New("pgwire: ErrorResponse is not terminated")
		}
		field := rest[0]
		if field == 0 {
			break
		}
		var value string
		var ok bool
		value, rest, ok = cutNullTerminated(rest[1:])
		if !ok {
			return ServerError{}, fmt.Errorf("pgwire: ErrorResponse field %q is not terminated", field)
		}
		switch field {
		case errorFieldSeverityNonLocal:
			e.Severity = value
		case errorFieldSeverity:
			if e.Severity == "" {
				e.Severity = value
			}
		case errorFieldCode:
			e.Code = value
		case errorFieldMessage:
			e.Message = value
		case errorFieldDetail:
			e.Detail = value
		}
	}

	return e, nil
}
