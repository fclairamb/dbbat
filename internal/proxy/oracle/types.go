package oracle

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// Oracle data type codes.
const (
	OracleTypeVARCHAR2     uint8 = 1
	OracleTypeNUMBER       uint8 = 2
	OracleTypeDATE         uint8 = 12
	OracleTypeRAW          uint8 = 23
	OracleTypeCHAR         uint8 = 96
	OracleTypeBINFLOAT     uint8 = 100
	OracleTypeBINDOUBLE    uint8 = 101
	OracleTypeCLOB         uint8 = 112
	OracleTypeBLOB         uint8 = 113
	OracleTypeTIMESTAMP    uint8 = 180
	OracleTypeTIMESTAMPTZ  uint8 = 181
	OracleTypeTIMESTAMPLTZ uint8 = 231
)

// Type decoding errors.
var (
	ErrInvalidDateLength      = errors.New("oracle DATE requires exactly 7 bytes")
	ErrInvalidTimestampLength = errors.New("oracle TIMESTAMP requires at least 11 bytes")
	ErrInvalidNumberData      = errors.New("oracle NUMBER data is empty")
)

// decodeOracleValue dispatches decoding based on the Oracle type code.
// Returns nil for nil/empty data (NULL values).
func decodeOracleValue(typeCode uint8, data []byte) (interface{}, error) {
	if len(data) == 0 {
		return nil, nil //nolint:nilnil // nil/empty data means SQL NULL — returning nil,nil is intentional
	}

	switch typeCode {
	case OracleTypeVARCHAR2:
		return string(data), nil
	case OracleTypeCHAR:
		return strings.TrimRight(string(data), " "), nil
	case OracleTypeNUMBER:
		return decodeOracleNumber(data)
	case OracleTypeDATE:
		t, err := decodeOracleDate(data)
		if err != nil {
			return nil, err
		}
		return t.Format("2006-01-02T15:04:05"), nil
	case OracleTypeRAW:
		return hex.EncodeToString(data), nil
	case OracleTypeBINFLOAT:
		if len(data) < 4 {
			return nil, fmt.Errorf("%w: BINARY_FLOAT requires 4 bytes, got %d", ErrInvalidFloatLength, len(data))
		}
		return math.Float32frombits(binary.BigEndian.Uint32(data)), nil
	case OracleTypeBINDOUBLE:
		if len(data) < 8 {
			return nil, fmt.Errorf("%w: BINARY_DOUBLE requires 8 bytes, got %d", ErrInvalidFloatLength, len(data))
		}
		return math.Float64frombits(binary.BigEndian.Uint64(data)), nil
	case OracleTypeTIMESTAMP, OracleTypeTIMESTAMPLTZ:
		t, err := decodeOracleTimestamp(data)
		if err != nil {
			return nil, err
		}
		return t.Format("2006-01-02T15:04:05.000000000"), nil
	case OracleTypeTIMESTAMPTZ:
		t, err := decodeOracleTimestamp(data)
		if err != nil {
			return nil, err
		}
		return t.Format("2006-01-02T15:04:05.000000000Z07:00"), nil
	case OracleTypeCLOB, OracleTypeBLOB:
		return "[LOB]", nil
	default:
		return base64.StdEncoding.EncodeToString(data), nil
	}
}

// decodeOracleNumber decodes Oracle's internal NUMBER format to a string.
//
// Oracle NUMBER format:
//   - Single byte 0x80 = zero
//   - First byte encodes sign and exponent
//   - Remaining bytes are base-100 mantissa digits
//   - For positive: exponent = byte[0] - 0xC1, digits = byte[i] - 1
//   - For negative: exponent = 0x3E - byte[0], digits = 101 - byte[i], last byte is terminator (0x66)
func decodeOracleNumber(data []byte) (string, error) {
	if len(data) == 0 {
		return "", ErrInvalidNumberData
	}

	// The column type is known here (NUMBER), so no isOracleNumber gate is
	// needed — decode the raw bytes directly with the shared formatter.
	s, ok := formatOracleNumber(data)
	if !ok {
		return "", ErrInvalidNumberData
	}

	return s, nil
}

// decodeOracleDate decodes Oracle's 7-byte DATE format.
//
// Format:
//
//	byte 0: century (e.g., 120 = 20th century, 119 = 19th century)
//	byte 1: year within century (e.g., 124 = year 24)
//	byte 2: month (1-12)
//	byte 3: day (1-31)
//	byte 4: hour + 1 (1-24)
//	byte 5: minute + 1 (1-60)
//	byte 6: second + 1 (1-60)
func decodeOracleDate(data []byte) (time.Time, error) {
	if len(data) != 7 {
		return time.Time{}, fmt.Errorf("%w: got %d bytes", ErrInvalidDateLength, len(data))
	}

	century := int(data[0])
	yearInCentury := int(data[1])

	// Century encoding: 100 = 1 AD, 119 = 1900s, 120 = 2000s
	year := (century-100)*100 + (yearInCentury - 100)
	month := int(data[2])
	day := int(data[3])
	hour := int(data[4]) - 1
	minute := int(data[5]) - 1
	second := int(data[6]) - 1

	return time.Date(year, time.Month(month), day, hour, minute, second, 0, time.UTC), nil
}

// decodeOracleTimestamp decodes Oracle's TIMESTAMP format.
// First 7 bytes are the same as DATE, followed by 4 bytes of fractional seconds (nanoseconds, big-endian).
// For the 13-byte WITH TIME ZONE form, bytes 11-12 carry a numeric offset
// (tzHour = (b[11]&0x3f)-20, tzMin = b[12]-60) when b[11]'s high bit is clear;
// the returned time is placed in that fixed zone so the original local wall
// clock is preserved (Oracle stores the instant in UTC). Named-region zones
// (high bit set) and out-of-range offsets fall back to UTC.
func decodeOracleTimestamp(data []byte) (time.Time, error) {
	if len(data) < 11 {
		return time.Time{}, fmt.Errorf("%w: got %d bytes", ErrInvalidTimestampLength, len(data))
	}

	t, err := decodeOracleDate(data[:7])
	if err != nil {
		return time.Time{}, err
	}

	// Fractional seconds: 4 bytes big-endian nanoseconds
	nsec := int(binary.BigEndian.Uint32(data[7:11]))

	base := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), nsec, time.UTC)

	if len(data) >= 13 && data[11]&0x80 == 0 {
		offsetSec := (int(data[11]&0x3f)-20)*3600 + (int(data[12])-60)*60
		if offsetSec >= -15*3600 && offsetSec <= 15*3600 {
			zone := time.FixedZone("", offsetSec)

			// Bit 0x40 set: the prefix is already the local wall clock; clear:
			// the prefix is UTC and is shifted into the zone.
			if data[11]&0x40 != 0 {
				return time.Date(base.Year(), base.Month(), base.Day(),
					base.Hour(), base.Minute(), base.Second(), base.Nanosecond(), zone), nil
			}

			return base.In(zone), nil
		}
	}

	return base, nil
}
