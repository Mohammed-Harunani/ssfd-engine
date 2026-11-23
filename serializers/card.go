package serializers

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"ssfd-engine/domain"
)

type BitmapEncoding string

const (
	HexBitmap    BitmapEncoding = "ascii-hex"
	BinaryBitmap BitmapEncoding = "binary"
)

type MTI string

const (
	Authorization MTI = "0100"
	Financial     MTI = "0200"
	CardReversal  MTI = "0400"
)

type CardMessage struct {
	Type           MTI
	ProcessingCode string
	Amount         domain.Money
	// DE7 carries MMDDhhmmss in UTC. Its year is supplied by the envelope.
	Transmission time.Time
	STAN         uint32
	// DE90 is original MTI, STAN, DE7, acquirer ID and forwarding ID.
	OriginalData string
}

var fieldOrder = [...]struct{ number, width int }{{3, 6}, {4, 12}, {7, 10}, {11, 6}, {49, 3}, {90, 42}}

func setBit(m []byte, f int)      { m[(f-1)/8] |= 1 << uint(7-(f-1)%8) }
func hasBit(m []byte, f int) bool { return (f-1)/8 < len(m) && m[(f-1)/8]&(1<<uint(7-(f-1)%8)) != 0 }

func EncodeCard(m CardMessage, c Catalog, encoding BitmapEncoding) ([]byte, error) {
	if m.Type != Authorization && m.Type != Financial && m.Type != CardReversal {
		return nil, fmt.Errorf("unsupported MTI %q", m.Type)
	}
	if !digits(m.ProcessingCode, 6) || m.Amount.Minor <= 0 || m.Amount.Minor > 999999999999 ||
		m.STAN == 0 || m.STAN > 999999 || m.Transmission.IsZero() || m.Transmission.Nanosecond() != 0 ||
		m.Transmission.UTC().Year() < 1 || m.Transmission.UTC().Year() > 9999 {
		return nil, fmt.Errorf("invalid card fields")
	}
	spec, err := c.lookup(m.Amount.Currency)
	if err != nil {
		return nil, err
	}
	de7 := m.Transmission.UTC().Format("0102150405")
	bitmap := make([]byte, 8)
	for _, f := range fieldOrder[:5] {
		setBit(bitmap, f.number)
	}
	if m.Type == CardReversal {
		if !digits(m.OriginalData, 42) || (m.OriginalData[:4] != string(Financial) && m.OriginalData[:4] != string(Authorization)) {
			return nil, fmt.Errorf("invalid original data element")
		}
		bitmap = append(bitmap, make([]byte, 8)...)
		setBit(bitmap, 1)
		setBit(bitmap, 90)
	} else if m.OriginalData != "" {
		return nil, fmt.Errorf("DE90 belongs to reversal MTI")
	}
	var wire bytes.Buffer
	wire.WriteString(string(m.Type))
	switch encoding {
	case HexBitmap:
		wire.WriteString(strings.ToUpper(hex.EncodeToString(bitmap)))
	case BinaryBitmap:
		wire.Write(bitmap)
	default:
		return nil, fmt.Errorf("unsupported bitmap encoding %q", encoding)
	}
	fmt.Fprintf(&wire, "%s%012d%s%06d%s%s", m.ProcessingCode, m.Amount.Minor, de7, m.STAN, spec.Numeric, m.OriginalData)
	return wire.Bytes(), nil
}

func cardBitmap(raw []byte, enc BitmapEncoding) ([]byte, error) {
	if enc == BinaryBitmap {
		return append([]byte(nil), raw...), nil
	}
	if enc == HexBitmap {
		return hex.DecodeString(string(raw))
	}
	return nil, fmt.Errorf("unsupported bitmap encoding %q", enc)
}

func DecodeCard(raw []byte, c Catalog, enc BitmapEncoding, year int) (CardMessage, error) {
	width := 8
	if enc == HexBitmap {
		width = 16
	}
	if len(raw) < 4+width || year < 1 || year > 9999 {
		return CardMessage{}, fmt.Errorf("invalid MTI/bitmap header or year")
	}
	m := CardMessage{Type: MTI(raw[:4])}
	mask, err := cardBitmap(raw[4:4+width], enc)
	if err != nil {
		return m, err
	}
	pos := 4 + width
	if hasBit(mask, 1) {
		if len(raw) < pos+width {
			return m, fmt.Errorf("missing secondary bitmap")
		}
		secondary, err := cardBitmap(raw[pos:pos+width], enc)
		if err != nil {
			return m, err
		}
		mask = append(mask, secondary...)
		pos += width
	}
	fields := make(map[int]string)
	for f := 2; f <= len(mask)*8; f++ {
		if !hasBit(mask, f) {
			continue
		}
		n := 0
		for _, d := range fieldOrder {
			if d.number == f {
				n = d.width
				break
			}
		}
		if n == 0 || pos+n > len(raw) {
			return m, fmt.Errorf("unsupported or truncated DE %d", f)
		}
		s := string(raw[pos : pos+n])
		if !digits(s, n) {
			return m, fmt.Errorf("non-numeric DE %d", f)
		}
		fields[f] = s
		pos += n
	}
	if pos != len(raw) {
		return m, fmt.Errorf("unexpected card trailing bytes")
	}
	m.ProcessingCode = fields[3]
	minor, err := strconv.ParseInt(fields[4], 10, 64)
	if err != nil {
		return m, err
	}
	var code domain.Currency
	for name, v := range c {
		if v.Numeric == fields[49] {
			if code != "" {
				return m, fmt.Errorf("ambiguous numeric currency")
			}
			code = name
		}
	}
	m.Amount = domain.Money{Currency: code, Minor: minor}
	m.Transmission, err = time.Parse("20060102150405", fmt.Sprintf("%04d%s", year, fields[7]))
	if err != nil {
		return m, err
	}
	stan, err := strconv.ParseUint(fields[11], 10, 32)
	if err != nil {
		return m, err
	}
	m.STAN = uint32(stan)
	m.OriginalData = fields[90]
	expected, err := EncodeCard(m, c, enc)
	if err != nil {
		return m, err
	}
	if !bytes.Equal(expected, raw) {
		return m, fmt.Errorf("message differs from canonical card encoding")
	}
	return m, nil
}

// WriteCardFrame writes an SSFD two-byte big-endian length plus one message.
func WriteCardFrame(dst *bytes.Buffer, message []byte) error {
	if len(message) == 0 || len(message) > 65535 {
		return fmt.Errorf("invalid framed card message length")
	}
	dst.Write(binary.BigEndian.AppendUint16(nil, uint16(len(message))))
	dst.Write(message)
	return nil
}

func ReadCardFrame(raw []byte) (message, rest []byte, err error) {
	if len(raw) < 2 {
		return nil, nil, fmt.Errorf("truncated frame header")
	}
	n := int(binary.BigEndian.Uint16(raw))
	if n == 0 || len(raw) < n+2 {
		return nil, nil, fmt.Errorf("truncated or empty card frame")
	}
	return raw[2 : 2+n], raw[2+n:], nil
}
