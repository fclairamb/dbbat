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
	OracleTypeVARCHAR2  uint8 = 1
	OracleTypeNUMBER    uint8 = 2
	OracleTypeDATE      uint8 = 12
	OracleTypeRAW       uint8 = 23
	OracleTypeCHAR      uint8 = 96
	OracleTypeBINFLOAT  uint8 = 100
	OracleTypeBINDOUBLE uint8 = 101
	OracleTypeCLOB      uint8 = 112
	OracleTypeBLOB      uint8 = 113
	OracleTypeTIMESTAMP uint8 = 180
	OracleTypeTIMESTAMP_TZ   uint8 = 181
	OracleTypeTIMESTAMP_LTZ  uint8 = 231
)

// Type decoding errors.
var (
	ErrInvalidDateLength      = errors.New("Oracle DATE requires exactly 7 bytes")
	ErrInvalidTimestampLength = errors.New("Oracle TIMESTAMP requires at least 11 bytes")
	ErrInvalidNumberData      = errors.New("Oracle NUMBER data is empty")
)

// decodeOracleValue dispatches decoding based on the Oracle type code.
// Returns nil for nil/empty data (NULL values).
func decodeOracleValue(typeCode uint8, data []byte) (interface{}, error) {
	if data == nil {
		return nil, nil
	}

	if len(data) == 0 {
		return nil, nil
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
			return nil, fmt.Errorf("BINARY_FLOAT requires 4 bytes, got %d", len(data))
		}
		return math.Float32frombits(binary.BigEndian.Uint32(data)), nil
	case OracleTypeBINDOUBLE:
		if len(data) < 8 {
			return nil, fmt.Errorf("BINARY_DOUBLE requires 8 bytes, got %d", len(data))
		}
		return math.Float64frombits(binary.BigEndian.Uint64(data)), nil
	case OracleTypeTIMESTAMP, OracleTypeTIMESTAMP_TZ, OracleTypeTIMESTAMP_LTZ:
		t, err := decodeOracleTimestamp(data)
		if err != nil {
			return nil, err
		}
		return t.Format("2006-01-02T15:04:05.000000000"), nil
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

	// Zero
	if len(data) == 1 && data[0] == 0x80 {
		return "0", nil
	}

	isPositive := data[0] > 0x80

	var exponent int
	var mantissa []int

	if isPositive {
		exponent = int(data[0]) - 0xC1
		for i := 1; i < len(data); i++ {
			mantissa = append(mantissa, int(data[i])-1)
		}
	} else {
		exponent = 0x3E - int(data[0])
		for i := 1; i < len(data); i++ {
			if data[i] == 102 { // terminator for negative numbers
				break
			}
			mantissa = append(mantissa, 101-int(data[i]))
		}
	}

	if len(mantissa) == 0 {
		return "0", nil
	}

	// Build the number string from base-100 digits
	// Each mantissa digit represents two decimal digits (0-99)
	var sb strings.Builder

	if !isPositive {
		sb.WriteByte('-')
	}

	// The exponent tells us where the decimal point goes.
	// exponent=0 means the first pair is in the units place (0-99).
	// exponent=1 means the first pair is in the hundreds place, etc.
	// We need (exponent+1) pairs before the decimal point.

	pairsBeforeDecimal := exponent + 1

	for i, digit := range mantissa {
		if i == pairsBeforeDecimal {
			// Remove trailing zeros from integer part if no fractional part follows
			// Actually, add decimal point
			sb.WriteByte('.')
		}

		if i == 0 {
			// First pair: no leading zero
			if digit >= 10 {
				sb.WriteByte(byte('0' + digit/10))
				sb.WriteByte(byte('0' + digit%10))
			} else {
				sb.WriteByte(byte('0' + digit))
			}
		} else {
			// Subsequent pairs: always two digits
			sb.WriteByte(byte('0' + digit/10))
			sb.WriteByte(byte('0' + digit%10))
		}
	}

	// If we have fewer pairs than needed before the decimal, pad with "00"
	for i := len(mantissa); i < pairsBeforeDecimal; i++ {
		sb.WriteString("00")
	}

	result := sb.String()

	// Clean up trailing zeros after decimal point
	if strings.Contains(result, ".") {
		result = strings.TrimRight(result, "0")
		result = strings.TrimRight(result, ".")
	}

	// Handle edge case: negative zero
	if result == "-0" {
		return "0", nil
	}

	return result, nil
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

	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), nsec, time.UTC), nil
}
