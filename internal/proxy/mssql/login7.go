package mssql

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf16"
)

// TDS protocol versions as they appear in LOGIN7 (MS-TDS 2.2.6.4). The value is
// stored little-endian, so 7.4 goes on the wire as 04 00 00 74.
const (
	tdsVersion71  uint32 = 0x71000001
	tdsVersion72  uint32 = 0x72090002
	tdsVersion73A uint32 = 0x730A0003
	tdsVersion73B uint32 = 0x730B0003
	// tdsVersion74 is the version dbbat targets (SQL Server 2012+).
	tdsVersion74 uint32 = 0x74000004
)

// tdsVersionName renders a TDS version for logs and error text.
func tdsVersionName(v uint32) string {
	switch v {
	case tdsVersion71:
		return "7.1"
	case tdsVersion72:
		return "7.2"
	case tdsVersion73A:
		return "7.3A"
	case tdsVersion73B:
		return "7.3B"
	case tdsVersion74:
		return "7.4"
	default:
		return fmt.Sprintf("unknown(%#08x)", v)
	}
}

// login7FixedLen is the size of the LOGIN7 fixed header: the 36-byte scalar
// block, the nine offset/length pairs, the 6-byte client id, and the SSPI /
// AtchDBFile / ChangePassword trailer. Every offset in the packet is relative
// to the start of the payload, so this is also the lowest legal blob offset.
const login7FixedLen = 94

// login7MaxLen bounds a LOGIN7 payload. Real ones are a few hundred bytes; the
// cap keeps a bogus length field from driving a large allocation.
const login7MaxLen = 1 << 20

// LOGIN7 parse errors.
var (
	// ErrLogin7TooShort — the payload is smaller than the fixed header.
	ErrLogin7TooShort = errors.New("mssql: LOGIN7 payload shorter than its fixed header")
	// ErrLogin7BadLength — the self-declared Length field disagrees with the
	// payload actually received.
	ErrLogin7BadLength = errors.New("mssql: LOGIN7 length field does not match the payload")
	// ErrLogin7BadOffset — an offset/length pair points outside the payload.
	ErrLogin7BadOffset = errors.New("mssql: LOGIN7 field offset outside the payload")
	// ErrLogin7Unsupported — the login asks for something dbbat does not do in
	// v1 (integrated auth, federated auth, a password change).
	ErrLogin7Unsupported = errors.New("mssql: unsupported LOGIN7 request")
	// ErrLogin7BadFeatureExt — the FEATUREEXT block does not decode. It is a
	// refusal rather than something to shrug off: that block is where a client
	// asks for federated authentication, so a block dbbat cannot read is a block
	// whose auth intent it cannot rule out.
	ErrLogin7BadFeatureExt = errors.New("mssql: LOGIN7 feature extension block does not decode")
)

// OptionFlags bits the proxy acts on (MS-TDS 2.2.6.4).
const (
	// optionFlags2IntSecurity — the client wants Windows integrated
	// authentication (NTLM/Kerberos) instead of a SQL login.
	optionFlags2IntSecurity byte = 0x80
	// optionFlags3Extension — ibExtension/cbExtension point at a DWORD which
	// is itself the offset of a FEATUREEXT block at the end of the payload.
	optionFlags3Extension byte = 0x10
)

// featureExtFedAuth is the FEATUREEXT feature id a client uses to ask for
// federated authentication — the Azure AD / Entra ID path (MS-TDS 2.2.6.4).
//
// It is the only feature in the block that carries an authentication mechanism:
// the others (session recovery, column encryption, UTF-8 support, Azure SQL
// support, data classification…) describe what the session can do once the
// login has succeeded, and dbbat relays them upstream untouched on purpose.
const featureExtFedAuth byte = 0x02

// Login7 is a parsed LOGIN7 message.
//
// Password holds the *descrambled* cleartext. It must never reach a log, at any
// level: String() omits it, and nothing in this package formats the struct with
// %v or %+v.
type Login7 struct {
	// Scalar block, in wire order. Little-endian, unlike the TDS packet header
	// that precedes it.
	TDSVersion     uint32
	PacketSize     uint32
	ClientProgVer  uint32
	ClientPID      uint32
	ConnectionID   uint32
	OptionFlags1   byte
	OptionFlags2   byte
	TypeFlags      byte
	OptionFlags3   byte
	ClientTimeZone int32
	ClientLCID     uint32

	// The variable part. The lengths in the packet are *character* counts for
	// the UCS-2 fields and *byte* counts for the extension and SSPI blobs,
	// which is the single most common way to get this structure wrong.
	HostName       string
	UserName       string
	Password       string
	AppName        string
	ServerName     string
	CltIntName     string
	Language       string
	Database       string
	AtchDBFile     string
	ChangePassword string
	ClientID       [6]byte
	SSPI           []byte

	// FeatureExt is the FEATUREEXT block that sits past every blob when
	// optionFlags3Extension is set (UTF-8 support, column encryption, session
	// recovery…). dbbat relays it verbatim — it has to survive a re-serialize or
	// the replayed login would lose features the client asked for — and only
	// reads it far enough to refuse the one feature that is an authentication
	// mechanism, FEDAUTH. See validateFeatureExt.
	FeatureExt []byte
}

// String renders the login for diagnostics — without the password.
func (l *Login7) String() string {
	return fmt.Sprintf(
		"LOGIN7{tds=%s user=%q database=%q host=%q app=%q lib=%q packet_size=%d}",
		tdsVersionName(l.TDSVersion), l.UserName, l.Database, l.HostName, l.AppName,
		l.CltIntName, l.PacketSize)
}

// IntegratedSecurity reports whether the client asked for Windows integrated
// authentication. dbbat v1 does SQL authentication only.
func (l *Login7) IntegratedSecurity() bool {
	return l.OptionFlags2&optionFlags2IntSecurity != 0 || len(l.SSPI) > 0
}

// FederatedAuthRequested reports whether the login's FEATUREEXT block asks for
// federated (Azure AD / Entra ID) authentication.
//
// A block that does not decode answers false here — the undecodable case is not
// silently allowed, it is refused separately by validateFeatureExt, which walks
// the block before this question is ever reached. Callers outside Validate
// should therefore treat a decode failure as its own refusal rather than trust
// this answer on a login that has not been validated.
func (l *Login7) FederatedAuthRequested() bool {
	ids, err := featureExtRequests(l.FeatureExt)
	if err != nil {
		return false
	}

	return bytes.IndexByte(ids, featureExtFedAuth) >= 0
}

// ChangePasswordRequested reports whether the login carries a password change,
// which the proxy does not relay in v1.
func (l *Login7) ChangePasswordRequested() bool {
	return l.ChangePassword != ""
}

// Validate rejects the logins dbbat cannot serve, with an error a human can act
// on.
func (l *Login7) Validate() error {
	// TDS 7.4 (SQL Server 2012+) is the floor this proxy targets. It is not
	// arbitrary: the token stream the proxy emits uses the 7.2+ wide forms —
	// a 4-byte ERROR line number and an 8-byte DONE row count — which an older
	// client would misparse rather than fail on. Zero means "server picks",
	// which is 7.4 here.
	//
	// The version constants are monotonically increasing as uint32
	// (0x71.. < 0x72.. < 0x73.. < 0x74..), so a plain comparison also accepts
	// anything newer.
	if l.TDSVersion != 0 && l.TDSVersion < tdsVersion74 {
		return fmt.Errorf("%w: TDS %s is below the minimum this proxy supports (7.4, SQL Server 2012+)",
			ErrLogin7Unsupported, tdsVersionName(l.TDSVersion))
	}

	if l.IntegratedSecurity() {
		return fmt.Errorf("%w: integrated (NTLM/Kerberos) authentication is not supported; "+
			"connect with a SQL login (User ID / Password)", ErrLogin7Unsupported)
	}

	if l.ChangePasswordRequested() {
		return fmt.Errorf("%w: changing a password through the proxy is not supported",
			ErrLogin7Unsupported)
	}

	return l.validateFeatureExt()
}

// validateFeatureExt refuses a FEATUREEXT block that asks for an authentication
// mechanism dbbat does not implement, and refuses one it cannot decode at all.
//
// The rest of the block is deliberately left alone: dbbat replays it upstream
// verbatim so the features negotiated there are the ones the client asked for.
// FEDAUTH is the exception, because dbbat can neither mint nor validate a
// federated token — relaying it would start an exchange the proxy has no part
// in, between two ends that both think it is in the middle of it.
func (l *Login7) validateFeatureExt() error {
	if len(l.FeatureExt) == 0 {
		return nil
	}

	ids, err := featureExtRequests(l.FeatureExt)
	if err != nil {
		// Fail closed. An unreadable block is exactly how a federated login
		// would slip past a walker that gave up quietly.
		return fmt.Errorf("%w: %w", ErrLogin7Unsupported, err)
	}

	if bytes.IndexByte(ids, featureExtFedAuth) >= 0 {
		return fmt.Errorf("%w: federated (Azure AD / Entra ID) authentication is not supported; "+
			"connect with a SQL login (User ID / Password)", ErrLogin7Unsupported)
	}

	return nil
}

// featureExtRelayable reports whether the FEATUREEXT block may be replayed
// upstream: it decodes, and it asks for no authentication mechanism dbbat would
// have no part in.
func (l *Login7) featureExtRelayable() bool {
	return l.validateFeatureExt() == nil
}

// featureExtRequests returns the feature ids a LOGIN7 FEATUREEXT block asks for.
//
// The block is a run of {feature id (1 byte), length (DWORD), data} entries
// closed by the 0xFF terminator — the same shape scanFeatureExtAck walks on the
// response side. Unlike that walk it returns an error instead of stopping
// quietly: a server response the proxy relays byte for byte can afford an
// unread tail, a login the proxy has to authorize cannot.
//
// Anything past the terminator is not an error. The block runs to the end of the
// payload, so a client that pads it is well within the protocol; what matters is
// that every entry up to the terminator was read.
func featureExtRequests(block []byte) ([]byte, error) {
	ids := make([]byte, 0, 4)
	pos := 0

	for {
		if pos >= len(block) {
			return nil, fmt.Errorf("%w: no terminator after %d feature(s) in %d bytes",
				ErrLogin7BadFeatureExt, len(ids), len(block))
		}

		id := block[pos]
		pos++

		if id == featureExtTerminator {
			return ids, nil
		}

		if pos+4 > len(block) {
			return nil, fmt.Errorf("%w: feature %#02x has no length field", ErrLogin7BadFeatureExt, id)
		}

		length := int(binary.LittleEndian.Uint32(block[pos : pos+4]))
		pos += 4

		if length < 0 || pos+length > len(block) {
			return nil, fmt.Errorf("%w: feature %#02x declares %d bytes, %d left",
				ErrLogin7BadFeatureExt, id, length, len(block)-pos)
		}

		ids = append(ids, id)
		pos += length
	}
}

// ucs2ToString decodes a UCS-2LE blob.
func ucs2ToString(b []byte) string {
	if len(b) < 2 {
		return ""
	}

	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		units = append(units, binary.LittleEndian.Uint16(b[i:i+2]))
	}

	return string(utf16.Decode(units))
}

// stringToUCS2 encodes a string as UCS-2LE.
func stringToUCS2(s string) []byte {
	units := utf16.Encode([]rune(s))

	out := make([]byte, len(units)*2)
	for i, u := range units {
		binary.LittleEndian.PutUint16(out[i*2:i*2+2], u)
	}

	return out
}

// scramblePassword applies the LOGIN7 password obfuscation: swap the nibbles of
// each UCS-2LE byte, then XOR with 0xA5.
//
// It is not encryption and offers no protection — it exists only so a password
// is not literally legible in a packet capture. The transform is *not* an
// involution (applying it twice yields b^0xFF), which is why descramble is a
// separate function rather than the same one called twice.
func scramblePassword(password string) []byte {
	raw := stringToUCS2(password)

	out := make([]byte, len(raw))
	for i, b := range raw {
		out[i] = (b<<4 | b>>4) ^ 0xA5
	}

	return out
}

// descramblePassword reverses scramblePassword: XOR with 0xA5 first, then swap
// the nibbles back.
func descramblePassword(scrambled []byte) string {
	raw := make([]byte, len(scrambled))
	for i, b := range scrambled {
		x := b ^ 0xA5
		raw[i] = x<<4 | x>>4
	}

	return ucs2ToString(raw)
}

// Positions of the offset/length pairs inside the fixed header. The first nine
// sit in a contiguous table right after the scalar block; the last three sit
// past the 6-byte ClientID.
const (
	login7OffsetTable       = 36
	login7ClientIDPos       = 72
	login7SSPIPos           = 78
	login7AtchDBFilePos     = 82
	login7ChangePasswordPos = 86
)

// Index of each pair in the regular offset table, in wire order.
const (
	pairHostName = iota
	pairUserName
	pairPassword
	pairAppName
	pairServerName
	pairExtension
	pairCltIntName
	pairLanguage
	pairDatabase
)

// tablePos is the byte position of a pair in the regular offset table.
func tablePos(pairIndex int) int {
	return login7OffsetTable + pairIndex*4
}

// blobAt resolves one offset/length pair at pos. charLen selects whether the
// length field counts UCS-2 characters (doubled to get bytes) or raw bytes.
func blobAt(payload []byte, pos int, charLen bool) ([]byte, error) {
	offset := int(binary.LittleEndian.Uint16(payload[pos : pos+2]))
	length := int(binary.LittleEndian.Uint16(payload[pos+2 : pos+4]))

	if charLen {
		length *= 2
	}

	if length == 0 {
		return nil, nil
	}

	if offset < login7FixedLen || offset+length > len(payload) {
		return nil, fmt.Errorf("%w: pair at %d -> %d+%d (payload %d bytes)",
			ErrLogin7BadOffset, pos, offset, length, len(payload))
	}

	return payload[offset : offset+length], nil
}

// parseLogin7 decodes a LOGIN7 payload — everything after the TDS packet
// header, reassembled across packets.
func parseLogin7(payload []byte) (*Login7, error) {
	if len(payload) < login7FixedLen {
		return nil, fmt.Errorf("%w: %d bytes", ErrLogin7TooShort, len(payload))
	}

	if len(payload) > login7MaxLen {
		return nil, fmt.Errorf("%w: %d bytes", ErrLogin7BadLength, len(payload))
	}

	declared := binary.LittleEndian.Uint32(payload[0:4])
	if int(declared) != len(payload) {
		return nil, fmt.Errorf("%w: declared %d, got %d", ErrLogin7BadLength, declared, len(payload))
	}

	login := &Login7{
		TDSVersion:     binary.LittleEndian.Uint32(payload[4:8]),
		PacketSize:     binary.LittleEndian.Uint32(payload[8:12]),
		ClientProgVer:  binary.LittleEndian.Uint32(payload[12:16]),
		ClientPID:      binary.LittleEndian.Uint32(payload[16:20]),
		ConnectionID:   binary.LittleEndian.Uint32(payload[20:24]),
		OptionFlags1:   payload[24],
		OptionFlags2:   payload[25],
		TypeFlags:      payload[26],
		OptionFlags3:   payload[27],
		ClientTimeZone: int32(binary.LittleEndian.Uint32(payload[28:32])),
		ClientLCID:     binary.LittleEndian.Uint32(payload[32:36]),
	}

	copy(login.ClientID[:], payload[login7ClientIDPos:login7ClientIDPos+6])

	if err := login.decodeFields(payload); err != nil {
		return nil, err
	}

	return login, nil
}

// decodeFields resolves every offset/length pair into the parsed struct.
func (l *Login7) decodeFields(payload []byte) error {
	textFields := []struct {
		pos  int
		dest *string
	}{
		{tablePos(pairHostName), &l.HostName},
		{tablePos(pairUserName), &l.UserName},
		{tablePos(pairAppName), &l.AppName},
		{tablePos(pairServerName), &l.ServerName},
		{tablePos(pairCltIntName), &l.CltIntName},
		{tablePos(pairLanguage), &l.Language},
		{tablePos(pairDatabase), &l.Database},
		{login7AtchDBFilePos, &l.AtchDBFile},
	}

	for _, field := range textFields {
		blob, err := blobAt(payload, field.pos, true)
		if err != nil {
			return err
		}

		*field.dest = ucs2ToString(blob)
	}

	scrambledFields := []struct {
		pos  int
		dest *string
	}{
		{tablePos(pairPassword), &l.Password},
		{login7ChangePasswordPos, &l.ChangePassword},
	}

	for _, field := range scrambledFields {
		blob, err := blobAt(payload, field.pos, true)
		if err != nil {
			return err
		}

		*field.dest = descramblePassword(blob)
	}

	sspi, err := blobAt(payload, login7SSPIPos, false)
	if err != nil {
		return err
	}

	l.SSPI = sspi

	return l.decodeFeatureExt(payload)
}

// decodeFeatureExt captures the FEATUREEXT block, which is not a blob in the
// offset table: ibExtension points at a DWORD that holds the block's own
// offset, and the block runs to the end of the payload.
func (l *Login7) decodeFeatureExt(payload []byte) error {
	if l.OptionFlags3&optionFlags3Extension == 0 {
		return nil
	}

	pointer, err := blobAt(payload, tablePos(pairExtension), false)
	if err != nil {
		return err
	}

	if len(pointer) < 4 {
		return nil
	}

	offset := int(binary.LittleEndian.Uint32(pointer[0:4]))
	if offset < login7FixedLen || offset > len(payload) {
		return fmt.Errorf("%w: FEATUREEXT at %d (payload %d bytes)",
			ErrLogin7BadOffset, offset, len(payload))
	}

	l.FeatureExt = payload[offset:]

	return nil
}

// serialize rebuilds a LOGIN7 payload from the struct, ready to be framed and
// replayed upstream. Stage 2 rewrites UserName / Password / Database first; the
// round-trip is built and tested here so that rewrite has a foundation it can
// trust.
//
// The blob order is the one clients use (host, user, password, app, server,
// extension pointer, library, language, database, SSPI, attached-db,
// change-password, then the FEATUREEXT block), which makes the output
// byte-identical to a typical client's input.
func (l *Login7) serialize() []byte {
	extPointer := l.extensionPointer()

	blobs := []struct {
		pos     int
		data    []byte
		charLen bool
	}{
		{tablePos(pairHostName), stringToUCS2(l.HostName), true},
		{tablePos(pairUserName), stringToUCS2(l.UserName), true},
		{tablePos(pairPassword), scramblePassword(l.Password), true},
		{tablePos(pairAppName), stringToUCS2(l.AppName), true},
		{tablePos(pairServerName), stringToUCS2(l.ServerName), true},
		{tablePos(pairExtension), extPointer, false},
		{tablePos(pairCltIntName), stringToUCS2(l.CltIntName), true},
		{tablePos(pairLanguage), stringToUCS2(l.Language), true},
		{tablePos(pairDatabase), stringToUCS2(l.Database), true},
		{login7SSPIPos, l.SSPI, false},
		{login7AtchDBFilePos, stringToUCS2(l.AtchDBFile), true},
		{login7ChangePasswordPos, scramblePassword(l.ChangePassword), true},
	}

	total := login7FixedLen + len(l.FeatureExt)
	for _, blob := range blobs {
		total += len(blob.data)
	}

	payload := make([]byte, login7FixedLen, total)

	binary.LittleEndian.PutUint32(payload[4:8], l.TDSVersion)
	binary.LittleEndian.PutUint32(payload[8:12], l.PacketSize)
	binary.LittleEndian.PutUint32(payload[12:16], l.ClientProgVer)
	binary.LittleEndian.PutUint32(payload[16:20], l.ClientPID)
	binary.LittleEndian.PutUint32(payload[20:24], l.ConnectionID)
	payload[24] = l.OptionFlags1
	payload[25] = l.OptionFlags2
	payload[26] = l.TypeFlags
	payload[27] = l.OptionFlags3
	binary.LittleEndian.PutUint32(payload[28:32], uint32(l.ClientTimeZone))
	binary.LittleEndian.PutUint32(payload[32:36], l.ClientLCID)
	copy(payload[login7ClientIDPos:login7ClientIDPos+6], l.ClientID[:])

	offset := login7FixedLen
	extPointerOffset := 0

	for _, blob := range blobs {
		count := len(blob.data)
		if blob.charLen {
			count /= 2
		}

		// An empty field points at the end of the fixed header with length 0,
		// which is what clients emit and what SQL Server tolerates.
		binary.LittleEndian.PutUint16(payload[blob.pos:blob.pos+2], uint16(offset))
		binary.LittleEndian.PutUint16(payload[blob.pos+2:blob.pos+4], uint16(count))

		if blob.pos == tablePos(pairExtension) && len(blob.data) == 4 {
			extPointerOffset = offset
		}

		payload = append(payload, blob.data...)
		offset += len(blob.data)
	}

	if len(l.FeatureExt) > 0 && extPointerOffset > 0 {
		// The DWORD the extension pair points at holds the block's offset, so
		// it can only be filled in once every blob has been placed.
		binary.LittleEndian.PutUint32(payload[extPointerOffset:extPointerOffset+4], uint32(len(payload)))
		payload = append(payload, l.FeatureExt...)
	}

	binary.LittleEndian.PutUint32(payload[0:4], uint32(len(payload)))

	return payload
}

// extensionPointer returns the 4-byte placeholder for the FEATUREEXT offset, or
// nil when the login carries no extension block.
func (l *Login7) extensionPointer() []byte {
	if l.OptionFlags3&optionFlags3Extension == 0 || len(l.FeatureExt) == 0 {
		return nil
	}

	return make([]byte, 4)
}
