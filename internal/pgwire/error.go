package pgwire

import (
	"bytes"
	"encoding/binary"
)

// SQLSTATE codes the proxy reports on its own behalf.
const (
	CodeConnectionFailure      = "08006"
	CodeProtocolViolation      = "08P01"
	CodeConnectionRejection    = "08004"
	errorResponseType          = 'E'
	errorFieldSeverity         = 'S'
	errorFieldSeverityNonLocal = 'V'
	errorFieldCode             = 'C'
	errorFieldMessage          = 'M'
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

	b := []byte{errorResponseType}
	b = binary.BigEndian.AppendUint32(b, uint32(4+fields.Len())) //nolint:gosec // short fixed fields

	return append(b, fields.Bytes()...)
}
